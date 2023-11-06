package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/internal/node/transport"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"reflect"
	"time"
)

// Channel flow:
// 1. User calls ProposeAction with open virtual channel to node
// 2. Node triggers ProcessAction and then ProposeAction to receiver
// 3. Receiver triggers ProcessAction, now channel is open
//
// 1. When receiver wants to close channel, he calls RequestActions using CloseVirtualAction with channel and state
// 2. Node triggers ProcessActionRequest, validates data, and responds with ProposeAction with remove condition and coins transfer.
// 3. Receiver triggers ProcessAction and approves
// 4. Node in chain repeats this steps in background, if it is not initial sender.

func (s *Service) ProcessAction(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, signedState payments.SignedSemiChannel, action transport.Action) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	channel, err := s.getVerifiedChannel(channelAddr.String())
	if err != nil {
		return err
	}

	if channel.Status != db.ChannelStateActive {
		return fmt.Errorf("channel is not active")
	}

	if !bytes.Equal(key, channel.TheirOnchain.Key) {
		return fmt.Errorf("incorrect channel key")
	}

	if err = signedState.Verify(channel.TheirOnchain.Key); err != nil {
		return fmt.Errorf("failed to verify passed state: %w", err)
	}

	if signedState.State.Data.Sent.Nano().Cmp(channel.Their.State.Data.Sent.Nano()) == -1 {
		return fmt.Errorf("amount decrease is not allowed")
	}

	var toExecute func() error
	if signedState.State.Data.Seqno == channel.Their.State.Data.Seqno {
		// idempotency check
		channel.Their.Signature = signedState.Signature
		if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
			return fmt.Errorf("inconsistent state, this seqno with different content was already committed")
		}
		return nil
	}

	if signedState.State.Data.Seqno != channel.Their.State.Data.Seqno+1 {
		return fmt.Errorf("incorrect state seqno %d, want %d", signedState.State.Data.Seqno, channel.Their.State.Data.Seqno+1)
	}

	// we will roll back to this moment's state in case of failure
	rollback, err := s.createRollback(channel, true)
	if err != nil {
		return fmt.Errorf("failed to prepare rollback: %w", err)
	}

	log.Debug().Type("action", action).Msg("action process")

	switch data := action.(type) {
	case transport.IncrementStatesAction:
		hasStates := channel.Our.IsReady() && channel.Their.IsReady()

		toExecute = func() error {
			if !data.Response {
				if err = s.proposeAction(ctx, channel, transport.IncrementStatesAction{Response: true}, nil); err != nil {
					return fmt.Errorf("failed to propose action to party: %w", err)
				}
			}

			if !hasStates {
				log.Info().Str("address", channel.Address).
					Hex("with", channel.TheirOnchain.Key).
					Msg("onchain channel states exchanged, ready to use")
			}
			return nil
		}
	case transport.UnlockExpiredAction:
		index, _, err := signedState.State.FindVirtualChannel(data.Key)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}
		if err == nil {
			return fmt.Errorf("condition should be removed to unlock")
		}

		index, vch, err := channel.Their.State.FindVirtualChannel(data.Key)
		if err != nil {
			return fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}

		if vch.Deadline >= time.Now().Unix() {
			return fmt.Errorf("condition is not expired")
		}

		if err = channel.Their.State.Data.Conditionals.SetIntKey(index, nil); err != nil {
			return fmt.Errorf("failed to remove condition with index %s: %w", index.String(), err)
		}
	case transport.ConfirmCloseAction:
		index, _, err := signedState.State.FindVirtualChannel(data.Key)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}
		if err == nil {
			return fmt.Errorf("condition should be removed to close")
		}

		index, vch, err := channel.Their.State.FindVirtualChannel(data.Key)
		if err != nil {
			return fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}

		balanceDiff := new(big.Int).Sub(signedState.State.Data.Sent.Nano(), channel.Their.State.Data.Sent.Nano())

		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, data.State.BeginParse()); err != nil {
			return fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(vch.Key) {
			return fmt.Errorf("incorrect channel state signature")
		}

		if vState.Amount.Nano().Cmp(vch.Capacity) == 1 {
			return fmt.Errorf("amount cannot be > capacity")
		}

		gotAmt := new(big.Int).Add(vState.Amount.Nano(), vch.Fee)
		if gotAmt.Cmp(balanceDiff) == -1 {
			return fmt.Errorf("incorrect amount unlocked: %s instead of %s", balanceDiff.String(), vState.Amount.Nano().String())
		}

		if res := channel.GetKnownResolve(vch.Key); res != nil {
			if res.Amount.Nano().Cmp(vState.Amount.Nano()) == 1 {
				// TODO: maybe just use newer instead of err
				return fmt.Errorf("outdated virtual channel state")
			}
		}

		if err = channel.Their.State.Data.Conditionals.DeleteIntKey(index); err != nil {
			return fmt.Errorf("failed to remove condition with index %s: %w", index.String(), err)
		}

		toExecute = func() error {
			log.Info().Hex("key", data.Key).
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Msg("virtual channel closed")
			return nil
		}
	case transport.OpenVirtualAction:
		index, vch, err := signedState.State.FindVirtualChannel(data.Key)
		if err != nil {
			return fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}

		if vch.Capacity.Sign() <= 0 {
			return fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return fmt.Errorf("invalid fee")
		}

		if vch.Deadline < time.Now().Unix()+s.deadlineDecreaseNextHop {
			return fmt.Errorf("too short virtual channel deadline")
		}

		_, oldVC, err := channel.Their.State.FindVirtualChannel(data.Key)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}
		if err == nil {
			if oldVC.Deadline == vch.Deadline && oldVC.Fee.Cmp(vch.Fee) == 0 && oldVC.Capacity.Cmp(vch.Capacity) == 0 {
				// idempotency
				return nil
			}
			return fmt.Errorf("channel with this key is already exists and has different configuration")
		}

		// we put our serialized condition to make sure that party is not cheated,
		// if something diff will be in state, final signature will not match
		if err = channel.Their.State.Data.Conditionals.SetIntKey(index, vch.Serialize()); err != nil {
			return fmt.Errorf("failed to settle condition with index %s: %w", index.String(), err)
		}

		theirBalance, err := channel.CalcBalance(true)
		if err != nil {
			return fmt.Errorf("failed to calc other side balance: %w", err)
		}

		if theirBalance.Sign() == -1 {
			return fmt.Errorf("not enough available balance, you need %s more to do this", theirBalance.Abs(theirBalance).String())
		}

		currentInstruction, err := data.DecryptOurInstruction(s.key)
		if err != nil {
			return fmt.Errorf("failed to decrypt instruction: %w", err)
		}

		expFee := new(big.Int).SetBytes(currentInstruction.ExpectedFee)
		expCap := new(big.Int).SetBytes(currentInstruction.ExpectedCapacity)

		if expFee.Cmp(vch.Fee) != 0 || expCap.Cmp(vch.Capacity) != 0 || currentInstruction.ExpectedDeadline != vch.Deadline {
			return fmt.Errorf("incorrect values, not equals to expected")
		}

		if !bytes.Equal(currentInstruction.NextTarget, s.key.Public().(ed25519.PublicKey)) {
			// willing to open tunnel for a virtual channel

			nextFee := new(big.Int).SetBytes(currentInstruction.NextFee)
			nextCap := new(big.Int).SetBytes(currentInstruction.NextCapacity)

			if currentInstruction.NextDeadline > vch.Deadline-s.deadlineDecreaseNextHop {
				return fmt.Errorf("too short next deadline")
			}

			if nextCap.Cmp(vch.Capacity) == 1 {
				return fmt.Errorf("capacity cannot increase")
			}

			ourFee := new(big.Int).Sub(vch.Fee, nextFee)
			if ourFee.Cmp(s.virtualChannelFee.Nano()) == -1 {
				return fmt.Errorf("min fee to open channel is %s TON", s.virtualChannelFee.String())
			}

			targetChannels, err := s.db.GetActiveChannelsWithKey(currentInstruction.NextTarget)
			if err != nil {
				return fmt.Errorf("failed to get target channel: %w", err)
			}
			// TODO: tampering checks

			if len(targetChannels) == 0 {
				return fmt.Errorf("destination channel is not belongs to this node")
			}

			var target *db.Channel
			for _, targetChannel := range targetChannels {
				balance, err := targetChannel.CalcBalance(false)
				if err != nil {
					return fmt.Errorf("failed to calc our channel %s balance: %w", targetChannel.Address, err)
				}

				amt := new(big.Int).Add(nextCap, nextFee)
				if balance.Cmp(amt) != -1 {
					target = targetChannel
					break
				}
			}

			if target == nil {
				return fmt.Errorf("not enough balance with target to tunnel requested capacity")
			}

			// we will execute it only after all checks passed, for example final signature verify
			toExecute = func() error {
				if err = s.proposeAction(ctx, targetChannels[0], data, payments.VirtualChannel{
					Key:      vch.Key,
					Capacity: nextCap,
					Fee:      nextFee,
					Deadline: currentInstruction.NextDeadline,
				}); err != nil {
					return fmt.Errorf("failed to propose actions to next node: %w", err)
				}

				log.Info().Hex("key", data.Key).
					Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
					Str("fee", tlb.FromNanoTON(vch.Fee).String()).
					Str("target", targetChannels[0].Address).
					Msg("channel tunnelled through us")

				return nil
			}
		} else {
			toExecute = func() error {
				// TODO: check if we accept virtual channels to us
				log.Info().Hex("key", data.Key).
					Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
					Msg("channel opened with us")
				return nil
			}
		}
	default:
		return fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	if channel.Our.IsReady() && signedState.State.CounterpartyData == nil {
		return fmt.Errorf("counterparty state downgrade attempt")
	}

	if signedState.State.CounterpartyData != nil {
		// if seqno is diff we do additional checks and replace counterparty
		if signedState.State.CounterpartyData.Seqno != channel.Our.State.Data.Seqno {
			return fmt.Errorf("counterparty state is incorrect")
		}

		cp, err := channel.Our.State.Data.Copy()
		if err != nil {
			return err
		}
		// we replace it to our value, if something is incorrect, signature will fail
		// we are doing copy to not depend on pointer to our state (which may change during exec)
		channel.Their.State.CounterpartyData = &cp
	}

	channel.Their.State.Data.Seqno = signedState.State.Data.Seqno
	channel.Their.State.Data.Sent = signedState.State.Data.Sent
	channel.Their.Signature = signedState.Signature

	if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
		log.Warn().Msg(channel.Their.State.Dump())
		log.Warn().Msg(signedState.State.Dump())
		return fmt.Errorf("state looks tampered: %w", err)
	}

	cp, err := channel.Their.State.Data.Copy()
	if err != nil {
		return err
	}
	// update our counterparty
	channel.Our.State.CounterpartyData = &cp
	cl, err := tlb.ToCell(channel.Our.State)
	if err != nil {
		return fmt.Errorf("failed to serialize our state for signing: %w", err)
	}
	channel.Our.Signature = payments.Signature{Value: cl.Sign(s.key)}

	if err = s.db.UpdateChannel(channel); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	if toExecute != nil {
		// TODO: This rollback gives ability to next node + sender to scam us by throwing err and keeping state,
		//  need to add additional logic to request seqno increment with state cancellation
		if err = toExecute(); err != nil {
			if rollErr := rollback(); rollErr != nil {
				log.Error().Err(rollErr).Msg("failed to rollback changes")
			}
			return err
		}
	}
	return nil
}

func (s *Service) ProcessInboundChannelRequest(ctx context.Context, capacity *big.Int, walletAddr *address.Address, key ed25519.PublicKey) error {
	if capacity.Cmp(s.maxOutboundCapacity.Nano()) == 1 {
		return fmt.Errorf("")
	}

	list, err := s.db.GetActiveChannelsWithKey(key)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	for _, channel := range list {
		if channel.OurOnchain.Deposited.Sign() == 1 {
			// TODO: respond with its address
			return fmt.Errorf("inbound channel already opened")
		}
	}

	id := key[:16]

	go func() {
		// TODO: more reliable
		addr, err := s.deployChannelWithNode(context.Background(), id, key, walletAddr, tlb.FromNanoTON(capacity))
		if err != nil {
			log.Error().Err(err).Msg("deploy of requested channel is failed")
			return
		}
		log.Info().Str("addr", addr.String()).Msg("requested channel is deployed")
	}()

	return nil
}

func (s *Service) ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action transport.Action) error {
	channel, err := s.GetActiveChannel(channelAddr.String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	log.Debug().Type("action", action).Msg("action request process")

	switch data := action.(type) {
	case transport.CloseVirtualAction:
		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, data.State.BeginParse()); err != nil {
			return fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		_, vch, err := channel.Our.State.FindVirtualChannel(data.Key)
		if err != nil {
			return fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if !vState.Verify(vch.Key) {
			return fmt.Errorf("incorrect channel state signature")
		}

		if vState.Amount.Nano().Cmp(vch.Capacity) == 1 {
			return fmt.Errorf("amount cannot be > capacity")
		}

		if vch.Deadline < time.Now().Unix() {
			return fmt.Errorf("virtual channel is expired")
		}

		err = s.proposeAction(context.Background(), channel, transport.ConfirmCloseAction{
			Key:   data.Key,
			State: data.State,
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to propose action: %w", err)
		}

		// TODO: not create task if we final destination
		// TODO: not allow 2 virtual channels with same key
		tryTill := time.Unix(vch.Deadline+(s.deadlineDecreaseNextHop/2), 0)
		err = s.db.CreateTask("close-next",
			"close-next-"+hex.EncodeToString(vch.Key),
			db.CloseNextTask{
				VirtualKey: data.Key,
				State:      data.State.ToBOC(),
			}, nil, &tryTill,
		)
	case transport.CooperativeCloseAction:
		// TODO: lock channel, to not accept new actions till tx confirms (set status closing)
		var req payments.CooperativeClose
		err = tlb.LoadFromCell(&req, data.SignedCloseRequest.BeginParse())
		if err != nil {
			return fmt.Errorf("failed to serialize their close channel request: %w", err)
		}

		if err = s.ExecuteCooperativeClose(ctx, &req, channel.Address); err != nil {
			return fmt.Errorf("failed to execute cooperative close action: %w", err)
		}
	default:
		return fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	return nil
}
