package tonpayments

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"reflect"
	"time"
)

func (s *Service) updateOurStateWithAction(channel *db.Channel, action transport.Action, details any) (func(), *cell.Cell, *cell.Cell, error) {
	var onSuccess func()

	var idempotency bool
	dictRoot := cell.CreateProofSkeleton()

	switch ch := action.(type) {
	case transport.IncrementStatesAction:
	case transport.OpenVirtualAction:
		vch := details.(payments.VirtualChannel)

		if vch.Capacity.Sign() <= 0 {
			return nil, nil, nil, fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return nil, nil, nil, fmt.Errorf("invalid fee")
		}

		if vch.Deadline < time.Now().Unix() {
			return nil, nil, nil, fmt.Errorf("deadline expired")
		}

		val := vch.Serialize()

		key := big.NewInt(int64(binary.LittleEndian.Uint32(vch.Key)))
		keyCell := cell.BeginCell().MustStoreBigInt(key, 32).EndCell()

		sl, proofValueBranch, err := channel.Our.Conditionals.LoadValueWithProof(keyCell, dictRoot)
		if err == nil {
			if bytes.Equal(sl.MustToCell().Hash(), val.Hash()) {
				// idempotency
				proofValueBranch.SetRecursive()
				idempotency = true
				break
			}
			return nil, nil, nil, fmt.Errorf("virtual channel with the same key prefix and different content is already exists")
		} else if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, nil, fmt.Errorf("failed to load our condition: %w", err)
		}

		// TODO: check virtual channels limit

		if err := channel.Our.Conditionals.SetIntKey(key, val); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		_, proofValueBranch, err = channel.Our.Conditionals.LoadValueWithProof(keyCell, dictRoot)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to find key for proof branch: %w", err)
		}
		// include whole value cell in proof
		proofValueBranch.SetRecursive()

		ourTargetBalance, err := channel.CalcBalance(false)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to calc our side balance with target: %w", err)
		}

		if ourTargetBalance.Sign() == -1 {
			return nil, nil, nil, fmt.Errorf("not enough available balance with target")
		}
	case transport.RemoveVirtualAction:
		idx, vch, err := payments.FindVirtualChannelWithProof(channel.Our.Conditionals, ch.Key, dictRoot)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency, if not found we consider it already closed
				idempotency = true
				break
			}
			return nil, nil, nil, err
		}
		// new skeleton to reset prev path
		dictRoot = cell.CreateProofSkeleton()

		if err = channel.Our.Conditionals.DeleteIntKey(idx); err != nil {
			return nil, nil, nil, err
		}

		key := big.NewInt(int64(binary.LittleEndian.Uint32(vch.Key)))
		keyCell := cell.BeginCell().MustStoreBigInt(key, 32).EndCell()

		_, _, err = channel.Our.Conditionals.LoadValueWithProof(keyCell, dictRoot)
		if err == nil || !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, nil, fmt.Errorf("deleted value is still exists for some reason: %w", err)
		}

		onSuccess = func() {
			log.Info().Hex("key", vch.Key).
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("channel", channel.Address).
				Msg("virtual channel successfully removed")
		}
	case transport.ConfirmCloseAction:
		var vState payments.VirtualChannelState
		if err := tlb.LoadFromCell(&vState, ch.State.BeginParse()); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(ch.Key) {
			return nil, nil, nil, fmt.Errorf("incorrect channel state signature")
		}

		idx, vch, err := payments.FindVirtualChannelWithProof(channel.Our.Conditionals, ch.Key, dictRoot)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency, if not found we consider it already closed
				idempotency = true
				break
			}
			return nil, nil, nil, err
		}
		// new skeleton to reset prev path
		dictRoot = cell.CreateProofSkeleton()

		if vch.Deadline < time.Now().Unix() {
			return nil, nil, nil, fmt.Errorf("virtual channel has expired")
		}

		if err = channel.Our.Conditionals.DeleteIntKey(idx); err != nil {
			return nil, nil, nil, err
		}

		key := big.NewInt(int64(binary.LittleEndian.Uint32(vch.Key)))
		keyCell := cell.BeginCell().MustStoreBigInt(key, 32).EndCell()

		_, _, err = channel.Our.Conditionals.LoadValueWithProof(keyCell, dictRoot)
		if err == nil || !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, nil, fmt.Errorf("deleted value is still exists for some reason: %w", err)
		}

		sent := new(big.Int).Add(channel.Our.State.Data.Sent.Nano(), vState.Amount.Nano())
		sent = sent.Add(sent, vch.Fee)
		channel.Our.State.Data.Sent = tlb.FromNanoTON(sent)

		onSuccess = func() {
			log.Info().Hex("key", vch.Key).
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Str("amount", vState.Amount.String()).
				Str("channel", channel.Address).
				Msg("virtual channel close confirmed")
		}
	default:
		return nil, nil, nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(ch).String())
	}

	var cond *cell.Cell
	if !channel.Our.Conditionals.IsEmpty() {
		cond = channel.Our.Conditionals.AsCell()
	}

	if !idempotency {
		channel.Our.State.Data.Seqno++
		if cond != nil {
			channel.Our.State.Data.ConditionalsHash = cond.Hash()
		} else {
			channel.Our.State.Data.ConditionalsHash = make([]byte, 32)
		}
		cl, err := tlb.ToCell(channel.Our.State)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize state for signing: %w", err)
		}
		channel.Our.Signature = payments.Signature{Value: cl.Sign(s.key)}
	}

	res, err := tlb.ToCell(channel.Our.SignedSemiChannel)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize signed state: %w", err)
	}

	if cond == nil {
		// empty conditionals
		return onSuccess, res, nil, nil
	}

	proofRoot := cell.CreateProofSkeleton()
	if !channel.Our.Conditionals.IsEmpty() {
		proofRoot.AttachAt(0, dictRoot)
	}

	updateProof, err := cond.CreateProof(proofRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create proof from conditionals: %w", err)
	}

	return onSuccess, res, updateProof, nil
}
