package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	db "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
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

func (s *Service) ProcessAction(ctx context.Context, key ed25519.PublicKey, lockId int64, channelAddr *address.Address,
	signedState payments.SignedSemiChannel, action transport.Action, updateProof *cell.Cell) (*payments.SignedSemiChannel, error) {
	lockId = -lockId // force negate, to not collide with our locks

	channel, _, unlock, err := s.AcquireChannel(ctx, channelAddr.String(), lockId)
	if err != nil {
		if errors.Is(err, db.ErrChannelBusy) {
			return nil, ErrChannelIsBusy
		}
		return nil, fmt.Errorf("failed to acquire channel lock: %w", err)
	}
	defer unlock()

	if channel.Status != db.ChannelStateActive {
		return nil, fmt.Errorf("channel is not active")
	}

	if !channel.AcceptingActions {
		return nil, fmt.Errorf("channel is currently not accepting new actions")
	}

	if !bytes.Equal(key, channel.TheirOnchain.Key) {
		return nil, fmt.Errorf("incorrect channel key")
	}

	if err = signedState.Verify(channel.TheirOnchain.Key); err != nil {
		return nil, fmt.Errorf("failed to verify passed state: %w", err)
	}

	if signedState.State.Data.Sent.Nano().Cmp(channel.Their.State.Data.Sent.Nano()) == -1 {
		return nil, fmt.Errorf("amount decrease is not allowed")
	}

	if updateProof == nil && !bytes.Equal(signedState.State.Data.ConditionalsHash, make([]byte, 32)) {
		return nil, fmt.Errorf("update proof can be empty only when hash is zero")
	}

	condProposal := cell.NewDict(32)
	if updateProof != nil {
		proofBody, err := cell.UnwrapProof(updateProof, signedState.State.Data.ConditionalsHash)
		if err != nil {
			return nil, fmt.Errorf("incorrect update proof: %w", err)
		}
		condProposal, err = proofBody.BeginParse().ToDict(32)
		if err != nil {
			return nil, fmt.Errorf("failed to load update proof dict: %w", err)
		}
	}

	var toExecute func(ctx context.Context) error
	if signedState.State.Data.Seqno == channel.Their.State.Data.Seqno {
		// idempotency check
		channel.Their.Signature = signedState.Signature
		if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
			return nil, fmt.Errorf("inconsistent state, this seqno with different content was already committed")
		}
		return &channel.Our.SignedSemiChannel, nil
	}

	if signedState.State.Data.Seqno != channel.Their.State.Data.Seqno+1 {
		return nil, fmt.Errorf("incorrect state seqno %d, want %d", signedState.State.Data.Seqno, channel.Their.State.Data.Seqno+1)
	}

	log.Debug().Type("action", action).Msg("action process")

	switch data := action.(type) {
	case transport.IncrementStatesAction:
		hasStates := channel.Our.IsReady() && channel.Their.IsReady()

		toExecute = func(ctx context.Context) error {
			if data.WantResponse {
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Address,
					"increment-state-"+channel.Address+"-"+fmt.Sprint(channel.Our.State.Data.Seqno),
					db.IncrementStatesTask{
						ChannelAddress: channel.Address,
						WantResponse:   false,
					}, nil, nil,
				)
				if err != nil {
					return fmt.Errorf("failed to create increment-state task: %w", err)
				}
			}

			if !hasStates {
				log.Info().Str("address", channel.Address).
					Hex("with", channel.TheirOnchain.Key).
					Msg("onchain channel states exchanged, ready to use")
			}
			return nil
		}
	case transport.RemoveVirtualAction:
		_, _, err = payments.FindVirtualChannel(condProposal, data.Key)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return nil, fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}
		if err == nil {
			return nil, fmt.Errorf("condition should be removed to unlock")
		}

		index, vch, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.Key)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return &channel.Our.SignedSemiChannel, nil
			}
			return nil, fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}

		meta, err := s.db.GetVirtualChannelMeta(context.Background(), vch.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if vch.Deadline >= time.Now().Unix() && meta.Status != db.VirtualChannelStateWantRemove {
			return nil, fmt.Errorf("virtual channel is not expired")
		}

		if err = channel.Their.Conditionals.DeleteIntKey(index); err != nil {
			return nil, fmt.Errorf("failed to remove condition with index %s: %w", index.String(), err)
		}

		toExecute = func(ctx context.Context) error {
			meta.Status = db.VirtualChannelStateRemoved
			meta.UpdatedAt = time.Now()
			if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
				return fmt.Errorf("failed to update virtual channel meta: %w", err)
			}

			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeRemove, meta); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			log.Info().Hex("key", data.Key).Msg("virtual channel removed")
			return nil
		}
	case transport.ConfirmCloseAction:
		_, _, err = payments.FindVirtualChannel(condProposal, data.Key)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return nil, fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}
		if err == nil {
			return nil, fmt.Errorf("condition should be removed to close")
		}

		index, vch, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.Key)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return &channel.Our.SignedSemiChannel, nil
			}
			return nil, fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}

		balanceDiff := new(big.Int).Sub(signedState.State.Data.Sent.Nano(), channel.Their.State.Data.Sent.Nano())

		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, data.State.BeginParse()); err != nil {
			return nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		if !vState.Verify(vch.Key) {
			return nil, fmt.Errorf("incorrect channel state signature")
		}

		if vState.Amount.Nano().Cmp(vch.Capacity) == 1 {
			return nil, fmt.Errorf("amount cannot be > capacity")
		}

		gotAmt := new(big.Int).Add(vState.Amount.Nano(), vch.Fee)
		if gotAmt.Cmp(balanceDiff) == -1 {
			return nil, fmt.Errorf("incorrect amount unlocked: %s instead of %s", balanceDiff.String(), vState.Amount.Nano().String())
		}

		meta, err := s.db.GetVirtualChannelMeta(context.Background(), vch.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if meta.Status != db.VirtualChannelStateWantClose {
			return nil, fmt.Errorf("virtual channel close was not requested")
		}

		if res := meta.GetKnownResolve(vch.Key); res != nil {
			if res.Amount.Nano().Cmp(vState.Amount.Nano()) == 1 {
				return nil, fmt.Errorf("outdated virtual channel state")
			}
		} else {
			return nil, fmt.Errorf("resolve is unknown on node side")
		}

		if err = channel.Their.Conditionals.DeleteIntKey(index); err != nil {
			return nil, fmt.Errorf("failed to remove condition with index %s: %w", index.String(), err)
		}

		toExecute = func(ctx context.Context) error {
			meta.Status = db.VirtualChannelStateClosed
			meta.UpdatedAt = time.Now()
			if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
				return fmt.Errorf("failed to update virtual channel meta: %w", err)
			}

			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeClose, meta); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			log.Info().Hex("key", data.Key).
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Msg("virtual channel closed")

			return nil
		}
	case transport.OpenVirtualAction:
		index, vch, err := payments.FindVirtualChannel(condProposal, data.ChannelKey)
		if err != nil {
			return nil, fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}

		if vch.Capacity.Sign() <= 0 {
			return nil, fmt.Errorf("invalid capacity")
		}

		if vch.Fee.Sign() < 0 {
			return nil, fmt.Errorf("invalid fee")
		}

		if vch.Deadline < time.Now().Unix()+channel.SafeOnchainClosePeriod {
			return nil, fmt.Errorf("too short virtual channel deadline")
		}

		_, oldVC, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.ChannelKey)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return nil, fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}
		if err == nil {
			if oldVC.Deadline == vch.Deadline && oldVC.Fee.Cmp(vch.Fee) == 0 && oldVC.Capacity.Cmp(vch.Capacity) == 0 {
				// idempotency
				return &channel.Our.SignedSemiChannel, nil
			}
			return nil, fmt.Errorf("channel with this key is already exists and has different configuration")
		}

		if _, err = s.db.GetVirtualChannelMeta(context.Background(), vch.Key); err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}
		if err == nil {
			return nil, fmt.Errorf("this virtual channel key was already used before")
		}

		// we put our serialized condition to make sure that party is not cheated,
		// if something diff will be in state, final signature will not match
		if err = channel.Their.Conditionals.SetIntKey(index, vch.Serialize()); err != nil {
			return nil, fmt.Errorf("failed to settle condition with index %s: %w", index.String(), err)
		}

		theirBalance, err := channel.CalcBalance(true)
		if err != nil {
			return nil, fmt.Errorf("failed to calc other side balance: %w", err)
		}

		if theirBalance.Sign() == -1 {
			return nil, fmt.Errorf("not enough available balance, you need %s more to do this", theirBalance.Abs(theirBalance).String())
		}

		currentInstruction, err := data.DecryptOurInstruction(s.key, data.InstructionKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt instruction: %w", err)
		}

		expFee := new(big.Int).SetBytes(currentInstruction.ExpectedFee)
		expCap := new(big.Int).SetBytes(currentInstruction.ExpectedCapacity)

		if expFee.Cmp(vch.Fee) != 0 || expCap.Cmp(vch.Capacity) != 0 || currentInstruction.ExpectedDeadline != vch.Deadline {
			return nil, fmt.Errorf("incorrect values, not equals to expected")
		}

		if !bytes.Equal(currentInstruction.NextTarget, s.key.Public().(ed25519.PublicKey)) {
			// willing to open tunnel for a virtual channel

			nextFee := new(big.Int).SetBytes(currentInstruction.NextFee)
			nextCap := new(big.Int).SetBytes(currentInstruction.NextCapacity)

			if currentInstruction.NextDeadline > vch.Deadline-channel.SafeOnchainClosePeriod {
				return nil, fmt.Errorf("too short next deadline")
			}

			if nextCap.Cmp(vch.Capacity) == 1 {
				return nil, fmt.Errorf("capacity cannot increase")
			}

			ourFee := new(big.Int).Sub(vch.Fee, nextFee)
			if ourFee.Cmp(s.virtualChannelProxyFee.Nano()) == -1 {
				return nil, fmt.Errorf("min fee to open channel is %s TON", s.virtualChannelProxyFee.String())
			}

			targetChannels, err := s.db.GetChannels(context.Background(), currentInstruction.NextTarget, db.ChannelStateAny)
			if err != nil {
				return nil, fmt.Errorf("failed to get target channel: %w", err)
			}
			// TODO: tampering checks

			if len(targetChannels) == 0 {
				return nil, fmt.Errorf("destination channel is not belongs to this node")
			}

			var target *db.Channel
			for _, targetChannel := range targetChannels {
				if targetChannel.Status != db.ChannelStateActive {
					continue
				}

				// token should be the same
				if targetChannel.JettonAddress != channel.JettonAddress {
					continue
				}

				balance, err := targetChannel.CalcBalance(false)
				if err != nil {
					return nil, fmt.Errorf("failed to calc our channel %s balance: %w", targetChannel.Address, err)
				}

				amt := new(big.Int).Add(nextCap, nextFee)
				if balance.Cmp(amt) != -1 {
					target = targetChannel
					break
				}
			}

			if target == nil {
				return nil, fmt.Errorf("not enough balance with target to tunnel requested capacity")
			}

			// we will execute it only after all checks passed and final signature verify
			toExecute = func(ctx context.Context) error {
				data.InstructionKey = currentInstruction.NextInstructionKey

				tryTill := time.Unix(currentInstruction.NextDeadline, 0)
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "open-virtual", target.Address,
					"open-virtual-"+hex.EncodeToString(vch.Key),
					db.OpenVirtualTask{
						PrevChannelAddress: channel.Address,
						ChannelAddress:     target.Address,
						VirtualKey:         vch.Key,
						Deadline:           currentInstruction.NextDeadline,
						Fee:                nextFee.String(),
						Capacity:           nextCap.String(),
						Action:             data,
					}, nil, &tryTill,
				)
				if err != nil {
					return fmt.Errorf("failed to create open-virtual task: %w", err)
				}

				log.Info().Hex("key", data.ChannelKey).
					Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
					Str("fee", tlb.FromNanoTON(vch.Fee).String()).
					Str("target", targetChannels[0].Address).
					Msg("channel tunnelling through us requested")

				return nil
			}
		} else {
			toExecute = func(ctx context.Context) error {
				meta := &db.VirtualChannelMeta{
					Key:    vch.Key,
					Status: db.VirtualChannelStateActive,
					Incoming: &db.VirtualChannelMetaSide{
						ChannelAddress: channel.Address,
						Capacity:       tlb.FromNanoTON(vch.Capacity).String(),
						Fee:            tlb.FromNanoTON(vch.Fee).String(),
						Deadline:       time.Unix(vch.Deadline, 0),
					},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				if currentInstruction.FinalState != nil {
					var state payments.VirtualChannelState
					if err = tlb.LoadFromCell(&state, currentInstruction.FinalState.BeginParse()); err != nil {
						return fmt.Errorf("failed to parse virtual channel state: %w", err)
					}

					if !state.Verify(vch.Key) {
						return fmt.Errorf("final state is incorrect")
					}

					if state.Amount.Nano().Cmp(vch.Capacity) != 0 {
						return fmt.Errorf("final state should use full capacity")
					}

					if err = meta.AddKnownResolve(meta.Key, &state); err != nil {
						return fmt.Errorf("failed to add channel condition resolve: %w", err)
					}

					tryTill := time.Unix(vch.Deadline, 0)
					if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-next-virtual", channel.Address,
						"close-next-"+hex.EncodeToString(vch.Key),
						db.CloseNextVirtualTask{
							VirtualKey: vch.Key,
							State:      currentInstruction.FinalState.ToBOC(),
							IsTransfer: true,
						}, nil, &tryTill,
					); err != nil {
						return fmt.Errorf("failed to create close-next-virtual task: %w", err)
					}
				}

				if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to update virtual channel meta: %w", err)
				}

				if currentInstruction.FinalState == nil && s.webhook != nil {
					if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, meta); err != nil {
						return fmt.Errorf("failed to push virtual channel close event: %w", err)
					}
				}

				log.Info().Hex("key", data.ChannelKey).
					Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
					Msg("virtual channel opened with us")

				return nil
			}
		}
	default:
		return nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	if channel.Our.IsReady() && signedState.State.CounterpartyData == nil {
		return nil, fmt.Errorf("counterparty state downgrade attempt")
	}

	if signedState.State.CounterpartyData != nil {
		// if seqno is diff we do additional checks and replace counterparty
		if signedState.State.CounterpartyData.Seqno != channel.Our.State.Data.Seqno {
			return nil, fmt.Errorf("counterparty state is incorrect")
		}

		cp, err := channel.Our.State.Data.Copy()
		if err != nil {
			return nil, err
		}
		// we replace it to our value, if something is incorrect, signature will fail
		// we are doing copy to not depend on pointer to our state (which may change during exec)
		channel.Their.State.CounterpartyData = &cp
	}

	condHash := make([]byte, 32)
	if !channel.Their.Conditionals.IsEmpty() {
		condHash = channel.Their.Conditionals.AsCell().Hash()
	}

	if !bytes.Equal(condHash, signedState.State.Data.ConditionalsHash) {
		return nil, fmt.Errorf("incorrect resulting hash")
	}

	channel.Their.State.Data.Seqno = signedState.State.Data.Seqno
	channel.Their.State.Data.Sent = signedState.State.Data.Sent
	channel.Their.State.Data.ConditionalsHash = signedState.State.Data.ConditionalsHash
	channel.Their.Signature = signedState.Signature

	if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
		log.Warn().Msg(channel.Their.State.Dump())
		log.Warn().Msg(signedState.State.Dump())
		return nil, fmt.Errorf("state looks tampered: %w", err)
	}

	cp, err := channel.Their.State.Data.Copy()
	if err != nil {
		return nil, err
	}
	// update our counterparty
	channel.Our.State.CounterpartyData = &cp
	cl, err := tlb.ToCell(channel.Our.State)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize our state for signing: %w", err)
	}
	channel.Our.Signature = payments.Signature{Value: cl.Sign(s.key)}

	if err = s.db.Transaction(context.Background(), func(ctx context.Context) error {
		if toExecute != nil {
			if err = toExecute(ctx); err != nil {
				return err
			}
		}
		if err = s.db.UpdateChannel(ctx, channel); err != nil {
			return fmt.Errorf("failed to update channel in db: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.touchWorker()

	return &channel.Our.SignedSemiChannel, nil
}

func (s *Service) ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action transport.Action) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	channel, err := s.GetActiveChannel(ctx, channelAddr.String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if !bytes.Equal(channel.TheirOnchain.Key, key) {
		return fmt.Errorf("unauthorized channel")
	}

	log.Debug().Type("action", action).Msg("action request process")

	switch data := action.(type) {
	case transport.RequestRemoveVirtualAction:
		if !channel.AcceptingActions {
			return fmt.Errorf("channel is currently not accepting new actions")
		}

		_, vch, err := payments.FindVirtualChannel(channel.Our.Conditionals, data.Key)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				return fmt.Errorf("virtual channel is not found")
			}
			return fmt.Errorf("failed to find virtual channel: %w", err)
		}

		tryTill := time.Unix(vch.Deadline, 0)
		if err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "remove-virtual", channel.Address,
			"remove-virtual-"+hex.EncodeToString(vch.Key),
			db.RemoveVirtualTask{
				Key: data.Key,
			}, nil, &tryTill,
		); err != nil {
			return fmt.Errorf("failed to create remove-virtual task: %w", err)
		}
		s.touchWorker()
	case transport.CloseVirtualAction:
		if !channel.AcceptingActions {
			return fmt.Errorf("channel is currently not accepting new actions")
		}

		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, data.State.BeginParse()); err != nil {
			return fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		_, vch, err := payments.FindVirtualChannel(channel.Our.Conditionals, data.Key)
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

		if err = s.db.Transaction(context.Background(), func(ctx context.Context) error {
			if err = s.AddVirtualChannelResolve(ctx, vch.Key, vState); err != nil {
				return fmt.Errorf("failed to add virtual channel resolve: %w", err)
			}

			tryTill := time.Unix(vch.Deadline+(channel.SafeOnchainClosePeriod/2), 0)
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "confirm-close-virtual", channel.Address,
				"confirm-close-virtual-"+hex.EncodeToString(vch.Key),
				db.ConfirmCloseVirtualTask{
					VirtualKey: data.Key,
				}, nil, &tryTill,
			); err != nil {
				return fmt.Errorf("failed to create confirm-close-virtual task: %w", err)
			}

			// We start uncooperative close at specific moment to have time
			// to commit resolve onchain in case partner is irresponsible.
			// But in the same time we give our partner time to
			uncooperativeAfter := time.Unix(vch.Deadline-channel.SafeOnchainClosePeriod, 0)
			minDelay := time.Now().Add(1 * time.Minute)
			if !uncooperativeAfter.After(minDelay) {
				uncooperativeAfter = minDelay
			}

			// Creating aggressive onchain close task, for the future,
			// in case we will not be able to communicate with party
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", channel.Address+"-uncoop",
				"uncooperative-close-"+channel.Address+"-vc-"+hex.EncodeToString(vch.Key),
				db.ChannelUncooperativeCloseTask{
					Address:                 channel.Address,
					CheckVirtualStillExists: vch.Key,
				}, &uncooperativeAfter, nil,
			); err != nil {
				return fmt.Errorf("failed to create uncooperative close task: %w", err)
			}

			return nil
		}); err != nil {
			return err
		}
		s.touchWorker()
	case transport.CooperativeCloseAction:
		var req payments.CooperativeClose
		err = tlb.LoadFromCell(&req, data.SignedCloseRequest.BeginParse())
		if err != nil {
			return fmt.Errorf("failed to serialize their close channel request: %w", err)
		}

		log.Info().Str("address", channel.Address).Msg("received cooperative close request")

		if err = s.executeCooperativeClose(ctx, &req, channel.Address); err != nil {
			return fmt.Errorf("failed to execute cooperative close action: %w", err)
		}
	default:
		return fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	return nil
}

func (s *Service) discoverChannel(channelAddr *address.Address) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	block, err := s.ton.CurrentMasterchainInfo(ctx)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to get current block")
		return false
	}

	acc, err := s.ton.GetAccount(ctx, block, channelAddr)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to get account")
		return false
	}

	txList, err := s.ton.ListTransactions(ctx, channelAddr, 1, acc.LastTxLT, acc.LastTxHash)
	if err != nil {
		if !errors.Is(err, ton.ErrNoTransactionsWereFound) {
			log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to get tx list")
		}
		return false
	}

	if len(txList) == 0 {
		log.Warn().Str("address", channelAddr.String()).Msg("no transactions at requested unknown account")
		return false
	}

	if txList[0].LT != acc.LastTxLT {
		log.Warn().Str("address", channelAddr.String()).Msg("incorrect last tx lt at requested unknown account")
		return false
	}

	ch, err := s.contractMaker.ParseAsyncChannel(channelAddr, acc.Code, acc.Data, true)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to parse channel")
		return false
	}

	log.Info().Str("address", channelAddr.String()).Msg("discovered previously unknown channel, scheduling force check")
	s.updates <- ChannelUpdatedEvent{
		Transaction: txList[0],
		Channel:     ch,
	}

	return true
}
