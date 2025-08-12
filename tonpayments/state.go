package tonpayments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"reflect"
	"time"
)

func (s *Service) updateOurStateWithAction(channel *db.Channel, action transport.Action, details any) (func(ctx context.Context) error, *cell.Cell, *cell.Cell, error) {
	var onSuccess func(ctx context.Context) error

	cc, err := s.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to resolve coin config: %w", err)
	}

	var idempotency bool
	dictRoot := cell.CreateProofSkeleton()

	switch act := action.(type) {
	case transport.IncrementStatesAction:
	case transport.OpenVirtualAction:
		vch := details.(payments.VirtualChannel)

		if vch.Capacity.Sign() <= 0 {
			return nil, nil, nil, fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return nil, nil, nil, fmt.Errorf("invalid fee")
		}

		if vch.Prepay.Sign() < 0 {
			return nil, nil, nil, fmt.Errorf("invalid prepay")
		}

		if vch.Deadline < time.Now().UTC().Unix() {
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
		// include a whole value cell in proof
		proofValueBranch.SetRecursive()

		/*ourTargetBalance, _, err := channel.CalcBalance(false)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to calc our side balance with target: %w", err)
		}

		if ourTargetBalance.Sign() < 0 {
			return nil, nil, nil, fmt.Errorf("not enough available balance with target")
		}*/
	case transport.CommitVirtualAction:
		_, vch, err := payments.FindVirtualChannelWithProof(channel.Our.Conditionals, act.Key, dictRoot)
		if err != nil {
			return nil, nil, nil, err
		}

		prepay := new(big.Int).SetBytes(act.PrepayAmount)
		toSend := new(big.Int).Sub(prepay, vch.Prepay)

		if toSend.Sign() < 0 {
			return nil, nil, nil, fmt.Errorf("prepay amount is less than before")
		} else if toSend.Sign() == 0 {
			// same
			idempotency = true
			break
		}

		key := big.NewInt(int64(binary.LittleEndian.Uint32(vch.Key)))

		vch.Prepay = prepay
		if err := channel.Our.Conditionals.SetIntKey(key, vch.Serialize()); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		channel.Our.State.Data.Sent = cc.MustAmount(new(big.Int).Add(channel.Our.State.Data.Sent.Nano(), toSend))

		onSuccess = func(_ context.Context) error {
			log.Info().Str("key", base64.StdEncoding.EncodeToString(vch.Key)).
				Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
				Str("fee", tlb.MustFromNano(vch.Fee, int(cc.Decimals)).String()).
				Str("prepaid", vch.Prepay.String()).
				Str("channel", channel.Address).
				Msg("virtual channel commit confirmed")
			return nil
		}
	case transport.RemoveVirtualAction:
		idx, vch, err := payments.FindVirtualChannelWithProof(channel.Our.Conditionals, act.Key, dictRoot)
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

		onSuccess = func(_ context.Context) error {
			log.Info().Str("key", base64.StdEncoding.EncodeToString(vch.Key)).
				Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
				Str("channel", channel.Address).
				Msg("virtual channel successfully removed")
			return nil
		}
	case transport.ConfirmCloseAction:
		var vState payments.VirtualChannelState
		if err := tlb.LoadFromCell(&vState, act.State.BeginParse()); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(act.Key) {
			return nil, nil, nil, fmt.Errorf("incorrect channel state signature")
		}

		idx, vch, err := payments.FindVirtualChannelWithProof(channel.Our.Conditionals, act.Key, dictRoot)
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

		if vch.Deadline < time.Now().UTC().Unix() {
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

		toSend := new(big.Int).Set(vState.Amount)
		toSend = toSend.Sub(toSend, vch.Prepay)
		toSend = toSend.Add(toSend, vch.Fee)

		if toSend.Sign() > 0 {
			// we cannot decrease sent, even when we prepaid more than actual
			channel.Our.State.Data.Sent = cc.MustAmount(new(big.Int).Add(toSend, channel.Our.State.Data.Sent.Nano()))
		}

		if channel.OurLockedDeposit != nil {
			// mark part of the rented deposit as used
			channel.OurLockedDeposit.Used.Add(channel.OurLockedDeposit.Used, toSend)
		}

		onSuccess = func(_ context.Context) error {
			log.Info().Str("key", base64.StdEncoding.EncodeToString(vch.Key)).
				Str("capacity", cc.MustAmount(vch.Capacity).String()).
				Str("fee", cc.MustAmount(vch.Fee).String()).
				Str("amount", cc.MustAmount(vState.Amount).String()).
				Str("prepaid", cc.MustAmount(vch.Prepay).String()).
				Str("channel", channel.Address).
				Msg("virtual channel close confirmed")
			return nil
		}
	case transport.RentCapacityAction:
		amount := new(big.Int).SetBytes(act.Amount)
		till := time.Unix(int64(act.Till), 0)
		totalFee := channel.CalcDepositFee(cc, amount, till, true)

		channel.Our.State.Data.Sent = cc.MustAmount(new(big.Int).Add(channel.Our.State.Data.Sent.Nano(), totalFee))

		used := big.NewInt(0)
		if channel.TheirLockedDeposit != nil && channel.TheirLockedDeposit.Till.After(time.Now()) {
			used = channel.TheirLockedDeposit.Used
			if amount.Cmp(channel.TheirLockedDeposit.Amount) <= 0 {
				return nil, nil, nil, fmt.Errorf("amount should increase only")
			}
			if till.Before(channel.TheirLockedDeposit.Till) {
				return nil, nil, nil, fmt.Errorf("new till should be greater than old one")
			}
		}

		channel.TheirLockedDeposit = &db.LockedDepositInfo{
			Amount: amount,
			Till:   till,
			Used:   used,
		}

		evData := db.ChannelHistoryActionRentCapData{
			Amount: amount.String(),
			Fee:    totalFee.String(),
		}
		jsonData, err := json.Marshal(evData)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to marshal event data: %w", err)
		}

		onSuccess = func(ctx context.Context) error {
			if err = s.db.CreateChannelEvent(ctx, channel, time.Now(), db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionTheirCapacityRented,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel our cap rent event: %w", err)
			}

			log.Info().Str("fee", cc.MustAmount(totalFee).String()).
				Str("amount", cc.MustAmount(amount).String()).
				Str("channel", channel.Address).
				Time("till", till).
				Msg("capacity rent confirmed")
			return nil
		}
	default:
		return nil, nil, nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(act).String())
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

	updateProof, err := cond.CreateProof(dictRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create proof from conditionals: %w, DUMP: %s", err, cond.Dump())
	}

	return onSuccess, res, updateProof, nil
}
