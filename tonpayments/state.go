package tonpayments

import (
	"bytes"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"reflect"
	"time"
)

func (s *Service) updateOurStateWithAction(channel *db.Channel, action transport.Action, details any) (func(), error) {
	var onSuccess func()

	switch ch := action.(type) {
	case transport.IncrementStatesAction:
	case transport.OpenVirtualAction:
		vch := details.(payments.VirtualChannel)

		if vch.Capacity.Sign() <= 0 {
			return nil, fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return nil, fmt.Errorf("invalid fee")
		}

		if vch.Deadline < time.Now().Unix() {
			return nil, fmt.Errorf("deadline expired")
		}

		val := vch.Serialize()

		for _, kv := range channel.Our.State.Data.Conditionals.All() {
			if bytes.Equal(kv.Value.Hash(), val.Hash()) {
				// idempotency
				return onSuccess, nil
			}
		}

		var slot = -1
		// TODO: we are looking for a free slot to keep it compact, [make it better]
		for i := 0; i < s.virtualChannelsLimit; i++ {
			if channel.Our.State.Data.Conditionals.GetByIntKey(big.NewInt(int64(i))) == nil {
				slot = i
				break
			}
		}

		if slot == -1 {
			return nil, fmt.Errorf("virtual channels limit has been reached")
		}

		if err := channel.Our.State.Data.Conditionals.SetIntKey(big.NewInt(int64(slot)), val); err != nil {
			return nil, fmt.Errorf("failed to set condition: %w", err)
		}

		ourTargetBalance, err := channel.CalcBalance(false)
		if err != nil {
			return nil, fmt.Errorf("failed to calc our side balance with target: %w", err)
		}

		if ourTargetBalance.Sign() == -1 {
			return nil, fmt.Errorf("not enough available balance with target")
		}
	case transport.RemoveVirtualAction:
		idx, vch, err := channel.Our.State.FindVirtualChannel(ch.Key)
		if err != nil {
			// idempotency, if not found we consider it already closed
			return onSuccess, nil
		}

		if err = channel.Our.State.Data.Conditionals.DeleteIntKey(idx); err != nil {
			return nil, err
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
			return nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(ch.Key) {
			return nil, fmt.Errorf("incorrect channel state signature")
		}

		idx, vch, err := channel.Our.State.FindVirtualChannel(ch.Key)
		if err != nil {
			// idempotency, if not found we consider it already closed
			return onSuccess, nil
		}

		if vch.Deadline < time.Now().Unix() {
			return nil, fmt.Errorf("virtual channel has expired")
		}

		if err = channel.Our.State.Data.Conditionals.DeleteIntKey(idx); err != nil {
			return nil, err
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
		return nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(ch).String())
	}

	channel.Our.State.Data.Seqno++
	cl, err := tlb.ToCell(channel.Our.State)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize state for signing: %w", err)
	}
	channel.Our.Signature = payments.Signature{Value: cl.Sign(s.key)}

	return onSuccess, nil
}
