package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
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
	signedState payments.SignedSemiChannel, action transport.Action, updateProof *cell.Cell, fromWeb bool) (*payments.SignedSemiChannel, error) {
	lockId = -lockId // force negate, to not collide with our locks

	channel, _, unlock, err := s.AcquireChannel(ctx, channelAddr.String(), lockId)
	if err != nil {
		if errors.Is(err, db.ErrChannelBusy) {
			return nil, ErrChannelIsBusy
		} else if errors.Is(err, db.ErrNotFound) {
			if s.discoveryMx.TryLock() {
				go func() {
					// our party proposed action with a channel we don't know,
					// we will try to find it onchain and register (asynchronously)
					s.discoverChannel(channelAddr)
					s.discoveryMx.Unlock()
				}()
			}
		}
		return nil, fmt.Errorf("failed to acquire channel lock: %w", err)
	}
	defer unlock()

	if channel.Status != db.ChannelStateActive {
		if s.discoveryMx.TryLock() {
			go func() {
				// our party proposed action with channel we don't know,
				// we will try to find it onchain and register (asynchronously)
				s.discoverChannel(channelAddr)
				s.discoveryMx.Unlock()
			}()
		}
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

	cc, err := s.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, true)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve coin config: %w", err)
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

	log.Debug().Str("action", reflect.TypeOf(action).String()).Msg("action process")

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
					Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
					Msg("channel states exchanged, ready to use")
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

		if vch.Deadline >= time.Now().UTC().Unix() && meta.Status != db.VirtualChannelStateWantRemove {
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
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeRemove, meta, cc); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			log.Info().Str("key", base64.StdEncoding.EncodeToString(data.Key)).Msg("virtual channel removed")
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

		if vState.Amount.Cmp(vch.Capacity) > 0 {
			return nil, fmt.Errorf("amount cannot be > capacity")
		}

		needAmt := new(big.Int).Add(vState.Amount, vch.Fee)
		needAmt = needAmt.Sub(needAmt, vch.Prepay)

		if needAmt.Sign() < 0 {
			// prepaid more than actual, we consider diff as our earning because of user's strange behave
			needAmt.SetInt64(0)
		}

		if needAmt.Cmp(balanceDiff) > 0 {
			return nil, fmt.Errorf("incorrect amount unlocked: %s instead of %s", balanceDiff.String(), needAmt.String())
		}

		meta, err := s.db.GetVirtualChannelMeta(context.Background(), vch.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if meta.Status != db.VirtualChannelStateWantClose {
			return nil, fmt.Errorf("virtual channel close was not requested")
		}

		if res := meta.GetKnownResolve(); res != nil {
			if res.Amount.Cmp(vState.Amount) > 0 {
				return nil, fmt.Errorf("outdated virtual channel state")
			}
		} else {
			return nil, fmt.Errorf("resolve is unknown on node side")
		}

		if err = channel.Their.Conditionals.DeleteIntKey(index); err != nil {
			return nil, fmt.Errorf("failed to remove condition with index %s: %w", index.String(), err)
		}

		if channel.TheirLockedDeposit != nil {
			// mark part of the rented deposit as used
			channel.TheirLockedDeposit.Used.Add(channel.TheirLockedDeposit.Used, balanceDiff)
		}

		toExecute = func(ctx context.Context) error {
			meta.Status = db.VirtualChannelStateClosed
			meta.UpdatedAt = time.Now()
			if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
				return fmt.Errorf("failed to update virtual channel meta: %w", err)
			}

			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeClose, meta, cc); err != nil {
					return fmt.Errorf("failed to push virtual channel close event: %w", err)
				}
			}

			var senderKey []byte
			if meta.Incoming != nil {
				senderKey = meta.Incoming.SenderKey
			}

			evData := db.ChannelHistoryActionTransferInData{
				Amount: vState.Amount.String(),
				From:   senderKey,
			}

			jsonData, err := json.Marshal(evData)
			if err != nil {
				log.Error().Err(err).Msg("failed to marshal event data")
			}

			if err = s.db.CreateChannelEvent(ctx, channel, meta.UpdatedAt, db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionTransferIn,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel event: %w", err)
			}

			log.Info().Str("key", base64.StdEncoding.EncodeToString(data.Key)).
				Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
				Str("fee", tlb.MustFromNano(vch.Fee, int(cc.Decimals)).String()).
				Str("prepay", tlb.MustFromNano(vch.Prepay, int(cc.Decimals)).String()).
				Msg("virtual channel closed")

			return nil
		}
	case transport.CommitVirtualAction:
		_, vchNew, err := payments.FindVirtualChannel(condProposal, data.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to find virtual channel in their new state: %w", err)
		}

		index, vchOld, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to find virtual channel in their old state: %w", err)
		}

		prepayAmt := new(big.Int).Sub(vchNew.Prepay, vchOld.Prepay)
		if prepayAmt.Sign() < 0 {
			return nil, fmt.Errorf("prepay cannot be decreased")
		}

		sentDiff := new(big.Int).Sub(signedState.State.Data.Sent.Nano(), channel.Their.State.Data.Sent.Nano())
		if sentDiff.Cmp(prepayAmt) < 0 {
			return nil, fmt.Errorf("sent is less than prepay")
		}

		// we put our serialized condition to make sure that party is not cheated,
		// if something diff will be in state, final signature will not match
		vchOld.Prepay = vchNew.Prepay // only prepay can change
		if err = channel.Their.Conditionals.SetIntKey(index, vchOld.Serialize()); err != nil {
			return nil, fmt.Errorf("failed to set condition with index %s: %w", index.String(), err)
		}

		toExecute = func(ctx context.Context) error {
			if prepayAmt.Sign() == 0 {
				return nil
			}

			log.Info().Str("key", base64.StdEncoding.EncodeToString(data.Key)).
				Str("capacity", tlb.MustFromNano(vchOld.Capacity, int(cc.Decimals)).String()).
				Str("fee", tlb.MustFromNano(vchOld.Fee, int(cc.Decimals)).String()).
				Str("prepaid", tlb.MustFromNano(vchNew.Prepay, int(cc.Decimals)).String()).
				Str("prepay_diff", tlb.MustFromNano(prepayAmt, int(cc.Decimals)).String()).
				Str("sent_diff", tlb.MustFromNano(sentDiff, int(cc.Decimals)).String()).
				Msg("virtual channel prepaid")

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

		if safe := vch.Deadline - (time.Now().UTC().Unix() + channel.SafeOnchainClosePeriod); safe < int64(s.cfg.MinSafeVirtualChannelTimeoutSec) {
			return nil, fmt.Errorf("safe virtual channel deadline is less than acceptable: %d, %d", safe, s.cfg.MinSafeVirtualChannelTimeoutSec)
		}

		_, vchOld, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.ChannelKey)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return nil, fmt.Errorf("failed to find virtual channel in their prev state: %w", err)
		}
		if err == nil {
			if vchOld.Deadline == vch.Deadline && vchOld.Fee.Cmp(vch.Fee) == 0 && vchOld.Capacity.Cmp(vch.Capacity) == 0 {
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

		theirBalance, _, err := channel.CalcBalance(true)
		if err != nil {
			return nil, fmt.Errorf("failed to calc other side balance: %w", err)
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

		removeAfterTimeout := func(ctx context.Context) error {
			// try to remove virtual channel in future if it will time out
			dl := time.Unix(vch.Deadline+1, 0)
			err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", channel.Address,
				"ask-remove-virtual-"+base64.StdEncoding.EncodeToString(vch.Key)+"-timeout",
				db.AskRemoveVirtualTask{
					ChannelAddress: channel.Address,
					Key:            vch.Key,
				}, &dl, nil,
			)
			if err != nil {
				return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
			}
			return nil
		}

		if !bytes.Equal(currentInstruction.NextTarget, s.key.Public().(ed25519.PublicKey)) {
			// willing to open tunnel for a virtual channel, for this we require party to have enough balance
			if theirBalance.Sign() < 0 {
				return nil, fmt.Errorf("not enough available balance, you need %s more tunnel channel through me", theirBalance.Abs(theirBalance).String())
			}

			nextFee := new(big.Int).SetBytes(currentInstruction.NextFee)
			nextCap := new(big.Int).SetBytes(currentInstruction.NextCapacity)

			maxNextDeadline := vch.Deadline - (channel.SafeOnchainClosePeriod + int64(s.cfg.MinSafeVirtualChannelTimeoutSec))
			if currentInstruction.NextDeadline > maxNextDeadline {
				return nil, fmt.Errorf("next deadline too late (not enough safety gap)")
			}

			if nextCap.Cmp(vch.Capacity) > 0 {
				return nil, fmt.Errorf("capacity cannot increase")
			}

			if !cc.VirtualTunnelConfig.AllowTunneling {
				return nil, fmt.Errorf("tunneling of such coin is not allowed through this node")
			}

			wantMinFee := tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMinFee, int(cc.Decimals))
			wantFeePercent := cc.VirtualTunnelConfig.ProxyFeePercent / 100.0

			wantFeeInt := new(big.Int).Add(nextCap, nextFee)

			maxCap := tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMaxCapacity, int(cc.Decimals))
			if wantFeeInt.Cmp(maxCap.Nano()) > 0 {
				return nil, fmt.Errorf("too big next capacity+fee")
			}

			wantFeeInt, _ = new(big.Float).Mul(new(big.Float).SetInt(wantFeeInt), big.NewFloat(wantFeePercent)).Int(wantFeeInt)
			wantFee := tlb.MustFromNano(wantFeeInt, int(cc.Decimals))
			if wantFee.Compare(wantMinFee) < 0 {
				wantFee = wantMinFee
			}

			proposedFee := new(big.Int).Sub(vch.Fee, nextFee)
			if proposedFee.Cmp(wantFee.Nano()) < 0 {
				return nil, fmt.Errorf("min fee to open channel is %s TON", wantFee.String())
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
			var targetNoBalance *db.Channel
			var highestBalance *big.Int
			for _, targetChannel := range targetChannels {
				if targetChannel.Status != db.ChannelStateActive {
					continue
				}

				// token should be the same
				if targetChannel.JettonAddress != channel.JettonAddress {
					continue
				}

				if targetChannel.ExtraCurrencyID != channel.ExtraCurrencyID {
					continue
				}

				balance, _, err := targetChannel.CalcBalance(false)
				if err != nil {
					return nil, fmt.Errorf("failed to calc our channel %s balance: %w", targetChannel.Address, err)
				}

				amt := new(big.Int).Add(nextCap, nextFee)
				if balance.Cmp(amt) >= 0 {
					// balance is enough to tunnel
					target = targetChannel
					break
				}

				if highestBalance == nil || (highestBalance.Sign() >= 0 && balance.Cmp(highestBalance) > 0) || balance.Sign() < 0 {
					// if we already have a credited channel, it is better to use it again for less onchain actions
					targetNoBalance = targetChannel
					highestBalance = balance
				}
			}

			if target == nil {
				if targetNoBalance == nil {
					return nil, fmt.Errorf("no active channel with %s to tunnel requested capacity", base64.StdEncoding.EncodeToString(currentInstruction.NextTarget))
				}
				target = targetNoBalance // we will tunnel and topup to get a resolve
			}

			// we will execute it only after all checks passed and final signature verified
			toExecute = func(ctx context.Context) error {
				senderKey := data.InstructionKey
				data.InstructionKey = currentInstruction.NextInstructionKey

				tryTill := time.Unix(currentInstruction.NextDeadline, 0)
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "open-virtual", target.Address,
					"open-virtual-"+base64.StdEncoding.EncodeToString(vch.Key),
					db.OpenVirtualTask{
						SenderKey:          senderKey,
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

				if err = removeAfterTimeout(ctx); err != nil {
					return err
				}

				log.Info().Str("key", base64.StdEncoding.EncodeToString(data.ChannelKey)).
					Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
					Str("fee", tlb.MustFromNano(vch.Fee, int(cc.Decimals)).String()).
					Str("target", target.Address).
					Msg("channel tunnelling through us requested")

				return nil
			}
		} else {
			var state payments.VirtualChannelState
			if currentInstruction.FinalState != nil {
				// for channels with known final state we allow credit, so we will wait for their topup and use it
				if err = tlb.LoadFromCell(&state, currentInstruction.FinalState.BeginParse()); err != nil {
					return nil, fmt.Errorf("failed to parse virtual channel state: %w", err)
				}

				if !state.Verify(vch.Key) {
					return nil, fmt.Errorf("final state is incorrect")
				}

				if state.Amount.Cmp(vch.Capacity) != 0 {
					return nil, fmt.Errorf("final state should use full capacity")
				}

				maxRentPerAction := cc.MustAmountDecimal(cc.VirtualTunnelConfig.MaxCapacityToRentPerTx)
				if new(big.Int).Add(state.Amount, vch.Fee).Cmp(maxRentPerAction.Nano()) > 0 {
					return nil, fmt.Errorf("amount to receive is too big to rent in single operation")
				}
			} else if theirBalance.Sign() < 0 {
				return nil, fmt.Errorf("not enough available balance, you need %s more to open virtual channel with me", theirBalance.Abs(theirBalance).String())
			}

			toExecute = func(ctx context.Context) error {
				meta := &db.VirtualChannelMeta{
					Key:    vch.Key,
					Status: db.VirtualChannelStateActive,
					Incoming: &db.VirtualChannelMetaSide{
						SenderKey:             data.InstructionKey,
						ChannelAddress:        channel.Address,
						Capacity:              tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String(),
						Fee:                   tlb.MustFromNano(vch.Fee, int(cc.Decimals)).String(),
						UncooperativeDeadline: time.Unix(vch.Deadline, 0),
						SafeDeadline:          time.Unix(vch.Deadline, 0).Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
					},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				if currentInstruction.FinalState != nil {
					if err = meta.AddKnownResolve(&state); err != nil {
						return fmt.Errorf("failed to add channel condition resolve: %w", err)
					}

					tryTill := time.Unix(vch.Deadline, 0)
					if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-next-virtual", channel.Address,
						"close-next-"+base64.StdEncoding.EncodeToString(vch.Key),
						db.CloseNextVirtualTask{
							VirtualKey: vch.Key,
							State:      currentInstruction.FinalState.ToBOC(),
						}, nil, &tryTill,
					); err != nil {
						return fmt.Errorf("failed to create close-next-virtual task: %w", err)
					}
				} else {
					if err = removeAfterTimeout(ctx); err != nil {
						return err
					}
				}

				if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to update virtual channel meta: %w", err)
				}

				if currentInstruction.FinalState == nil && s.webhook != nil {
					if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, meta, cc); err != nil {
						return fmt.Errorf("failed to push virtual channel close event: %w", err)
					}
				}

				log.Info().Str("key", base64.StdEncoding.EncodeToString(data.ChannelKey)).
					Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
					Msg("virtual channel opened with us")

				return nil
			}
		}
	case transport.CooperativeCommitAction:
		// When requested to process this kind of action,
		// then we should execute tx by ourselves and take fee from a party for gas
		// it is useful when the party doesn't have a onchain balance yet
		attachedFee := new(big.Int).Sub(signedState.State.Data.Sent.Nano(), channel.Their.State.Data.Sent.Nano())
		wantFeePerAction := cc.MustAmountDecimal(cc.FeePerWithdrawPropose)
		if wantFeePerAction.Nano().Sign() <= 0 {
			return nil, fmt.Errorf("node not accepts cooperative commit proposals, use request")
		}

		if attachedFee.Cmp(wantFeePerAction.Nano()) < 0 {
			return nil, fmt.Errorf("not enough fee, %s need %s", cc.MustAmount(attachedFee).String(), wantFeePerAction.String())
		}

		theirBalance, _, err := channel.CalcBalance(true)
		if err != nil {
			return nil, fmt.Errorf("failed to calc other side balance: %w", err)
		}

		if theirBalance.Cmp(attachedFee) < 0 {
			return nil, fmt.Errorf("balance is less than attached fee")
		}

		var reqTheir payments.CooperativeCommit
		err = tlb.LoadFromCell(&reqTheir, data.SignedCommitRequest.BeginParse())
		if err != nil {
			return nil, fmt.Errorf("failed to serialize their commit channel request: %w", err)
		}

		wOur, wTheir := reqTheir.Signed.WithdrawA, reqTheir.Signed.WithdrawB
		if !channel.WeLeft {
			wOur, wTheir = wTheir, wOur
		}

		var theirSignature = reqTheir.SignatureA.Value
		if channel.WeLeft {
			theirSignature = reqTheir.SignatureB.Value
		}

		req, dataCell, _, err := s.getCommitRequest(wOur, wTheir, channel)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare commit channel request: %w", err)
		}

		if !dataCell.Verify(channel.TheirOnchain.Key, theirSignature) {
			return nil, fmt.Errorf("incorrect party signature")
		}

		if channel.WeLeft {
			req.SignatureB.Value = theirSignature
		} else {
			req.SignatureA.Value = theirSignature
		}

		cl, err := tlb.ToCell(req)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize commit channel request: %w", err)
		}

		ourW := req.Signed.WithdrawA.Nano()
		theirW := req.Signed.WithdrawB.Nano()
		if !channel.WeLeft {
			ourW, theirW = theirW, ourW
		}

		if err = channel.UpdatePendingWithdraw(false, ourW); err != nil {
			return nil, fmt.Errorf("failed to update our pending withdraw: %w", err)
		}
		if err = channel.UpdatePendingWithdraw(true, theirW); err != nil {
			return nil, fmt.Errorf("failed to update their pending withdraw: %w", err)
		}

		toExecute = func(ctx context.Context) error {
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Address,
				"increment-state-"+channel.Address+"-"+fmt.Sprint(channel.Our.State.Data.Seqno),
				db.IncrementStatesTask{
					ChannelAddress: channel.Address,
					WantResponse:   true,
				}, nil, nil,
			); err != nil {
				return fmt.Errorf("failed to create increment-state task: %w", err)
			}

			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "withdraw-execute", channel.Address,
				"withdraw-execute-"+channel.Address+"-"+hex.EncodeToString(cl.Hash()),
				db.WithdrawExecuteTask{
					Address:       channel.Address,
					SignedRequest: cl,
				}, nil, nil,
			); err != nil {
				return fmt.Errorf("failed to create increment-state task: %w", err)
			}

			log.Info().Str("address", channel.Address).Msg("accepted cooperative commit proposal")
			return nil
		}
	case transport.RentCapacityAction:
		attachedFee := new(big.Int).Sub(signedState.State.Data.Sent.Nano(), channel.Their.State.Data.Sent.Nano())
		amount := new(big.Int).SetBytes(data.Amount)
		maxRentPerAction := cc.MustAmountDecimal(cc.VirtualTunnelConfig.MaxCapacityToRentPerTx)

		dt := time.Unix(int64(data.Till), 0)

		amountBefore := big.NewInt(0)
		used := big.NewInt(0)
		if channel.OurLockedDeposit != nil && channel.OurLockedDeposit.Till.After(time.Now()) {
			used = new(big.Int).Set(channel.OurLockedDeposit.Used)
			amountBefore = new(big.Int).Set(channel.OurLockedDeposit.Amount)
		}
		diffAmount := new(big.Int).Sub(amount, amountBefore)

		if time.Until(dt) > 366*24*time.Hour {
			// more than one year is too long
			return nil, fmt.Errorf("duration too long")
		}
		if diffAmount.Sign() <= 0 {
			return nil, fmt.Errorf("resulting topup amount must be positive")
		}
		if diffAmount.Cmp(maxRentPerAction.Nano()) > 0 {
			return nil, fmt.Errorf("capacity to rent is too big")
		}

		totalFee := channel.CalcDepositFee(cc, amount, dt, false)
		if totalFee.Sign() <= 0 {
			return nil, fmt.Errorf("payment not needed, capacity is already enough")
		}

		if attachedFee.Cmp(totalFee) < 0 {
			return nil, fmt.Errorf("not enough fee, %s need %s", cc.MustAmount(attachedFee).String(), cc.MustAmount(totalFee).String())
		}

		theirBalance, _, err := channel.CalcBalance(true)
		if err != nil {
			return nil, fmt.Errorf("failed to calc their balance: %w", err)
		}
		_, ourLockedBalance, err := channel.CalcBalance(false)
		if err != nil {
			return nil, fmt.Errorf("failed to calc our balance: %w", err)
		}

		// calc balance + amount potentially could be received
		usableBalance := new(big.Int).Add(theirBalance, ourLockedBalance)
		// pending withdraw is not usable
		pendingWithdraw := new(big.Int).Sub(channel.Our.PendingWithdraw, channel.OurOnchain.Withdrawn)
		if pendingWithdraw.Sign() > 0 {
			usableBalance = usableBalance.Sub(usableBalance, pendingWithdraw)
		}

		if usableBalance.Cmp(totalFee) < 0 {
			return nil, fmt.Errorf("not enough locked+balance for fee to rent")
		}

		channel.OurLockedDeposit = &db.LockedDepositInfo{
			Amount: amount,
			Till:   dt,
			Used:   used,
		}

		// we will execute it only after all checks passed and final signature verified
		toExecute = func(ctx context.Context) error {
			evData := db.ChannelHistoryActionRentCapData{
				Amount: amount.String(),
				Fee:    totalFee.String(),
				Till:   dt.Unix(),
			}
			jsonData, err := json.Marshal(evData)
			if err != nil {
				return fmt.Errorf("failed to marshal event data: %w", err)
			}

			if err = s.db.CreateChannelEvent(ctx, channel, time.Now(), db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionOurCapacityRented,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel our cap rent event: %w", err)
			}

			// topup will be executed by balance handler on chanel updated
			log.Info().Str("total", cc.MustAmount(amount).String()).
				Str("amount", cc.MustAmount(diffAmount).String()).
				Str("paid", cc.MustAmount(attachedFee).String()).
				Str("channel", channel.Address).
				Msg("capacity purchased")

			return nil
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

	/*bal, _, err := channel.CalcBalance(true)
	if err != nil {
		return nil, fmt.Errorf("failed to calc balance: %w", err)
	}

	if bal.Sign() < 0 {
		return nil, fmt.Errorf("balance cannot be negative")
	}*/

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
	channel.WebPeer = fromWeb

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

func (s *Service) ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action transport.Action) ([]byte, error) {
	s.mx.Lock()
	defer s.mx.Unlock()

	channel, err := s.GetActiveChannel(ctx, channelAddr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	if !bytes.Equal(channel.TheirOnchain.Key, key) {
		return nil, fmt.Errorf("unauthorized channel")
	}

	log.Debug().Str("action", reflect.TypeOf(action).String()).Msg("action request process")

	switch data := action.(type) {
	case transport.RequestRemoveVirtualAction:
		if !channel.AcceptingActions {
			return nil, fmt.Errorf("channel is currently not accepting new actions")
		}

		_, vch, err := payments.FindVirtualChannel(channel.Our.Conditionals, data.Key)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				return nil, fmt.Errorf("virtual channel is not found")
			}
			return nil, fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "remove-virtual", channel.Address,
			"remove-virtual-"+base64.StdEncoding.EncodeToString(vch.Key)+"-requested",
			db.RemoveVirtualTask{
				Key: data.Key,
			}, nil, nil,
		); err != nil {
			return nil, fmt.Errorf("failed to create remove-virtual task: %w", err)
		}
		s.touchWorker()
	case transport.CloseVirtualAction:
		if !channel.AcceptingActions {
			return nil, fmt.Errorf("channel is currently not accepting new actions")
		}

		var vState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&vState, data.State.BeginParse()); err != nil {
			return nil, fmt.Errorf("failed to load virtual channel state cell: %w", err)
		}

		_, vch, err := payments.FindVirtualChannel(channel.Our.Conditionals, data.Key)
		if err != nil {
			return nil, fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if !vState.Verify(vch.Key) {
			return nil, fmt.Errorf("incorrect channel state signature")
		}

		if vState.Amount.Cmp(vch.Capacity) > 0 {
			return nil, fmt.Errorf("amount cannot be > capacity")
		}

		if vch.Deadline < time.Now().UTC().Unix() {
			return nil, fmt.Errorf("virtual channel is expired")
		}

		if err = s.db.Transaction(context.Background(), func(ctx context.Context) error {
			if err = s.AddVirtualChannelResolve(ctx, vch.Key, vState); err != nil {
				// we don't care if it is older, since party wants to close with this amount
				if !errors.Is(err, db.ErrNewerStateIsKnown) {
					return fmt.Errorf("failed to add virtual channel resolve: %w", err)
				}
			}

			// serialize by ourselves for safety
			stateCell, err := vState.ToCell()
			if err != nil {
				return fmt.Errorf("failed to serialize virtual channel state: %w", err)
			}

			tryTill := time.Unix(vch.Deadline+(channel.SafeOnchainClosePeriod/2), 0)
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "confirm-close-virtual", channel.Address,
				"confirm-close-virtual-"+base64.StdEncoding.EncodeToString(vch.Key),
				db.ConfirmCloseVirtualTask{
					VirtualKey: data.Key,
					State:      stateCell.ToBOC(),
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
				"uncooperative-close-"+channel.Address+"-vc-"+base64.StdEncoding.EncodeToString(vch.Key),
				db.ChannelUncooperativeCloseTask{
					Address:                 channel.Address,
					CheckVirtualStillExists: vch.Key,
				}, &uncooperativeAfter, nil,
			); err != nil {
				return fmt.Errorf("failed to create uncooperative close task: %w", err)
			}

			return nil
		}); err != nil {
			return nil, err
		}
		s.touchWorker()
	case transport.CooperativeCloseAction:
		var req payments.CooperativeClose
		err = tlb.LoadFromCell(&req, data.SignedCloseRequest.BeginParse())
		if err != nil {
			return nil, fmt.Errorf("failed to serialize their close channel request: %w", err)
		}

		log.Info().Str("address", channel.Address).Msg("received cooperative close request")

		_, dataCell, ourSignature, err := s.getCooperativeCloseRequest(channel)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare close channel request: %w", err)
		}

		var theirSignature = req.SignatureA.Value
		if channel.WeLeft {
			theirSignature = req.SignatureB.Value
		}

		if !dataCell.Verify(channel.TheirOnchain.Key, theirSignature) {
			return nil, fmt.Errorf("incorrect party signature")
		}

		channel.AcceptingActions = false
		if err = s.db.UpdateChannel(ctx, channel); err != nil {
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}

		return ourSignature, nil
	case transport.CooperativeCommitAction:
		if !channel.AcceptingActions {
			return nil, fmt.Errorf("channel is currently not accepting new actions")
		}

		var req payments.CooperativeCommit
		err = tlb.LoadFromCell(&req, data.SignedCommitRequest.BeginParse())
		if err != nil {
			return nil, fmt.Errorf("failed to serialize their commit channel request: %w", err)
		}

		log.Info().Str("address", channel.Address).Msg("received cooperative commit request")

		wOur, wTheir := req.Signed.WithdrawA, req.Signed.WithdrawB
		if !channel.WeLeft {
			wOur, wTheir = wTheir, wOur
		}

		_, dataCell, ourSignature, err := s.getCommitRequest(wOur, wTheir, channel)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare commit channel request: %w", err)
		}

		var theirSignature = req.SignatureA.Value
		if channel.WeLeft {
			theirSignature = req.SignatureB.Value
		}

		if !dataCell.Verify(channel.TheirOnchain.Key, theirSignature) {
			return nil, fmt.Errorf("incorrect party signature")
		}

		ourW := req.Signed.WithdrawA.Nano()
		theirW := req.Signed.WithdrawB.Nano()
		if !channel.WeLeft {
			ourW, theirW = theirW, ourW
		}

		if err = channel.UpdatePendingWithdraw(false, ourW); err != nil {
			return nil, fmt.Errorf("failed to update our pending withdraw: %w", err)
		}
		if err = channel.UpdatePendingWithdraw(true, theirW); err != nil {
			return nil, fmt.Errorf("failed to update their pending withdraw: %w", err)
		}

		if err = s.db.UpdateChannel(ctx, channel); err != nil {
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}
		return ourSignature, nil
	default:
		return nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	return nil, nil
}

func (s *Service) discoverChannel(channelAddr *address.Address) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	tx, acc, err := s.ton.GetLastTransaction(ctx, channelAddr)
	if err != nil {
		log.Debug().Err(err).Str("address", channelAddr.String()).Msg("failed to get last transaction")
		return false
	}

	if tx == nil {
		log.Debug().Str("address", channelAddr.String()).Msg("no transactions at requested unknown account")
		return false
	}

	ch, err := s.channelClient.ParseAsyncChannel(channelAddr, acc.Code, acc.Data, true)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to parse channel")
		return false
	}

	if ch.Status == payments.ChannelStatusUninitialized {
		return false
	}

	log.Info().Str("address", channelAddr.String()).Msg("discovered channel, scheduling check")
	s.updates <- ChannelUpdatedEvent{
		Transaction: tx,
		Channel:     ch,
	}

	return true
}
