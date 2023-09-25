package node

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/internal/node/transport"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"reflect"
	"time"
)

var ErrAlreadyApplied = errors.New("action is already applied")

func (s *Service) createRollback(channel *db.Channel, isOur bool) (func() error, error) {
	side := &channel.Our
	if !isOur {
		side = &channel.Their
	}

	bc := side.State.Data.Conditionals.MustToCell()
	backupCond := cell.NewDict(32)
	if bc != nil {
		var err error
		backupCond, err = bc.BeginParse().ToDict(32)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize cond dict: %w", err)
		}
	}

	cp := side.State.CounterpartyData
	backupSeqno := side.State.Data.Seqno
	backupSent := side.State.Data.Sent
	backupSignature := append([]byte{}, side.Signature.Value...)
	return func() error {
		side.State.CounterpartyData = cp
		side.State.Data.Conditionals = backupCond
		side.State.Data.Seqno = backupSeqno
		side.State.Data.Sent = backupSent
		side.Signature = payments.Signature{Value: backupSignature}

		if err := s.db.UpdateChannel(channel); err != nil {
			return fmt.Errorf("failed to rollback: %w", err)
		}
		return nil
	}, nil
}

func (s *Service) updateOurStateWithAction(channel *db.Channel, action transport.Action, details any) (*payments.SignedSemiChannel, func() error, error) {
	rollback, err := s.createRollback(channel, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize cond dict: %w", err)
	}

	switch ch := action.(type) {
	case transport.IncrementStatesAction:
	case transport.OpenVirtualAction:
		vch := details.(payments.VirtualChannel)

		if vch.Capacity.Sign() <= 0 {
			return nil, nil, fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return nil, nil, fmt.Errorf("invalid fee")
		}

		if vch.Deadline < time.Now().Unix() {
			return nil, nil, fmt.Errorf("deadline expired")
		}

		val := vch.Serialize()

		for _, kv := range channel.Our.State.Data.Conditionals.All() {
			if bytes.Equal(kv.Value.Hash(), val.Hash()) {
				return nil, nil, ErrAlreadyApplied
			}
		}

		var slot = -1
		// TODO: we are looking for a free slot to keep it compact, [make it better]
		for i := 0; i < s.channelsLimit; i++ {
			if channel.Our.State.Data.Conditionals.GetByIntKey(big.NewInt(int64(i))) == nil {
				slot = i
				break
			}
		}

		if slot == -1 {
			return nil, nil, fmt.Errorf("virtual channels limit has been reached")
		}

		if err := channel.Our.State.Data.Conditionals.SetIntKey(big.NewInt(int64(slot)), val); err != nil {
			return nil, nil, fmt.Errorf("failed to set condition: %w", err)
		}

		ourTargetBalance, err := channel.CalcBalance(false)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to calc our side balance with target: %w", err)
		}

		if ourTargetBalance.Sign() == -1 {
			return nil, nil, fmt.Errorf("not enough available balance with target")
		}
	case transport.ConfirmCloseAction:
		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, ch.State.BeginParse()); err != nil {
			return nil, nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(ch.Key) {
			return nil, nil, fmt.Errorf("incorrect channel state signature")
		}

		idx, vch, err := channel.Our.State.FindVirtualChannel(ch.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if vch.Deadline < time.Now().Unix() {
			return nil, nil, fmt.Errorf("virtual channel has expired")
		}

		if err = channel.Our.State.Data.Conditionals.DeleteIntKey(idx); err != nil {
			return nil, nil, err
		}

		sent := new(big.Int).Add(channel.Our.State.Data.Sent.Nano(), vState.Amount.Nano())
		sent = sent.Add(sent, vch.Fee)
		channel.Our.State.Data.Sent = tlb.FromNanoTON(sent)
	default:
		return nil, nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(ch).String())
	}

	channel.Our.State.Data.Seqno++
	cl, err := tlb.ToCell(channel.Our.State)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize state for signing: %w", err)
	}
	channel.Our.Signature = payments.Signature{Value: cl.Sign(s.key)}

	if err = s.db.UpdateChannel(channel); err != nil {
		return nil, nil, fmt.Errorf("failed to update channel in db: %w", err)
	}
	return &channel.Our.SignedSemiChannel, rollback, nil
}
