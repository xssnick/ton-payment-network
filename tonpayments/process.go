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
	"math/big"
	"reflect"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Tunnel channel flow:
// 1. User calls ProposeAction with add conditional to node (action must be added before)
// 2. Node triggers ProcessAction and then ProposeAction to receiver
// 3. Receiver triggers ProcessAction, now channel is open
//
// 1. When receiver wants to execute (close) conditional, he calls RequestActions using ExecuteConditionalAction with channel and state
// 2. Node triggers ProcessActionRequest, validates data, and responds with ProposeAction with "execute conditional"
//		and hash of state with removed condition and transferred coins.
// 3. Receiver triggers ProcessAction and approves
// 4. Node in chain repeats this steps in background, if it is not initial sender.

func (s *Service) ProcessAction(ctx context.Context, key ed25519.PublicKey, lockId int64, channelAddr *address.Address,
	proposedState payments.StateBodySigned, action transport.Action, fromWeb bool) (*payments.StateBodySigned, error) {
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

	if channel.Status == db.ChannelStateInactive {
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

	if !bytes.Equal(key, channel.Their.OnchainInfo.Key) {
		return nil, fmt.Errorf("incorrect channel key")
	}

	currentState := channel.LoadSignedState()

	var theirSideProposal, ourSideProposal payments.StateSide
	var keyA, keyB ed25519.PublicKey
	if channel.WeLeft {
		keyB = channel.Their.OnchainInfo.Key
		theirSideProposal = proposedState.Body.B
		ourSideProposal = proposedState.Body.A
	} else {
		keyA = channel.Their.OnchainInfo.Key
		theirSideProposal = proposedState.Body.A
		ourSideProposal = proposedState.Body.B
	}

	// verify their side signature
	if err = proposedState.Verify(keyA, keyB); err != nil {
		return nil, fmt.Errorf("failed to verify passed state: %w", err)
	}

	var toExecute func(ctx context.Context) error
	if proposedState.Body.Seqno == currentState.Body.Seqno {
		// idempotency check, set his signature to our state and verify
		if channel.WeLeft {
			currentState.SignatureB = proposedState.SignatureB
		} else {
			currentState.SignatureA = proposedState.SignatureA
		}
		if err = currentState.Verify(channel.SideA().OnchainInfo.Key, channel.SideB().OnchainInfo.Key); err != nil {
			log.Debug().Str("a_actions_hash", base64.StdEncoding.EncodeToString(currentState.Body.A.ActionStatesHash)).
				Str("b_actions_hash", base64.StdEncoding.EncodeToString(currentState.Body.B.ActionStatesHash)).
				Str("a_cond_hash", base64.StdEncoding.EncodeToString(currentState.Body.A.ConditionalsHash)).
				Str("b_cond_hash", base64.StdEncoding.EncodeToString(currentState.Body.B.ConditionalsHash)).
				Str("seqno", fmt.Sprint(currentState.Body.Seqno)).
				Msg("cur state")

			log.Debug().Str("a_actions_hash", base64.StdEncoding.EncodeToString(proposedState.Body.A.ActionStatesHash)).
				Str("b_actions_hash", base64.StdEncoding.EncodeToString(proposedState.Body.B.ActionStatesHash)).
				Str("a_cond_hash", base64.StdEncoding.EncodeToString(proposedState.Body.A.ConditionalsHash)).
				Str("b_cond_hash", base64.StdEncoding.EncodeToString(proposedState.Body.B.ConditionalsHash)).
				Str("seqno", fmt.Sprint(proposedState.Body.Seqno)).
				Msg("prop state")
			return nil, fmt.Errorf("inconsistent state, seqno %d with different content was already committed: %w", proposedState.Body.Seqno, err)
		}
		return currentState, nil
	}

	if proposedState.Body.Seqno != currentState.Body.Seqno+1 {
		return nil, fmt.Errorf("incorrect state seqno %d, want %d", proposedState.Body.Seqno, currentState.Body.Seqno+1)
	}

	log.Debug().Str("action", reflect.TypeOf(action).String()).Msg("action process")

	switch data := action.(type) {
	case transport.RemoveActionAction:
		return nil, fmt.Errorf("not implemented")
	case transport.IncrementStatesAction:
		toExecute = func(ctx context.Context) error {
			if data.WantResponse {
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Our.Address,
					"increment-state-"+channel.Our.Address+"-"+fmt.Sprint(proposedState.Body.Seqno),
					db.IncrementStatesTask{
						ChannelAddress: channel.Our.Address,
						WantResponse:   false,
					}, nil, nil,
				)
				if err != nil {
					return fmt.Errorf("failed to create increment-state task: %w", err)
				}
			}

			if channel.SignedState == nil {
				log.Info().Str("address", channel.Our.Address).
					Str("with", base64.StdEncoding.EncodeToString(channel.Their.OnchainInfo.Key)).
					Msg("channel states exchanged, ready to use")
			}
			return nil
		}
	case transport.RemoveConditionalAction:
		idxOut, vchOut, err := payments.FindConditional(ctx, channel.Our.Data.Conditionals, data.ID, s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				break
			}
			return nil, fmt.Errorf("failed to find virtual channel in our prev state: %w", err)
		}

		var idxIn *cell.Cell

		meta, err := s.db.GetVirtualChannelMeta(ctx, vchOut.GetKey())
		if err != nil {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if _, ok := vchOut.(*conditionals.ConditionalResolvable); ok {
			if err = s.ensureDerivativeRemovable(ctx, meta); err != nil {
				return nil, fmt.Errorf("failed to remove derivative conditional: %w", err)
			}
		}

		if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) > 0 {
			linkedMeta, err := s.db.GetVirtualChannelMeta(ctx, meta.Outgoing.LinkedKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load linked virtual channel meta: %w", err)
			}

			if linkedMeta.Incoming != nil {
				idxIn, _, err = payments.FindConditional(ctx, channel.Their.Data.Conditionals, linkedMeta.Incoming.Conditional.Hash(), s)
				if err != nil && !errors.Is(err, payments.ErrNotFound) {
					return nil, fmt.Errorf("failed to find linked virtual channel: %w", err)
				}
			}
		}

		if err = channel.Our.Data.Conditionals.Delete(idxOut); err != nil {
			return nil, fmt.Errorf("failed to remove condition: %w", err)
		}
		if idxIn != nil {
			if err = channel.Their.Data.Conditionals.Delete(idxIn); err != nil {
				return nil, fmt.Errorf("failed to remove linked condition: %w", err)
			}
		}

		toExecute = func(ctx context.Context) error {
			if err = s.scheduleDerivativeHedgeClose(ctx, meta, db.ConditionalStateRemoved); err != nil {
				return fmt.Errorf("failed to schedule derivative hedge remove webhook: %w", err)
			}
			if meta.Incoming != nil {
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "remove-cond", meta.Incoming.ChannelAddress,
					"remove-cond-"+base64.StdEncoding.EncodeToString(meta.Key),
					db.RemoveConditionalTask{
						Key: meta.Key,
					}, nil, nil,
				)
				if err != nil {
					return fmt.Errorf("failed to create remove-cond task: %w", err)
				}
			}
			log.Info().Str("id", base64.StdEncoding.EncodeToString(data.ID)).Msg("our conditional removed (and linked if present)")
			return nil
		}
	case transport.ExecuteConditionalAction:
		idxOut, condOut, err := payments.FindConditional(ctx, channel.Our.Data.Conditionals, data.ID, s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				break
			}
			return nil, fmt.Errorf("failed to find virtual channel in our prev state: %w", err)
		}

		meta, err := s.db.GetVirtualChannelMeta(ctx, condOut.GetKey())
		if err != nil {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		}

		if err = meta.AddKnownResolve(ctx, condOut, data.State, false); err != nil {
			return nil, fmt.Errorf("failed to add resolve: %w", err)
		}

		// Check for linked conditional (Atomic Derivative)
		var idxIn *cell.Cell // Assuming key is *cell.Cell
		var condIn payments.Conditional

		if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) > 0 {
			linkedMeta, err := s.db.GetVirtualChannelMeta(ctx, meta.Outgoing.LinkedKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load linked virtual channel meta: %w", err)
			}

			// Find linked in Their conditionals
			if linkedMeta.Incoming != nil {
				var k *cell.Cell
				var c payments.Conditional
				k, c, err = payments.FindConditional(ctx, channel.Their.Data.Conditionals, linkedMeta.Incoming.Conditional.Hash(), s)
				if err != nil && !errors.Is(err, payments.ErrNotFound) {
					return nil, fmt.Errorf("failed to find linked conditional: %w", err)
				}
				if c != nil {
					idxIn = k
					condIn = c
				}
			}
		}

		// Execute Our Outgoing
		actIdOut := condOut.GetAction().IDCell()
		actStateOut, err := channel.Our.Data.ActionStates.LoadValue(actIdOut)
		if err != nil {
			return nil, fmt.Errorf("failed to load action state (out): %w", err)
		}

		newActStateOut, err := executeConditionalSettlement(condOut, actStateOut.MustToCell(), data.State, channel.Our.LockedDeposits)
		if err != nil {
			return nil, fmt.Errorf("failed to execute condition: %w", err)
		}

		if err = channel.Our.Data.ActionStates.Set(actIdOut, newActStateOut); err != nil {
			return nil, fmt.Errorf("failed to set action state: %w", err)
		}

		// Execute Their Linked (if exists)
		if condIn != nil {
			if linkedResolvable, ok := condIn.(*conditionals.ConditionalResolvable); ok {
				// Derivative: only execute the linked side if its settle > 0
				linkedResolve, err := computeLinkedDerivativeSettle(ctx, data.State, linkedResolvable)
				if err != nil {
					return nil, fmt.Errorf("failed to compute linked derivative settle: %w", err)
				}

				if linkedResolve != nil {
					actIdIn := condIn.GetAction().IDCell()
					actStateIn, err := channel.Their.Data.ActionStates.LoadValue(actIdIn)
					if err != nil {
						return nil, fmt.Errorf("failed to load action state (in): %w", err)
					}

					newActStateIn, err := condIn.Execute(actStateIn.MustToCell(), linkedResolve, channel.Their.LockedDeposits)
					if err != nil {
						return nil, fmt.Errorf("failed to execute linked condition: %w", err)
					}

					if err = channel.Their.Data.ActionStates.Set(actIdIn, newActStateIn); err != nil {
						return nil, fmt.Errorf("failed to set action state (in): %w", err)
					}
				}
			} else {
				// Non-derivative linked conditional: execute with same state
				actIdIn := condIn.GetAction().IDCell()
				actStateIn, err := channel.Their.Data.ActionStates.LoadValue(actIdIn)
				if err != nil {
					return nil, fmt.Errorf("failed to load action state (in): %w", err)
				}

				newActStateIn, err := condIn.Execute(actStateIn.MustToCell(), data.State, channel.Their.LockedDeposits)
				if err != nil {
					return nil, fmt.Errorf("failed to execute linked condition: %w", err)
				}

				if err = channel.Their.Data.ActionStates.Set(actIdIn, newActStateIn); err != nil {
					return nil, fmt.Errorf("failed to set action state (in): %w", err)
				}
			}

			if err = channel.Their.Data.Conditionals.Delete(idxIn); err != nil {
				return nil, fmt.Errorf("failed to remove condition (in): %w", err)
			}
		}

		if err = channel.Our.Data.Conditionals.Delete(idxOut); err != nil {
			return nil, fmt.Errorf("failed to remove condition (out): %w", err)
		}
		if err = channel.Our.Data.ActionStates.Set(actIdOut, newActStateOut); err != nil {
			return nil, fmt.Errorf("failed to set action state (out): %w", err)
		}

		balanceDiff, err := condOut.GetAction().StatesDiff(actStateOut.MustToCell(), newActStateOut)
		if err != nil {
			return nil, fmt.Errorf("failed to calc balance diff: %w", err)
		}

		evData := db.ChannelHistoryActionTransferOutData{
			Amounts: s.formatDiff(balanceDiff),
			To:      meta.FinalDestination,
		}

		jsonData, err := json.Marshal(evData)
		if err != nil {
			log.Error().Err(err).Msg("failed to marshal event data")
		}

		toExecute = func(ctx context.Context) error {
			// TODO: another event for linked conditional
			if err = s.db.CreateChannelEvent(ctx, channel, meta.UpdatedAt, db.ChannelHistoryItem{
				Action: db.ChannelHistoryActionTransferOut,
				Data:   jsonData,
			}); err != nil {
				return fmt.Errorf("failed to create channel event: %w", err)
			}

			if s.webhook != nil {
				if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, meta); err != nil {
					return fmt.Errorf("failed to push virtual channel open event: %w", err)
				}
			}

			if meta.Incoming == nil {
				if err = s.db.ClosePairMeta(ctx, meta.Key, db.ConditionalStateClosed); err != nil {
					return fmt.Errorf("failed to close virtual channel pair meta: %w", err)
				}
				meta.Status = db.ConditionalStateClosed
				meta.UpdatedAt = time.Now()
			} else {
				meta.UpdatedAt = time.Now()
				if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to update virtual channel meta: %w", err)
				}
			}
			if err = s.scheduleDerivativeHedgeClose(ctx, meta, db.ConditionalStateClosed); err != nil {
				return fmt.Errorf("failed to schedule derivative hedge close webhook: %w", err)
			}

			if meta.Incoming != nil {
				if err = s.closeConditional(ctx, meta); err != nil {
					return fmt.Errorf("failed to close next conditional: %w", err)
				}
				if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to save closing virtual channel meta: %w", err)
				}
			}

			log.Info().Str("key", base64.StdEncoding.EncodeToString(meta.Key)).Fields(s.repackDiffForLogs(evData.Amounts)).
				Msg("our conditional executed, sent amounts")

			return nil
		}
	case transport.CommitVirtualAction:
		idx, cond, err := payments.FindConditional(ctx, channel.Their.Data.Conditionals, data.ID, s)
		if err != nil {
			return nil, fmt.Errorf("failed to find virtual channel in their old state: %w", err)
		}

		newCond, err := payments.CodeToConditional(ctx, data.UpdatedConditional, s)
		if err != nil {
			return nil, fmt.Errorf("failed to decode updated conditional: %w", err)
		}

		act := cond.GetAction()
		actIdx := act.IDCell()

		actStateOld, err := channel.Their.Data.ActionStates.LoadValue(actIdx)
		if err != nil {
			return nil, fmt.Errorf("failed to load new action state: %w", err)
		}

		actStateUpdated, err := cond.Commit(newCond, actStateOld.MustToCell())
		if err != nil {
			return nil, fmt.Errorf("failed to commit new action state: %w", err)
		}

		// even after updated we keep index as hash of initial cond
		if err = channel.Their.Data.Conditionals.Set(idx, cond.Serialize()); err != nil {
			return nil, fmt.Errorf("failed to set condition: %w", err)
		}

		if err = channel.Their.Data.ActionStates.Set(actIdx, actStateUpdated); err != nil {
			return nil, fmt.Errorf("failed to set action: %w", err)
		}

		toExecute = func(ctx context.Context) error {
			if bytes.Equal(actStateUpdated.Hash(), actStateOld.MustToCell().Hash()) {
				// nothing changed
				return nil
			}

			log.Info().Fields(cond.GetLogInfo()).
				Msg("condition committed")

			return nil
		}
	case transport.AddConditionalAction:
		var aResolver payments.FullResolver = s
		if data.NewActionCode != nil {
			sr, err := s.addActionToChannel(ctx, channel, data.NewActionCode)
			if err == nil {
				aResolver = sr
			} else if !errors.Is(err, ErrActionAlreadyExists) {
				return nil, fmt.Errorf("failed to add action to channel: %w", err)
			}
		}

		cond, err := payments.CodeToConditional(ctx, data.Conditional, aResolver)
		if err != nil {
			return nil, fmt.Errorf("failed to decode updated conditional: %w", err)
		}

		srz := cond.Serialize()
		if !bytes.Equal(data.Conditional.Hash(), srz.Hash()) {
			return nil, fmt.Errorf("incorrect conditional")
		}

		if safe := cond.GetDeadline().UTC().Unix() - (time.Now().UTC().Unix() + channel.SafeOnchainClosePeriod); safe < int64(s.cfg.MinSafeVirtualChannelTimeoutSec) {
			return nil, fmt.Errorf("safe conditional deadline is less than acceptable: %d, %d", safe, s.cfg.MinSafeVirtualChannelTimeoutSec)
		}

		_, _, err = payments.FindConditional(ctx, channel.Their.Data.Conditionals, srz.Hash(), aResolver)
		if err != nil && !errors.Is(err, payments.ErrNotFound) {
			return nil, fmt.Errorf("failed to lookup conditional in their prev state: %w", err)
		}
		if err == nil {
			// idempotency
			break
		}

		if err = cond.ValidateOnAdd(); err != nil {
			return nil, fmt.Errorf("failed to validate condition: %w", err)
		}
		if _, ok := cond.(*conditionals.ConditionalResolvable); ok && !s.cfg.AcceptingDerivatives {
			return nil, fmt.Errorf("derivatives are not accepted on this node")
		}
		if err = s.validateIncomingAction(cond.GetAction()); err != nil {
			return nil, err
		}

		// we will not accept conditional with already used key
		if _, err = s.db.GetVirtualChannelMeta(ctx, cond.GetKey()); err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("failed to load virtual channel meta: %w", err)
		} else if err == nil {
			return nil, fmt.Errorf("this conditional key %s was already used before", base64.StdEncoding.EncodeToString(cond.GetKey()))
		}

		id := cell.BeginCell().MustStoreSlice(srz.Hash(), 256).EndCell()
		// we put our serialized condition to make sure that party is not cheated,
		// if something diff will be in state, final signature will not match
		if err = channel.Their.Data.Conditionals.Set(id, srz); err != nil {
			return nil, fmt.Errorf("failed to settle condition with index %s: %w", hex.EncodeToString(srz.Hash()), err)
		}

		theirBalance, err := channel.CalcBalance(ctx, true, aResolver)
		if err != nil {
			return nil, fmt.Errorf("failed to calc other side balance: %w", err)
		}

		currentInstruction, err := data.DecryptOurInstruction(ctx, s.key, data.InstructionKey, aResolver)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt instruction: %w", err)
		}

		if currentInstruction.ExpectedDeadline != cond.GetDeadline().Unix() {
			return nil, fmt.Errorf("incorrect deadline %d, not equals to expected %d", cond.GetDeadline().Unix(), currentInstruction.ExpectedDeadline)
		}

		isFinalDest := bytes.Equal(currentInstruction.NextTarget, s.key.Public().(ed25519.PublicKey))

		err = cond.CheckInstruction(currentInstruction.Details, isFinalDest, theirBalance, currentInstruction.FinalState)
		if err != nil {
			return nil, fmt.Errorf("failed to check instruction: %w", err)
		}

		removeAfterTimeout := func(ctx context.Context) error {
			condKey := cond.GetKey()

			// try to remove conditional in future if it will time out
			dl := time.Unix(cond.GetDeadline().Unix()+1, 0)
			err = s.db.CreateTask(ctx, PaymentsTaskPool, "remove-cond", channel.Our.Address,
				"remove-cond-"+base64.StdEncoding.EncodeToString(condKey)+"-timeout",
				db.RemoveConditionalTask{
					Key: condKey,
				}, &dl, nil,
			)
			if err != nil {
				return fmt.Errorf("failed to create remove-cond task: %w", err)
			}
			return nil
		}

		if aResolver != s {
			// new action
			if err = s.SaveAction(ctx, cond.GetAction()); err != nil {
				return nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		if res, ok := cond.(*conditionals.ConditionalResolvable); ok {
			if !res.IsInitiator {
				return nil, fmt.Errorf("resolvable condition must be initiated by proposer side")
			}

			linkedCond, err := s.buildLinkedDerivativeConditional(res)
			if err != nil {
				return nil, fmt.Errorf("failed to build linked derivative condition: %w", err)
			}

			// linked key must also be globally unique
			if _, err = s.db.GetVirtualChannelMeta(ctx, linkedCond.GetKey()); err != nil && !errors.Is(err, db.ErrNotFound) {
				return nil, fmt.Errorf("failed to load linked virtual channel meta: %w", err)
			} else if err == nil {
				return nil, fmt.Errorf("this linked conditional key %s was already used before", base64.StdEncoding.EncodeToString(linkedCond.GetKey()))
			}

			if _, err = ensureConditionalOnSide(&channel.Our, linkedCond); err != nil {
				return nil, fmt.Errorf("failed to add linked derivative condition: %w", err)
			}

			linkedActionAdded, err := ensureActionStateOnSide(&channel.Our, linkedCond.GetAction())
			if err != nil {
				return nil, fmt.Errorf("failed to init linked derivative action state: %w", err)
			}

			if linkedActionAdded {
				if err = s.SaveAction(ctx, linkedCond.GetAction()); err != nil {
					return nil, fmt.Errorf("failed to save linked derivative action: %w", err)
				}
			}
			if s.derivativesHedgingEnabled() {
				if err = s.requestDerivativeHedgeOpen(ctx, channel, res, linkedCond); err != nil {
					return nil, fmt.Errorf("derivative hedge webhook rejected order: %w", err)
				}
			}

			toExecute = func(ctx context.Context) error {
				meta := &db.ConditionalMeta{
					Key:    cond.GetKey(),
					Status: db.ConditionalStateActive,
					Incoming: &db.ConditionalMetaSide{
						ChannelAddress:        channel.Our.Address,
						Conditional:           cond.Serialize(),
						UncooperativeDeadline: cond.GetDeadline(),
						SafeDeadline:          cond.GetDeadline().Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						SenderKey:             data.InstructionKey,
						LinkedKey:             linkedCond.GetKey(),
					},
					SpecialDetails: buildDerivativeMetaAny(res.Details, currentInstruction.Details),
					CreatedAt:      time.Now(),
					UpdatedAt:      time.Now(),
				}

				// Derivatives have no real deadline (year 3000); skip removeAfterTimeout.
				// Positions are closed via explicit ClosePosition or liquidation worker.

				if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to create virtual channel meta: %w", err)
				}

				metaReciprocal := &db.ConditionalMeta{
					Key:    linkedCond.GetKey(),
					Status: db.ConditionalStateActive,
					Outgoing: &db.ConditionalMetaSide{
						ChannelAddress:        channel.Our.Address,
						Conditional:           linkedCond.Serialize(),
						UncooperativeDeadline: cond.GetDeadline(),
						SafeDeadline:          cond.GetDeadline().Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						SenderKey:             data.InstructionKey,
						LinkedKey:             cond.GetKey(),
					},
					SpecialDetails: buildDerivativeMetaAny(linkedCond.Details, currentInstruction.Details),
					CreatedAt:      time.Now(),
					UpdatedAt:      time.Now(),
				}

				if err = s.db.CreateVirtualChannelMeta(ctx, metaReciprocal); err != nil {
					return fmt.Errorf("failed to create linked virtual channel meta: %w", err)
				}

				if s.webhook != nil {
					if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, meta); err != nil {
						return fmt.Errorf("failed to push virtual channel open event: %w", err)
					}
				}

				log.Info().
					Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).
					Str("linked_key", base64.StdEncoding.EncodeToString(linkedCond.GetKey())).
					Msg("derivative conditional pair added")
				return nil
			}
		} else if !isFinalDest {
			maxNextDeadline := cond.GetDeadline().Unix() - (channel.SafeOnchainClosePeriod + int64(s.cfg.MinSafeVirtualChannelTimeoutSec))
			if currentInstruction.NextDeadline > maxNextDeadline {
				return nil, fmt.Errorf("next deadline too late (not enough safety gap)")
			}

			targetChannels, err := s.db.GetChannels(context.Background(), currentInstruction.NextTarget, db.ChannelStateAny)
			if err != nil {
				return nil, fmt.Errorf("failed to get target channel: %w", err)
			}

			if len(targetChannels) == 0 {
				return nil, fmt.Errorf("destination channel is not belongs to this node")
			}

			var target *db.Channel
			var targetNoBalance *db.Channel
			var lowestScore *big.Int
			for _, targetChannel := range targetChannels {
				if targetChannel.Status != db.ChannelStateActive {
					continue
				}

				balance, err := targetChannel.CalcBalance(ctx, false, s)
				if err != nil {
					return nil, fmt.Errorf("failed to calc our channel %s balance: %w", targetChannel.Our.Address, err)
				}

				score, err := cond.ScoreTunnelTarget(currentInstruction.Details, balance)
				if err != nil {
					return nil, fmt.Errorf("failed to check target: %w", err)
				}

				if score.Sign() >= 0 {
					// balance is enough to tunnel
					target = targetChannel
					break
				}

				if lowestScore == nil || score.Cmp(lowestScore) < 0 {
					// if we already have a credited channel, it is better to use it again for less onchain actions
					targetNoBalance = targetChannel
					lowestScore = score // < 0 score means credit amount on a channel
				}
			}

			if target == nil {
				if targetNoBalance == nil {
					return nil, fmt.Errorf("no active channel with %s to tunnel requested capacity", base64.StdEncoding.EncodeToString(currentInstruction.NextTarget))
				}
				target = targetNoBalance // we will tunnel and topup to get a resolve
			}

			a, b := address.MustParseAddr(target.Our.Address), address.MustParseAddr(target.Their.Address)
			if !target.WeLeft {
				a, b = b, a
			}

			log.Debug().Str("target", target.Our.Address).Str("a", a.String()).Str("b", b.String()).Msg("channel tunnelling requested")

			nextAction, err := cond.GetAction().PrepareNext(ctx, a, b)
			if err != nil {
				return nil, fmt.Errorf("failed to prepare next action: %w", err)
			}

			nextDeadline := time.Unix(currentInstruction.NextDeadline, 0)
			next, err := cond.PrepareNext(currentInstruction.Details, nextAction, nextDeadline)
			if err != nil {
				return nil, fmt.Errorf("failed to prepare next conditional: %w", err)
			}

			if _, err = target.Our.Data.ActionStates.LoadValue(nextAction.IDCell()); err != nil {
				if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
					return nil, fmt.Errorf("failed to load next action state: %w", err)
				}
				data.NewActionCode = nextAction.Serialize()
			}

			senderKey := data.InstructionKey
			data.InstructionKey = currentInstruction.NextInstructionKey
			data.Conditional = next.Serialize()

			// we will execute it only after all checks passed and final signature verified
			toExecute = func(ctx context.Context) error {
				err = s.db.CreateTask(ctx, PaymentsTaskPool, "create-send-conditional", target.Our.Address,
					"create-send-conditional-"+base64.StdEncoding.EncodeToString(cond.GetKey()),
					db.AddConditionalTask{
						SenderKey:          senderKey,
						PrevChannelAddress: channel.Our.Address,
						PrevConditionalID:  srz.Hash(),
						ChannelAddress:     target.Our.Address,
						Deadline:           currentInstruction.NextDeadline,
						TransportAction:    data,
					}, nil, &nextDeadline,
				)
				if err != nil {
					return fmt.Errorf("failed to create open-virtual task: %w", err)
				}

				if err = removeAfterTimeout(ctx); err != nil {
					return err
				}

				log.Info().Str("target", target.Our.Address).
					Fields(cond.GetLogInfo()).
					Msg("channel tunnelling through us requested")

				return nil
			}
		} else {
			toExecute = func(ctx context.Context) error {
				meta := &db.ConditionalMeta{
					Key:    cond.GetKey(),
					Status: db.ConditionalStateActive,
					Incoming: &db.ConditionalMetaSide{
						ChannelAddress:        channel.Our.Address,
						Conditional:           cond.Serialize(),
						UncooperativeDeadline: cond.GetDeadline(),
						SafeDeadline:          cond.GetDeadline().Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						SenderKey:             data.InstructionKey,
					},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				if currentInstruction.FinalState != nil {
					if err = meta.AddKnownResolve(ctx, cond, currentInstruction.FinalState, false); err != nil {
						return fmt.Errorf("failed to add channel condition resolve: %w", err)
					}

					tryTill := cond.GetDeadline()
					if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-next-virtual", channel.Our.Address,
						"close-next-"+base64.StdEncoding.EncodeToString(cond.GetKey()),
						db.CloseNextVirtualTask{
							VirtualKey: cond.GetKey(),
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
					if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, meta); err != nil {
						return fmt.Errorf("failed to push virtual channel close event: %w", err)
					}
				}

				log.Info().Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).
					Fields(cond.GetLogInfo()).
					Msg("conditional created with us")

				return nil
			}
		}
	case transport.SwapAction:
		if s.onSwap == nil {
			return nil, fmt.Errorf("swaps are not enabled on this node")
		}

		fromCC, err := s.ResolveCoinConfig(hex.EncodeToString(data.FromBalanceID))
		if err != nil {
			return nil, fmt.Errorf("failed to resolve from coin config: %w", err)
		}

		toCC, err := s.ResolveCoinConfig(hex.EncodeToString(data.ToBalanceID))
		if err != nil {
			return nil, fmt.Errorf("failed to resolve to coin config: %w", err)
		}

		fromAmt := fromCC.MustAmount(new(big.Int).SetBytes(data.FromAmount))
		toAmt := toCC.MustAmount(new(big.Int).SetBytes(data.ToAmount))

		fromAct, err := actions.NewSendActionFromBalanceID(ctx, fromCC, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create send action from 'from' balance id: %w", err)
		}

		toAct, err := actions.NewSendActionFromBalanceID(ctx, toCC, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create send action from 'to' balance id: %w", err)
		}

		saveOurAction, saveTheirAction := false, false

		theirState, err := channel.Their.Data.ActionStates.LoadValue(fromAct.IDCell())
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return nil, fmt.Errorf("failed to load from action state: %w", err)
			}
			saveTheirAction = true
			theirState = fromAct.GetEmptyState().MustBeginParse()
		}
		ourState, err := channel.Our.Data.ActionStates.LoadValue(toAct.IDCell())
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				return nil, fmt.Errorf("failed to load to action state: %w", err)
			}
			saveOurAction = true
			ourState = toAct.GetEmptyState().MustBeginParse()
		}

		newTheirState, err := fromAct.AddCoins(theirState.MustToCell(), fromAmt.Nano(), channel.Their.LockedDeposits)
		if err != nil {
			return nil, fmt.Errorf("failed to add coins to their action state: %w", err)
		}
		newOurState, err := toAct.AddCoins(ourState.MustToCell(), toAmt.Nano(), channel.Our.LockedDeposits)
		if err != nil {
			return nil, fmt.Errorf("failed to add coins to our action state: %w", err)
		}

		if err = channel.Their.Data.ActionStates.Set(fromAct.IDCell(), newTheirState); err != nil {
			return nil, fmt.Errorf("failed to set their action state: %w", err)
		}
		if err = channel.Our.Data.ActionStates.Set(toAct.IDCell(), newOurState); err != nil {
			return nil, fmt.Errorf("failed to set our action state: %w", err)
		}

		resolver := tmpFullResolver{[]payments.Action{fromAct, toAct}, s}

		theirBalance, err := channel.CalcBalance(ctx, true, resolver)
		if err != nil {
			return nil, fmt.Errorf("failed to calc their balance: %w", err)
		}
		if b := theirBalance[fromCC.BalanceID]; b == nil || b.Available().Sign() < 0 {
			return nil, fmt.Errorf("not enough funds on their balance")
		}

		ourBalance, err := channel.CalcBalance(ctx, false, resolver)
		if err != nil {
			return nil, fmt.Errorf("failed to calc our balance: %w", err)
		}
		if b := ourBalance[toCC.BalanceID]; b == nil || b.Available().Sign() < 0 {
			return nil, fmt.Errorf("not enough funds on our balance")
		}

		// ask to approve this swap in user-space logic
		if err = s.onSwap(ctx, channel, fromCC, toCC, fromAmt, toAmt); err != nil {
			return nil, fmt.Errorf("swap is not allowed: %w", err)
		}

		if saveOurAction {
			if err = s.SaveAction(ctx, toAct); err != nil {
				return nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		if saveTheirAction {
			if err = s.SaveAction(ctx, fromAct); err != nil {
				return nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		toExecute = func(ctx context.Context) error {
			log.Info().Str("addr", channel.Our.Address).
				Str("from", fromAmt.String()+" "+fromCC.Symbol).Str("to", toAmt.String()+" "+toCC.Symbol).
				Msg("swap confirmed")

			return nil
		}
	case transport.CooperativeCommitAction:
		if channel.PendingCommit != nil {
			return nil, fmt.Errorf("can't execute action while there is already pending commit")
		}
		if len(channel.Their.PendingOnchainTransfers) > 0 || len(channel.Our.PendingOnchainTransfers) > 0 {
			return nil, fmt.Errorf("can't execute action while there are pending onchian transfers")
		}

		var hasFee *bool
		if data.WithFee {
			fromUs := false
			hasFee = &fromUs
		}

		req, ourPending, theirPending, _, err := s.getCommitRequest(ctx, channel, data.ActionID, data.WithFee, hasFee)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare execute action channel request: %w", err)
		}

		toSign, err := tlb.ToCell(req.Signed)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize pending commit message: %w", err)
		}

		if !toSign.Verify(channel.Their.OnchainInfo.Key, data.MsgSignature) {
			return nil, fmt.Errorf("expected msg is not equal to actual")
		}

		if channel.WeLeft {
			req.SignatureB.Value = data.MsgSignature
		} else {
			req.SignatureA.Value = data.MsgSignature
		}

		aid := cell.BeginCell().MustStoreSlice(data.ActionID, 256).EndCell()

		our, their := req.Signed.Action.StateA, req.Signed.Action.StateB
		if !channel.WeLeft {
			our, their = their, our
		}

		if our != nil {
			if err = channel.Our.Data.ActionStates.Set(aid, our); err != nil {
				return nil, fmt.Errorf("failed to set our action state: %w", err)
			}
		}
		if their != nil {
			if err = channel.Their.Data.ActionStates.Set(aid, their); err != nil {
				return nil, fmt.Errorf("failed to set their action state: %w", err)
			}
		}

		channel.PendingCommit = &db.PendingCommit{
			Seqno:   req.Signed.Seqno,
			Message: toSign,
		}

		if ourPending != nil {
			channel.Our.PendingOnchainTransfers[pendingIDCommit(req.Signed.Seqno)] = ourPending
		}
		if theirPending != nil {
			channel.Their.PendingOnchainTransfers[pendingIDCommit(req.Signed.Seqno)] = theirPending
		}

		msg, err := tlb.ToCell(req)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize pending commit message: %w", err)
		}

		toExecute = func(ctx context.Context) error {
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Our.Address,
				"increment-state-"+channel.Our.Address+"-"+fmt.Sprint(proposedState.Body.Seqno),
				db.IncrementStatesTask{
					ChannelAddress: channel.Our.Address,
				}, nil, nil,
			); err != nil {
				return fmt.Errorf("failed to create increment-state task: %w", err)
			}

			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "commit-execute", channel.Our.Address,
				"commit-execute-"+channel.Our.Address+"-"+hex.EncodeToString(toSign.Hash()),
				db.CommitExecuteTask{
					ChannelAddress: channel.Our.Address,
					SignedRequest:  msg,
				}, nil, nil,
			); err != nil {
				return fmt.Errorf("failed to create increment-state task: %w", err)
			}

			log.Info().Str("address", channel.Our.Address).Bool("execute", req.Signed.FromA == channel.WeLeft).Msg("accepted cooperative commit proposal")

			return nil
		}
	case transport.RentCapacityAction:
		bi := hex.EncodeToString(data.BalanceID)
		cc, err := s.ResolveBalanceType(bi)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve balance type %s: %w", bi, err)
		}

		a, err := actions.NewSendActionFromBalanceID(ctx, cc, channel.SideA().Address, channel.SideB().Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create send action: %w", err)
		}

		actId := a.IDCell()
		aState, err := channel.Their.Data.ActionStates.LoadValue(actId)
		if err != nil && !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, fmt.Errorf("failed to load action state: %w", err)
		}

		var saveAction bool
		if aState == nil {
			saveAction = true
			aState = a.GetEmptyState().MustBeginParse()
		}

		amount := new(big.Int).SetBytes(data.Amount)
		till := time.Unix(int64(data.Till), 0)

		amountBefore := big.NewInt(0)
		used := big.NewInt(0)
		ld := channel.Our.LockedDeposits[cc.BalanceID]
		if ld != nil && ld.Till.After(time.Now()) {
			used = new(big.Int).Set(ld.Used)
			amountBefore = new(big.Int).Set(ld.Amount)
		}
		diffAmount := new(big.Int).Sub(amount, amountBefore)

		if time.Until(till) > 366*24*time.Hour {
			// more than one year is too long
			return nil, fmt.Errorf("duration too long")
		}
		if diffAmount.Sign() <= 0 {
			return nil, fmt.Errorf("resulting topup amount must be positive")
		}
		if diffAmount.Cmp(cc.VirtualTunnelConfig.MaxCapacityToRentPerTx.Nano()) > 0 {
			return nil, fmt.Errorf("capacity to rent is too big")
		}

		totalFee := channel.CalcDepositFee(cc, amount, till, false)
		if totalFee.Sign() <= 0 {
			return nil, fmt.Errorf("payment not needed, capacity is already enough")
		}

		theirBalances, err := channel.CalcBalance(ctx, true, s)
		if err != nil {
			return nil, fmt.Errorf("failed to calc their balance: %w", err)
		}
		theirBalance := theirBalances[cc.BalanceID]
		if theirBalance == nil {
			return nil, fmt.Errorf("their balance have no fee coin")
		}

		// calc balance + amount potentially could be received
		usableBalance := new(big.Int).Add(theirBalance.Available(), theirBalance.ConditionalPending)

		if usableBalance.Cmp(totalFee) < 0 {
			return nil, fmt.Errorf("not enough locked+balance %s for fee to rent, usable: %s, fee: %s", cc.Symbol, cc.MustAmount(usableBalance), cc.MustAmount(totalFee))
		}

		var actState actions.StateActionSend
		if err = payments.LoadState(&actState, aState.MustToCell()); err != nil {
			return nil, fmt.Errorf("failed to load action state: %w", err)
		}
		actState.Amount.Val = new(big.Int).Add(actState.Amount.Nano(), totalFee)

		updatedState, err := tlb.ToCell(actState)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize updated action state: %w", err)
		}

		if err := channel.Their.Data.ActionStates.Set(actId, updatedState); err != nil {
			return nil, fmt.Errorf("failed to set condition: %w", err)
		}

		channel.Our.LockedDeposits[cc.BalanceID] = &payments.LockedDepositInfo{
			Amount: amount,
			Till:   till,
			Used:   used,
		}

		if saveAction {
			if err = s.SaveAction(ctx, a); err != nil {
				return nil, fmt.Errorf("failed to save action: %w", err)
			}
		}

		// we will execute it only after all checks passed and final signature verified
		toExecute = func(ctx context.Context) error {
			evData := db.ChannelHistoryActionRentCapData{
				BalanceID: cc.BalanceID,
				Amount:    amount.String(),
				Fee:       totalFee.String(),
				Till:      till.Unix(),
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
				Str("paid", cc.MustAmount(totalFee).String()).
				Str("channel", channel.Our.Address).
				Msg("inbound liquidity purchased from us")

			return nil
		}
	default:
		return nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}

	theirActHash, ourActHash := make([]byte, 32), make([]byte, 32)
	theirCondHash, ourCondHash := make([]byte, 32), make([]byte, 32)
	if !channel.Their.Data.Conditionals.IsEmpty() {
		theirCondHash = channel.Their.Data.Conditionals.AsCell().Hash()
	}
	if !channel.Our.Data.Conditionals.IsEmpty() {
		ourCondHash = channel.Our.Data.Conditionals.AsCell().Hash()
	}
	if !channel.Their.Data.ActionStates.IsEmpty() {
		theirActHash = channel.Their.Data.ActionStates.AsCell().Hash()
	}
	if !channel.Our.Data.ActionStates.IsEmpty() {
		ourActHash = channel.Our.Data.ActionStates.AsCell().Hash()
	}

	if !bytes.Equal(theirCondHash, theirSideProposal.ConditionalsHash) {
		return nil, fmt.Errorf("incorrect their resulting conditionals hash")
	}
	if !bytes.Equal(theirActHash, theirSideProposal.ActionStatesHash) {
		return nil, fmt.Errorf("incorrect their resulting actions hash %s != %s",
			hex.EncodeToString(theirActHash), hex.EncodeToString(theirSideProposal.ActionStatesHash))
	}
	if !bytes.Equal(ourCondHash, ourSideProposal.ConditionalsHash) {
		return nil, fmt.Errorf("incorrect our resulting conditionals hash")
	}
	if !bytes.Equal(ourActHash, ourSideProposal.ActionStatesHash) {
		return nil, fmt.Errorf("incorrect our resulting actions hash")
	}

	toSign, err := tlb.ToCell(proposedState.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize our state for signing: %w", err)
	}

	if channel.WeLeft {
		proposedState.SignatureA = payments.Signature{Value: toSign.Sign(s.key)}
	} else {
		proposedState.SignatureB = payments.Signature{Value: toSign.Sign(s.key)}
	}

	newState, err := tlb.ToCell(proposedState)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize new state: %w", err)
	}

	channel.SignedState = newState
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

	return &proposedState, nil
}

func (s *Service) ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action transport.Action) ([]byte, error) {
	s.mx.Lock()
	defer s.mx.Unlock()

	channel, err := s.GetActiveChannel(ctx, channelAddr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	if !bytes.Equal(channel.Their.OnchainInfo.Key, key) {
		return nil, fmt.Errorf("unauthorized channel")
	}

	if !channel.AcceptingActions {
		return nil, fmt.Errorf("channel is currently not accepting new actions")
	}

	log.Debug().Str("action", reflect.TypeOf(action).String()).Msg("action request process")

	switch data := action.(type) {
	case transport.ExecuteTransactionAction:
		var req payments.ExternalMsgDoubleSigned
		err = tlb.Parse(&req, data.ExternalBody)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize message request: %w", err)
		}

		if !bytes.Equal(req.Signed.ChannelID, channel.ID) {
			return nil, fmt.Errorf("channel id is not match")
		}

		if req.Signed.SideA == channel.WeLeft {
			return nil, fmt.Errorf("execute action must be applied to other side")
		}

		if int64(req.Signed.ValidUntil) < time.Now().Add(90*time.Second).Unix() {
			return nil, fmt.Errorf("execute action must have at least 90 seconds ttl")
		}

		if req.Signed.WalletSeqno != channel.Their.LatestWalletSeqno {
			return nil, fmt.Errorf("to execute action must use last wallet seqno")
		}

		pendingID := pendingIDWallet(req.Signed.WalletSeqno)
		if channel.Their.PendingOnchainTransfers[pendingID] != nil {
			// idempotency
			return nil, nil
		}
		if len(channel.Their.PendingOnchainTransfers) != 0 {
			return nil, fmt.Errorf("to execute action must be no pending onchain transfers")
		}

		var theirSig = req.SignatureA.Value
		if channel.WeLeft {
			theirSig = req.SignatureB.Value
		}

		toSign, err := tlb.ToCell(req.Signed)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize to sign: %w", err)
		}

		if !toSign.Verify(channel.Their.OnchainInfo.Key, theirSig) {
			return nil, fmt.Errorf("invalid signature")
		}

		p, notEnoughOnchain, err := s.validateOutMessages(ctx, channel, req.Signed.OutActions, true)
		if err != nil {
			return nil, fmt.Errorf("failed to validate out messages: %w", err)
		}

		if len(notEnoughOnchain) > 0 {
			return nil, fmt.Errorf("not enough onchain balance")
		}

		if channel.WeLeft {
			req.SignatureA = payments.Signature{Value: toSign.Sign(s.key)}
		} else {
			req.SignatureB = payments.Signature{Value: toSign.Sign(s.key)}
		}

		msg, err := tlb.ToCell(req)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize msg: %w", err)
		}

		err = s.db.Transaction(ctx, func(ctx context.Context) error {
			channel.Their.PendingOnchainTransfers[pendingID] = p

			if err = s.db.UpdateChannel(ctx, channel); err != nil {
				return fmt.Errorf("failed to update channel: %w", err)
			}

			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "exec-tx-external", channel.Our.Address+"-ext-snd",
				"exec-tx-external-"+channel.Their.Address+"-seq-"+fmt.Sprint(req.Signed.WalletSeqno),
				db.ExecuteExternalTxTask{
					ChannelAddress: channel.Our.Address,
					OurSide:        false,
					Body:           msg,
					WalletSeqno:    req.Signed.WalletSeqno,
				}, nil, nil,
			); err != nil {
				return fmt.Errorf("failed to create execute task: %w", err)
			}

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to execute db transaction: %w", err)
		}

		return nil, nil
	case transport.CooperativeCloseAction:
		var req payments.CooperativeClose
		err = tlb.Parse(&req, data.SignedCloseRequest)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize their close channel request: %w", err)
		}

		log.Info().Str("address", channel.Our.Address).Msg("received cooperative close request")

		_, dataCell, ourSignature, err := s.getCooperativeCloseRequest(ctx, channel, !channel.WeLeft)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare close channel request: %w", err)
		}

		var theirSignature = req.SignatureA.Value
		if channel.WeLeft {
			theirSignature = req.SignatureB.Value
		}

		if !dataCell.Verify(channel.Their.OnchainInfo.Key, theirSignature) {
			return nil, fmt.Errorf("incorrect party signature")
		}

		channel.AcceptingActions = false
		if err = s.db.UpdateChannel(ctx, channel); err != nil {
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}

		return ourSignature, nil
	default:
		return nil, fmt.Errorf("unexpected action type: %s", reflect.TypeOf(data).String())
	}
}

func (s *Service) discoverChannel(channelAddr *address.Address) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	tx, acc, err := s.ton.GetLastTransaction(ctx, channelAddr, time.Time{})
	if err != nil {
		log.Debug().Err(err).Str("address", channelAddr.String()).Msg("failed to get last transaction")
		return false
	}

	if tx == nil {
		log.Debug().Str("address", channelAddr.String()).Msg("no transactions at requested unknown account")
		return false
	}

	ch, err := s.channelClient.ParseChannel(channelAddr, acc.Code, acc.Data, true)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr.String()).Msg("failed to parse channel")
		return false
	}

	if ch.Status == payments.ChannelStatusUninitialized {
		return false
	}

	log.Info().Str("address", channelAddr.String()).Msg("discovered channel, scheduling check")
	s.updates <- &ChannelUpdatedEvent{
		Transaction:   tx,
		LatestChannel: ch,
	}

	return true
}

func (s *Service) fetchChannelSideOnchainBalances(ctx context.Context, addr *address.Address, blockAfter time.Time) (map[string]*big.Int, error) {
	acc, err := s.ton.GetAccount(ctx, addr, blockAfter)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	log.Debug().Str("address", addr.String()).Msg("fetching onchain balances")

	balances := map[string]*big.Int{}
	if b := s.knownBalanceTypes[payments.GetTONBalanceID()]; b != nil && b.Enabled {
		// TODO: subtract reserved value?
		balances[payments.GetTONBalanceID()] = acc.Balance.Nano()
	}

	if !acc.ExtraCurrencies.IsEmpty() {
		ecs, err := acc.ExtraCurrencies.LoadAll()
		if err != nil {
			return nil, fmt.Errorf("failed to load extra currencies: %w", err)
		}

		for _, dictKV := range ecs {
			currencyId := payments.GetECBalanceID(uint32(dictKV.Key.MustLoadUInt(32)))
			if b := s.knownBalanceTypes[currencyId]; b != nil && b.Enabled {
				balances[currencyId] = dictKV.Value.MustLoadVarUInt(32)
			}
		}
	}

	for id, bt := range s.knownBalanceTypes {
		if bt.Enabled && bt.JettonClient != nil {
			amt, err := bt.JettonClient.GetBalance(ctx, addr, blockAfter)
			if err != nil {
				return nil, fmt.Errorf("failed to get jetton %s balance: %w", bt.JettonClient.GetRootAddress().String(), err)
			}
			balances[id] = amt
			log.Debug().Str("address", addr.String()).Str("symbol", bt.Symbol).Str("amount", bt.MustAmount(amt).String()).Msg("fetched jetton balance")
		}
	}

	return balances, nil
}

func calcBalancesDiff(before, after map[string]*big.Int) map[string]*big.Int {
	changes := map[string]*big.Int{}
	for s2, b := range after {
		diff := new(big.Int).Set(b)
		prev := before[s2]
		if prev != nil && prev.Sign() > 0 {
			diff.Sub(diff, prev)
		}

		if diff.Sign() != 0 {
			changes[s2] = diff
		}
	}
	return changes
}

type tmpFullResolver struct {
	newActions []payments.Action
	s          *Service
}

func (s tmpFullResolver) GetKnownBalanceTypes() []*payments.CoinConfig {
	return s.s.GetKnownBalanceTypes()
}

func (s tmpFullResolver) ResolveBalanceType(id string) (*payments.CoinConfig, error) {
	return s.s.ResolveCoinConfig(id)
}

func (s tmpFullResolver) ResolveAction(ctx context.Context, id []byte) (payments.Action, error) {
	a, err := s.s.ResolveAction(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			for _, action := range s.newActions {
				if bytes.Equal(id, action.Serialize().Hash()) {
					return action, nil
				}
			}
		}
		return nil, err
	}
	return a, nil
}

var ErrActionAlreadyExists = fmt.Errorf("action already exists")

func (s *Service) addActionToChannel(ctx context.Context, channel *db.Channel, code *cell.Cell) (payments.FullResolver, error) {
	newCodeHash := code.Hash()

	a, err := payments.CodeToAction(ctx, code, s)
	if err != nil {
		return nil, fmt.Errorf("failed to detect action type: %w", err)
	}

	// after reserialization must match
	if !bytes.Equal(a.Serialize().Hash(), newCodeHash) {
		return nil, fmt.Errorf("incorrect action hash")
	}

	_, err = channel.Their.Data.ActionStates.LoadValue(a.IDCell())
	if err != nil {
		if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, fmt.Errorf("failed to find action in proposed state: %w", err)
		}
	} else {
		// already exists
		return nil, ErrActionAlreadyExists
	}

	validateAddrs := func(addrA, addrB *address.Address) error {
		ourAddr, theirAddr := addrA, addrB
		if !channel.WeLeft {
			ourAddr, theirAddr = theirAddr, ourAddr
		}

		if ourAddr.String() != channel.Our.Address || theirAddr.String() != channel.Their.Address {
			return fmt.Errorf("incorrect addresses %s != %s || %s != %s", ourAddr.String(), channel.Our.Address, theirAddr.String(), channel.Their.Address)
		}
		return nil
	}

	switch act := a.(type) {
	case *actions.ActionSendJettonInsured:
		if err = validateAddrs(act.AddressA, act.AddressB); err != nil {
			return nil, fmt.Errorf("failed to validate jetton action addresses: %w", err)
		}
	case *actions.ActionSendTonInsured:
		if err = validateAddrs(act.AddressA, act.AddressB); err != nil {
			return nil, fmt.Errorf("failed to validate ton action addresses: %w", err)
		}
	case *actions.ActionSendECInsured:
		if err = validateAddrs(act.AddressA, act.AddressB); err != nil {
			return nil, fmt.Errorf("failed to validate ec action addresses: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported action type: %T", a)
	}

	if err = channel.Their.Data.ActionStates.Set(a.IDCell(), a.GetEmptyState()); err != nil {
		return nil, fmt.Errorf("failed to set action state: %w", err)
	}

	return tmpFullResolver{[]payments.Action{a}, s}, nil
}

func (s *Service) formatDiff(balances map[string]*big.Int) map[string]string {
	res := map[string]string{}
	for id, b := range balances {
		cc, err := s.ResolveCoinConfig(id)
		if err != nil {
			continue
		}
		res[cc.Symbol] = cc.MustAmount(b).String()
	}
	return res
}

func (s *Service) repackDiffForLogs(balances map[string]string) map[string]any {
	res := make(map[string]any, len(balances))
	for id, b := range balances {
		res[id] = b
	}
	return res
}
