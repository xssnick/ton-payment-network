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
	"math/rand"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var ErrWaitingForCapacity = errors.New("capacity request was sent, waiting for it")
var ErrActionStarted = errors.New("action was started, waiting for completion from other side")
var ErrStillPending = errors.New("still pending")
var WorkerTick = time.Second * 1

func (s *Service) taskExecutor() {
	if s.useMetrics {
		go s.taskMonitor()
	}

	tick := time.Tick(WorkerTick)

	for {
		select {
		case <-s.globalCtx.Done():
			return
		default:
		}

		task, err := s.db.AcquireTask(context.Background(), PaymentsTaskPool)
		if err != nil {
			log.Error().Err(err).Msg("failed to acquire task from db")
			time.Sleep(WorkerTick * 3)
			continue
		}

		if task == nil {
			select {
			case <-s.workerSignal:
			case <-tick:
			}
			continue
		}

		// run each task in own routine, to not block other's execution
		go func() {
			defer s.touchWorker()

			err = func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				switch task.Type {
				case "derivative-hedge-webhook":
					var data derivativeHedgeWebhookTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}
					if err = s.sendDerivativeHedgeWebhook(ctx, data.Request, derivativeHedgeWebhookCloseTimeout); err != nil {
						return err
					}
					return nil
				case "increment-state":
					var data db.IncrementStatesTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					if err := s.proposeAction(ctx, lockId, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: data.WantResponse}, nil); err != nil {
						return fmt.Errorf("failed to increment state with party: %w", err)
					}
				case "close-conditional":
					var data db.CloseConditionalTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load conditional meta: %w", err)
					}

					if meta.Status == db.ConditionalStateClosed || meta.Status == db.ConditionalStateRemoved || meta.Status == db.ConditionalStateWantRemove {
						log.Debug().Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).Msg("is not active, skip closing")
						return nil
					}

					if meta.LastKnownResolve == nil {
						log.Warn().Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).Msg("no last known resolve, cannot close")
						return nil
					}

					if meta.Incoming == nil {
						log.Warn().Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).
							Msg("virtual channel has no incoming side, close task skipped")
						return nil
					}

					channel, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						log.Warn().Str("channel", channel.Our.Address).Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).Msg("onchain channel is not active, cannot close conditional")

						// not needed anymore
						return nil
					}

					condId := meta.Incoming.Conditional.Hash()
					_, cond, err := payments.FindConditional(ctx, channel.Their.Data.Conditionals, condId, s)
					if err != nil {
						if errors.Is(err, payments.ErrNotFound) {
							// already removed
							return nil
						}

						log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to find their conditional")
						return fmt.Errorf("failed to find conditional: %w", err)
					}

					theirBalances, err := channel.CalcBalance(ctx, true, s)
					if err != nil {
						return fmt.Errorf("failed to calc other side balance: %w", err)
					}

					for _, cc := range cond.GetAction().GetAffectedCoins() {
						// TODO: check if we really need this capacity, in case of 2+ affected coins
						if err = s.rentCapacityIfNeeded(ctx, channel, cc, theirBalances); err != nil {
							return err
						}
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, meta.Incoming.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire 'to' channel: %w", err)
					}
					defer unlock()

					err = s.proposeAction(ctx, lockId, meta.Incoming.ChannelAddress, transport.ExecuteConditionalAction{
						ID:    condId,
						State: meta.LastKnownResolve,
					}, meta)
					if err != nil {
						return fmt.Errorf("failed to propose action: %w", err)
					}
					return nil
				case "close-next-virtual":
					var data db.CloseNextVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					if err = s.CloseConditional(ctx, data.VirtualKey); err != nil {
						return fmt.Errorf("failed to request conditional close: %w", err)
					}

					return nil
				case "commit-virtual":
					var data db.CommitVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load conditional meta: %w", err)
					}

					if meta.Status != db.ConditionalStateActive {
						// not needed anymore
						return nil
					}

					_, cond, err := payments.FindConditional(ctx, channel.Our.Data.Conditionals, meta.Outgoing.Conditional.Hash(), s)
					if err != nil {
						if errors.Is(err, payments.ErrNotFound) {
							// no need
							return nil
						}
						return fmt.Errorf("failed to find virtual channel: %w", err)
					}

					if meta.LastKnownResolve == nil {
						// nothing to commit
						return nil
					}

					upd, err := cond.PrepareCommit(meta.LastKnownResolve)
					if err != nil {
						return fmt.Errorf("failed to prepare condition commit: %w", err)
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, transport.CommitVirtualAction{
						ID:                 meta.Outgoing.Conditional.Hash(),
						UpdatedConditional: upd.Serialize(),
					}, nil); err != nil {
						// reversal is not mandatory, because 'sent amount' is atomic with conditional prepay, and no actual balance change
						return fmt.Errorf("failed to propose action: %w", err)
					}
				case "create-send-conditional":
					var data db.AddConditionalTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					var resolver payments.FullResolver = s
					if data.TransportAction.NewActionCode != nil {
						a, err := payments.CodeToAction(ctx, data.TransportAction.NewActionCode, s)
						if err != nil {
							return fmt.Errorf("failed to detect action type: %w", err)
						}
						resolver = tmpFullResolver{[]payments.Action{a}, s}
					}

					cond, err := payments.CodeToConditional(ctx, data.TransportAction.Conditional, resolver)
					if err != nil {
						return fmt.Errorf("failed to parse conditional: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					meta := &db.ConditionalMeta{
						Key:    cond.GetKey(),
						Status: db.ConditionalStatePending,
						Outgoing: &db.ConditionalMetaSide{
							ChannelAddress:        data.ChannelAddress,
							Conditional:           data.TransportAction.Conditional,
							UncooperativeDeadline: time.Unix(data.Deadline, 0),
							SafeDeadline:          time.Unix(data.Deadline, 0).Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						},
						FinalDestination: data.FinalDestinationKey,
						CreatedAt:        time.Now(),
						UpdatedAt:        time.Now(),
					}

					if data.PrevConditionalID != nil {
						prev, err := s.db.GetChannel(ctx, data.PrevChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to get prev channel: %w", err)
						}

						_, prevVch, err := payments.FindConditional(ctx, prev.Their.Data.Conditionals, data.PrevConditionalID, s)
						if err != nil {
							return fmt.Errorf("failed to find prev virtual channel: %w", err)
						}

						meta.Incoming = &db.ConditionalMetaSide{
							SenderKey:             data.SenderKey,
							ChannelAddress:        data.PrevChannelAddress,
							Conditional:           prevVch.Serialize(),
							UncooperativeDeadline: prevVch.GetDeadline(),
							SafeDeadline:          prevVch.GetDeadline().Add(-time.Duration(prev.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						}
					}

					if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
						return fmt.Errorf("failed to create conditional meta: %w", err)
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, data.TransportAction, cond); err != nil {
						if errors.Is(err, ErrDenied) {
							// ensure that state was not modified on the other side by sending newer state without this conditional
							if err := s.proposeAction(ctx, lockId, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: false}, nil); err != nil {
								return fmt.Errorf("failed to increment states on conditional revert: %w", err)
							}

							return s.db.Transaction(ctx, func(ctx context.Context) error {
								meta, err := s.db.GetVirtualChannelMeta(ctx, cond.GetKey())
								if err != nil {
									return fmt.Errorf("failed to load conditional meta: %w", err)
								}

								meta.Status = db.ConditionalStateWantRemove
								meta.UpdatedAt = time.Now()
								if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
									return fmt.Errorf("failed to update conditional meta: %w", err)
								}

								// if we are not the first node of the tunnel
								if data.PrevChannelAddress != "" {
									tryTill := cond.GetDeadline()
									// consider conditional unsuccessful and gracefully removed
									// and notify previous party that we are ready to release locked coins.
									err = s.db.CreateTask(ctx, PaymentsTaskPool, "remove-cond", data.PrevChannelAddress,
										"remove-cond-"+base64.StdEncoding.EncodeToString(meta.Key),
										db.RemoveConditionalTask{
											Key: meta.Key,
										}, nil, &tryTill,
									)
									if err != nil {
										return fmt.Errorf("failed to create remove-cond task: %w", err)
									}
								}
								return nil
							})
						} else if errors.Is(err, ErrNotPossible) {
							// not possible by us, so no revert confirmation needed
							log.Warn().Err(err).Msg("it is not possible to open virtual channel")
							return nil
						}
						return fmt.Errorf("failed to propose actions to the next node: %w", err)
					}

					meta, err = s.db.GetVirtualChannelMeta(ctx, cond.GetKey())
					if err != nil {
						return fmt.Errorf("failed to load conditional meta: %w", err)
					}

					meta.Status = db.ConditionalStateActive
					meta.UpdatedAt = time.Now()
					if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
						return fmt.Errorf("failed to update conditional meta: %w", err)
					}

					if s.webhook != nil {
						if err = s.webhook.PushVirtualChannelEvent(context.Background(), db.VirtualChannelEventTypeOpen, meta); err != nil {
							return fmt.Errorf("failed to push conditional open event: %w", err)
						}
					}

					log.Info().Fields(cond.GetLogInfo()).
						Str("target", data.ChannelAddress).
						Msg("conditional successfully tunnelled through us")
				case "create-derivative-cond":
					var data db.AddDerivativeTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					var resolver payments.FullResolver = s
					if data.TransportAction.NewActionCode != nil {
						a, err := payments.CodeToAction(ctx, data.TransportAction.NewActionCode, s)
						if err != nil {
							return fmt.Errorf("failed to detect action type: %w", err)
						}
						resolver = tmpFullResolver{[]payments.Action{a}, s}
					}

					cond, err := payments.CodeToConditional(ctx, data.TransportAction.Conditional, resolver)
					if err != nil {
						return fmt.Errorf("failed to parse conditional: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					resOut, ok := cond.(*conditionals.ConditionalResolvable)
					if !ok {
						return fmt.Errorf("outgoing conditional is not resolvable")
					}

					var resolverDetailsCell *cell.Cell
					if _, _, instruction, buildErr := s.buildDerivativeResolverContract(channel, resOut.GetKey(), resOut.Amount, resOut.Details); buildErr == nil {
						if resolverDetailsCell, buildErr = tlb.ToCell(instruction); buildErr != nil {
							log.Warn().Err(buildErr).Str("key", base64.StdEncoding.EncodeToString(resOut.GetKey())).
								Msg("failed to serialize derivative resolver instruction details")
						}
					} else {
						log.Warn().Err(buildErr).Str("key", base64.StdEncoding.EncodeToString(resOut.GetKey())).
							Msg("failed to rebuild derivative resolver instruction details")
					}

					inCondResolvable, err := s.buildLinkedDerivativeConditional(resOut)
					if err != nil {
						return fmt.Errorf("failed to prepare linked derivative conditional: %w", err)
					}

					incomingKey := ed25519.PublicKey(inCondResolvable.GetKey())

					metaOut := &db.ConditionalMeta{
						Key:    cond.GetKey(),
						Status: db.ConditionalStatePending,
						Outgoing: &db.ConditionalMetaSide{
							ChannelAddress:        data.ChannelAddress,
							Conditional:           data.TransportAction.Conditional,
							UncooperativeDeadline: time.Unix(data.Deadline, 0),
							SafeDeadline:          time.Unix(data.Deadline, 0).Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
							LinkedKey:             incomingKey,
						},
						SpecialDetails: buildDerivativeMetaAny(resOut.Details, resolverDetailsCell),
						CreatedAt:      time.Now(),
						UpdatedAt:      time.Now(),
					}

					metaIn := &db.ConditionalMeta{
						Key:    incomingKey,
						Status: db.ConditionalStatePending,
						Incoming: &db.ConditionalMetaSide{
							ChannelAddress:        data.ChannelAddress,
							Conditional:           inCondResolvable.Serialize(),
							UncooperativeDeadline: time.Unix(data.Deadline, 0),
							SafeDeadline:          time.Unix(data.Deadline, 0).Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
							SenderKey:             channel.Their.OnchainInfo.Key,
							LinkedKey:             cond.GetKey(),
						},
						SpecialDetails: buildDerivativeMetaAny(inCondResolvable.Details, resolverDetailsCell),
						CreatedAt:      time.Now(),
						UpdatedAt:      time.Now(),
					}

					if err := s.db.CreateVirtualChannelMeta(ctx, metaOut); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
						return fmt.Errorf("failed to save meta out: %w", err)
					}
					if err := s.db.CreateVirtualChannelMeta(ctx, metaIn); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
						return fmt.Errorf("failed to save meta in: %w", err)
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, data.TransportAction, cond); err != nil {
						if errors.Is(err, ErrDenied) {
							// ensure that state was not modified on the other side by sending newer state without this conditional
							if err := s.proposeAction(ctx, lockId, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: false}, nil); err != nil {
								return fmt.Errorf("failed to increment states on conditional revert: %w", err)
							}

							return s.db.Transaction(ctx, func(ctx context.Context) error {
								meta, err := s.db.GetVirtualChannelMeta(ctx, cond.GetKey())
								if err != nil {
									return fmt.Errorf("failed to load conditional meta: %w", err)
								}

								meta.Status = db.ConditionalStateWantRemove
								meta.UpdatedAt = time.Now()
								if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
									return fmt.Errorf("failed to update conditional meta: %w", err)
								}

								return nil
							})
						} else if errors.Is(err, ErrNotPossible) {
							// not possible by us, so no revert confirmation needed
							log.Warn().Err(err).Msg("it is not possible to open virtual channel")
							return nil
						}
						return fmt.Errorf("failed to propose actions to the next node: %w", err)
					}

					err = s.db.Transaction(ctx, func(ctx context.Context) error {
						metaOut, err = s.db.GetVirtualChannelMeta(ctx, cond.GetKey())
						if err != nil {
							return fmt.Errorf("failed to load conditional meta: %w", err)
						}

						metaIn, err = s.db.GetVirtualChannelMeta(ctx, incomingKey)
						if err != nil {
							return fmt.Errorf("failed to load conditional meta: %w", err)
						}

						metaOut.Status = db.ConditionalStateActive
						metaOut.UpdatedAt = time.Now()
						if err = s.db.UpdateVirtualChannelMeta(ctx, metaOut); err != nil {
							return fmt.Errorf("failed to update conditional meta: %w", err)
						}

						metaIn.Status = db.ConditionalStateActive
						metaIn.UpdatedAt = time.Now()
						if err = s.db.UpdateVirtualChannelMeta(ctx, metaIn); err != nil {
							return fmt.Errorf("failed to update conditional meta: %w", err)
						}

						if s.webhook != nil {
							if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeOpen, metaOut); err != nil {
								return fmt.Errorf("failed to push conditional open event: %w", err)
							}
						}
						return nil
					})
					if err != nil {
						return fmt.Errorf("failed to update conditional meta: %w", err)
					}

					log.Info().Fields(cond.GetLogInfo()).
						Str("target", data.ChannelAddress).
						Msg("derivative conditional created")
				case "swap":
					var data db.SwapTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if channel.Status != db.ChannelStateActive {
						log.Warn().Str("channel", channel.Our.Address).Msg("channel is not active, skipping swap")
						return nil
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, data.TransportAction, nil); err != nil {
						if errors.Is(err, ErrDenied) {
							// ensure that state was not modified on the other side by sending newer state without this conditional
							if err := s.proposeAction(ctx, lockId, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: false}, nil); err != nil {
								return fmt.Errorf("failed to increment states on conditional revert: %w", err)
							}

							log.Warn().Msg("swap is denied, revert confirmation sent")
							return nil
						} else if errors.Is(err, ErrNotPossible) {
							// not possible by us, so no revert confirmation needed
							log.Warn().Msg("it is not possible to swap")
							return nil
						}
						return fmt.Errorf("failed to propose actions to the next node: %w", err)
					}

					log.Info().Str("channel", data.ChannelAddress).
						Msg("swap completed successfully")
				case "remove-cond":
					var data db.RemoveConditionalTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.Key)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load conditional meta: %w", err)
					}

					if meta.Status == db.ConditionalStateRemoved || meta.Status == db.ConditionalStateClosed ||
						meta.Incoming == nil {
						return nil
					}

					channelAddr := meta.Incoming.ChannelAddress
					if meta.Outgoing != nil && meta.Outgoing.ChannelAddress != "" {
						channelAddr = meta.Outgoing.ChannelAddress
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, channelAddr)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					id := meta.Incoming.Conditional.Hash()
					if err = s.proposeAction(ctx, lockId, meta.Incoming.ChannelAddress, transport.RemoveConditionalAction{
						ID: id,
					}, nil); err != nil {
						if !errors.Is(err, ErrNotPossible) {
							// We start uncooperative close at specific moment to have time
							// to commit resolve onchain in case partner is irresponsible.
							// But in the same time we give our partner time to
							uncooperativeAfter := time.Now().Add(5 * time.Minute)

							// Creating aggressive onchain close task, for the future,
							// in case we will not be able to communicate with party
							if err := s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", meta.Incoming.ChannelAddress+"-uncoop",
								"uncooperative-close-"+meta.Incoming.ChannelAddress+"-vc-"+base64.StdEncoding.EncodeToString(id),
								db.ChannelUncooperativeCloseTask{
									Address:              meta.Incoming.ChannelAddress,
									CheckCondStillExists: id,
								}, &uncooperativeAfter, nil,
							); err != nil {
								log.Warn().Err(err).Str("channel", meta.Incoming.ChannelAddress).Msg("failed to create uncooperative close task")
							}
						}

						if errors.Is(err, ErrNotPossible) || errors.Is(err, ErrDenied) {
							log.Warn().Err(err).Msg("it is not possible to remove virtual channel")
						}
						return fmt.Errorf("failed to propose remove virtual action: %w", err)
					}

					// next party accepted remove, so we are ready to release coins to previous party
					meta.Status = db.ConditionalStateWantRemove
					meta.UpdatedAt = time.Now()
					if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
						return fmt.Errorf("failed to update conditional meta: %w", err)
					}

					// if we are not the first node of the tunnel
					if meta.Outgoing != nil && len(meta.Outgoing.LinkedKey) > 0 {
						channel, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to load 'from' channel: %w", err)
						}

						_, vch, err := payments.FindConditional(ctx, channel.Their.Data.Conditionals, meta.Incoming.Conditional.Hash(), s)
						if err != nil {
							if errors.Is(err, payments.ErrNotFound) {
								return nil
							}
							return fmt.Errorf("failed to find conditional with 'from': %w", err)
						}

						tryTill := vch.GetDeadline()
						// consider conditional unsuccessful and gracefully removed
						// and notify previous party that we are ready to release locked coins.
						err = s.db.CreateTask(ctx, PaymentsTaskPool, "remove-cond", meta.Incoming.ChannelAddress,
							"remove-cond-"+base64.StdEncoding.EncodeToString(meta.Key),
							db.RemoveConditionalTask{
								Key: meta.Key,
							}, nil, &tryTill,
						)
						if err != nil {
							return fmt.Errorf("failed to create remove-cond task: %w", err)
						}
					}
				case "cooperative-close":
					var data db.ChannelCooperativeCloseTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, _, unlock, err := s.AcquireChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if ch.Status != db.ChannelStateActive {
						return nil
					}

					if ch.InitAt.Before(data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					req, dataCell, _, err := s.getCooperativeCloseRequest(ctx, ch, ch.WeLeft)
					if err != nil {
						if errors.Is(err, ErrNotActive) {
							// expected channel already closed
							return nil
						}
						return fmt.Errorf("failed to prepare close channel request: %w", err)
					}

					cl, err := tlb.ToCell(req)
					if err != nil {
						return fmt.Errorf("failed to serialize request to cell: %w", err)
					}

					log.Info().Str("address", ch.Our.Address).Msg("trying cooperative close")

					if ch.AcceptingActions {
						ch.AcceptingActions = false
						if err = s.db.UpdateChannel(ctx, ch); err != nil {
							return fmt.Errorf("failed to update channel: %w", err)
						}
					}

					partySignature, err := s.requestAction(ctx, ch.Our.Address, transport.CooperativeCloseAction{
						SignedCloseRequest: cl,
					})
					if err != nil {
						return fmt.Errorf("failed to request action from the node: %w", err)
					}

					if !dataCell.Verify(ch.Their.OnchainInfo.Key, partySignature) {
						return fmt.Errorf("incorrect party signature")
					}

					if ch.WeLeft {
						req.SignatureB.Value = partySignature
					} else {
						req.SignatureA.Value = partySignature
					}

					if ch.Our.ActiveOnchain && ch.Their.ActiveOnchain {
						ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()

						if err = s.executeCooperativeClose(ctxTx, req, ch); err != nil {
							return fmt.Errorf("failed to execute cooperative close: %w", err)
						}
					} else {
						balances, err := ch.CalcBalance(ctx, false, s)
						if err != nil {
							return fmt.Errorf("failed to calc balance: %w", err)
						}

						for _, b := range balances {
							if b.Available().Sign() > 0 {
								return fmt.Errorf("channel is not active onchain, and we have balance")
							}
						}

						log.Warn().Str("channel", ch.Our.Address).Msg("channel is not active onchain, and we have ne balance on this channel, onchain action skipped")
					}
				case "uncooperative-close":
					var data db.ChannelUncooperativeCloseTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						return nil
					}

					if data.ChannelInitiatedAt != nil && channel.InitAt.After(*data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					if data.CheckCondStillExists != nil {
						_, _, err = payments.FindConditional(ctx, channel.Their.Data.Conditionals, data.CheckCondStillExists, s)
						if err != nil {
							if errors.Is(err, payments.ErrNotFound) {
								return nil
							}
							return fmt.Errorf("failed to find virtual channel: %w", err)
						}
					}

					if !channel.Our.ActiveOnchain || !channel.Their.ActiveOnchain {
						log.Warn().Str("channel", channel.Our.Address).Msg("channel is not active onchain, uncoop close skipped")
						return nil
					}

					if err = s.scheduleDerivativeResolverDeployTask(ctx, channel.Our.Address, &channel.InitAt, nil); err != nil {
						log.Warn().Err(err).Str("channel", channel.Our.Address).Msg("failed to schedule derivative resolvers deployment")
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.startUncooperativeClose(ctxTx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to start uncooperative close")
						return err
					}
				case "deploy-derivative-resolvers":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.deployChannelDerivativeResolvers(ctxTx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to deploy derivative resolvers")
						return err
					}
				case "challenge":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					if err = s.scheduleDerivativeResolverDeployTask(ctx, data.Address, nil, nil); err != nil {
						log.Warn().Err(err).Str("channel", data.Address).Msg("failed to schedule derivative resolvers deployment")
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.challengeChannelState(ctxTx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to challenge state")
						return err
					}
				case "settle":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					if err = s.settleChannelConditionals(context.Background(), data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle conditionals")
						return err
					}
				case "settle-step":
					var data db.SettleConditionalStepTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
					defer cancel()

					if err = s.executeSettleStep(ctxTx, data.Address, data.Message, data.Step); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Int("step", data.Step).Msg("failed to settle conditionals step")
						return err
					}
				case "settle-fin":
					var data db.FinalizeSettleTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
					defer cancel()

					if err = s.executeSettleFinalize(ctxTx, data.ChannelAddress, data.ExpectedActionsHash); err != nil {
						log.Error().Err(err).Str("channel", data.ChannelAddress).
							Str("expected_hash", base64.StdEncoding.EncodeToString(data.ExpectedActionsHash)).
							Msg("failed to finalize settle")
						return err
					}
				case "settle-act":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					if err = s.settleChannelActions(ctx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle actions")
						return err
					}
				case "settle-act-step":
					var data db.SettleActionStepTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
					defer cancel()

					if err = s.executeSettleActionStep(ctxTx, data.Address, data.Message, data.Step); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Int("step", data.Step).Msg("failed to settle actions step")
						return err
					}
				case "finalize":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					if err = s.finishUncooperativeChannelClose(ctx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to finish close")
						return err
					}
				case "topup":
					var data db.TopupTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.GetChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status != db.ChannelStateActive {
						return nil
					}

					if ch.InitAt.Before(data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					cc, err := s.ResolveCoinConfig(data.BalanceID)
					if err != nil {
						return fmt.Errorf("failed to resolve coin config: %w", err)
					}

					if err = s.ExecuteTopup(ctx, data.Address, data.BalanceID, tlb.MustFromDecimal(data.Amount, int(cc.Decimals)), data.FromBalanceControl); err != nil {
						return fmt.Errorf("failed to execute topup: %w", err)
					}
				case "commit-action":
					var data db.ActionCommitTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status != db.ChannelStateActive {
						return nil
					}

					if !ch.Our.ActiveOnchain || !ch.Their.ActiveOnchain {
						log.Warn().Str("channel", ch.Our.Address).Msg("channel is not active onchain, withdraw skipped")
						return nil
					}

					ch, lockId, unlock, err := s.AcquireChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if ch.InitAt.Before(data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					var payFee *bool
					if data.ForFee {
						side := ch.WeLeft
						payFee = &side
					}

					req, _, _, _, err := s.getCommitRequest(ctx, ch, data.ActionId, !data.ForFee, payFee)
					if err != nil {
						if errors.Is(err, ErrNotActive) || errors.Is(err, ErrNothingToCommit) {
							// expected channel already closed or already committed
							return nil
						}
						return fmt.Errorf("failed to prepare execute action channel request: %w", err)
					}

					if ch.PendingCommit != nil {
						if ch.PendingCommit.Seqno == req.Signed.Seqno {
							return nil
						}
						return fmt.Errorf("can't execute new commit while there is already pending commit")
					}

					if len(ch.Their.PendingOnchainTransfers) > 0 || len(ch.Our.PendingOnchainTransfers) > 0 {
						return fmt.Errorf("can't execute action while there are pending onchian transfers")
					}

					sig := req.SignatureA.Value
					if !ch.WeLeft {
						sig = req.SignatureB.Value
					}

					log.Info().Str("address", ch.Our.Address).Msg("proposing cooperative commit to execute action")

					if err := s.proposeAction(ctx, lockId, ch.Our.Address, transport.CooperativeCommitAction{
						ActionID:     data.ActionId,
						MsgSignature: sig,
						WithFee:      data.ForFee,
					}, nil); err != nil {
						return fmt.Errorf("failed to increment state with party: %w", err)
					}

					return nil
				case "commit-execute":
					var data db.CommitExecuteTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					var req payments.CooperativeCommit
					if err = tlb.Parse(&req, data.SignedRequest); err != nil {
						return fmt.Errorf("failed to serialize their commit channel request: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status != db.ChannelStateActive {
						return nil
					}

					if ch.Our.LatestCommitedSeqno >= req.Signed.Seqno || ch.Their.LatestCommitedSeqno >= req.Signed.Seqno {
						log.Info().Str("channel", ch.Our.Address).Msg("already committed, skipping")
						return nil
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					addr := address.MustParseAddr(ch.Our.Address)
					if req.Signed.FromA != ch.WeLeft {
						addr = address.MustParseAddr(ch.Their.Address)
					}

					if err = s.executeCooperativeCommit(ctxTx, &req, addr); err != nil {
						return fmt.Errorf("failed to execute cooperative close: %w", err)
					}
				case "wait-pending-tx-completion":
					var data db.WaitPendingTxTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status == db.ChannelStateInactive {
						return nil
					}

					side := &ch.Our
					if !data.IsOurSide {
						side = &ch.Their
					}

					pn := side.PendingOnchainTransfers[data.PendingID]
					if pn == nil {
						return nil
					}

					var completion *address.Address
					if pn.CompletionAddress != "" {
						completion = address.MustParseAddr(pn.CompletionAddress)
					}

					at, err := s.resolveTxChain(ctx, address.MustParseAddr(side.Address), completion, data.MsgHash, pn.CompletionBodyPrefix, data.StartedAt, pn.LimitDepth)
					if err != nil {
						return fmt.Errorf("check failed: %w", err)
					}

					balances, err := s.fetchChannelSideOnchainBalances(ctx, address.MustParseAddr(side.Address), time.Unix(at, 0))
					if err != nil {
						return fmt.Errorf("failed to refresh onchain balance: %w", err)
					}
					changes := calcBalancesDiff(side.OnchainBalances, balances)

					// refresh channel since operation takes a long time, and we don't want to retry if something changed
					ch, err = s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}
					side = &ch.Our
					if !data.IsOurSide {
						side = &ch.Their
					}
					side.OnchainBalances = balances
					delete(side.PendingOnchainTransfers, data.PendingID)

					err = s.db.Transaction(ctx, func(ctx context.Context) error {
						if len(changes) > 0 {
							if err = s.createChannelBalanceChangedEvent(ctx, ch, changes, !data.IsOurSide); err != nil {
								return fmt.Errorf("failed to create channel balance change event: %w", err)
							}
						}

						if err = s.db.UpdateChannel(ctx, ch); err != nil {
							return fmt.Errorf("failed to update channel: %w", err)
						}
						return nil
					})
					if err != nil {
						return fmt.Errorf("failed to execute db tx: %w", err)
					}
				case "wait-deposit-completion":
					var data db.WaitDepositCompletionTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status == db.ChannelStateInactive {
						return nil
					}

					at, err := s.resolveTxChain(ctx, address.MustParseAddr(data.FromAddress),
						address.MustParseAddr(ch.Our.Address), data.MsgHash, nil, data.StartedAt, 0)
					if err != nil {
						return fmt.Errorf("check failed: %w", err)
					}

					balances, err := s.fetchChannelSideOnchainBalances(ctx, address.MustParseAddr(ch.Our.Address), time.Unix(at, 0))
					if err != nil {
						return fmt.Errorf("failed to refresh onchain balance: %w", err)
					}
					changes := calcBalancesDiff(ch.Our.OnchainBalances, balances)

					err = s.db.Transaction(ctx, func(ctx context.Context) error {
						// refresh channel since operation takes a long time and we don't want to retry if something changed
						ch, err = s.db.GetChannel(ctx, data.ChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to get channel: %w", err)
						}
						ch.Our.OnchainBalances = balances

						if len(changes) > 0 {
							if err = s.createChannelBalanceChangedEvent(ctx, ch, changes, false); err != nil {
								return fmt.Errorf("failed to create channel balance change event: %w", err)
							}
						}

						if err = s.db.UpdateChannel(ctx, ch); err != nil {
							return fmt.Errorf("failed to update channel: %w", err)
						}
						return nil
					})
					if err != nil {
						return fmt.Errorf("failed to execute db tx: %w", err)
					}

					if data.UnlockBalanceControl {
						if bc := s.balanceControllers[data.BalanceID]; bc != nil {
							bc.mx.Lock()
							if bc.channels[data.ChannelAddress] != nil {
								bc.channels[data.ChannelAddress].depositLockedTill = nil
							}
							bc.mx.Unlock()
						}
					}
				case "refresh-onchain-balance":
					var data db.RefreshOnchainBalanceTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status == db.ChannelStateInactive {
						return nil
					}

					side := &ch.Our
					if !data.IsOurSide {
						side = &ch.Their
					}

					balances, err := s.fetchChannelSideOnchainBalances(ctx, address.MustParseAddr(side.Address), time.Unix(data.BlockAfter, 0))
					if err != nil {
						return fmt.Errorf("failed to refresh onchain balance: %w", err)
					}
					changes := calcBalancesDiff(side.OnchainBalances, balances)

					err = s.db.Transaction(ctx, func(ctx context.Context) error {
						// refresh channel since operation takes a long time and we don't want to retry if something changed
						ch, err = s.db.GetChannel(ctx, data.ChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to get channel: %w", err)
						}
						side = &ch.Our
						if !data.IsOurSide {
							side = &ch.Their
						}
						side.OnchainBalances = balances

						if len(changes) > 0 {
							if err = s.createChannelBalanceChangedEvent(ctx, ch, changes, !data.IsOurSide); err != nil {
								return fmt.Errorf("failed to create channel balance change event: %w", err)
							}
						}

						if err = s.db.UpdateChannel(ctx, ch); err != nil {
							return fmt.Errorf("failed to update channel: %w", err)
						}
						return nil
					})
					if err != nil {
						return fmt.Errorf("failed to execute db tx: %w", err)
					}
				case "request-tx-external":
					var data db.RequestExternalTxTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					contract, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(ch.Our.Address), true, ch.Our.LastProcessedTxAt)
					if err != nil {
						return fmt.Errorf("failed to get channel contract: %w", err)
					}

					messages, err := payments.UnpackOutActions(data.PackedMessages)
					if err != nil {
						return fmt.Errorf("failed to unpack actions: %w", err)
					}

					if ch.Status == db.ChannelStateInactive {
						body, err := contract.PrepareOwnerExternalMessage(s.key, messages, uint32(time.Now().Add(15*time.Minute).Unix()))
						if err != nil {
							return fmt.Errorf("failed to prepare double external message: %w", err)
						}

						ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()

						if err = s.executeSignedExternal(ctxTx, body, address.MustParseAddr(ch.Our.Address)); err != nil {
							return fmt.Errorf("failed to execute external tx: %w", err)
						}
						return nil
					}

					if ch.Status != db.ChannelStateActive {
						log.Warn().Str("channel", ch.Our.Address).Msg("channel is not active, skipping external tx request")
						return nil
					}

					body, _, err := contract.PrepareDoubleExternalMessage(s.key, nil, messages, uint32(time.Now().Add(15*time.Minute).Unix()))
					if err != nil {
						return fmt.Errorf("failed to prepare double external message: %w", err)
					}

					var req payments.ExternalMsgDoubleSigned
					err = tlb.Parse(&req, body)
					if err != nil {
						return fmt.Errorf("failed to serialize request: %w", err)
					}

					if req.Signed.WalletSeqno > data.WalletSeqno {
						// executed already
						return nil
					}

					if ch.PendingCommit != nil || len(ch.Their.PendingOnchainTransfers) > 0 {
						// waiting commit completion, when it will be enough balances
						return ErrWaitingForCapacity
					}

					p, notEnoughOnchain, err := s.validateOutMessages(ctx, ch, req.Signed.OutActions, false)
					if err != nil {
						return fmt.Errorf("failed to validate out messages: %w", err)
					}

					if len(notEnoughOnchain) > 0 {
						var balanceId string
						for id := range notEnoughOnchain {
							if balanceId == "" || balanceId < id {
								// consistently choose balance to commit
								balanceId = id
							}
						}

						if ch.Their.Data.ActionStates.IsEmpty() {
							// no actions
							log.Warn().Str("channel", ch.Our.Address).Msg("nothing to commit to move onchain balance, external skipped")
							return nil
						}

						states, err := ch.Their.Data.ActionStates.LoadAll()
						if err != nil {
							return fmt.Errorf("failed to load action states: %w", err)
						}

						var actionId []byte
						biggest := big.NewInt(0)

						// looking for the most suitable action to commit to move balance to our contract from a party
						for _, v := range states {
							id := v.Key.MustLoadSlice(256)
							act, err := s.ResolveAction(ctx, id)
							if err != nil {
								return fmt.Errorf("failed to resolve action %s: %w", base64.StdEncoding.EncodeToString(id), err)
							}

							// just to calculate effect
							_, pi, err := act.PrepareExecuteState(v.Value.MustToCell(), address.MustParseAddr(ch.Our.Address), ch.LoadSignedState().Body.Seqno, false, nil)
							if err != nil && !errors.Is(err, payments.ErrAlreadyCommitted) {
								return fmt.Errorf("failed to prepare execute state: %w", err)
							}

							if pi != nil {
								if amt := pi.Amounts[balanceId]; amt != nil && amt.Cmp(biggest) > 0 {
									actionId = id
									biggest.Set(amt)
								}
							}
						}

						if actionId == nil {
							log.Warn().Str("channel", ch.Our.Address).Msg("nothing to commit to move onchain balance, external skipped")
							return nil
						}

						if err = s.requestCommitAction(ctx, ch, actionId); err != nil {
							return fmt.Errorf("failed to commit: %w", err)
						}

						return ErrWaitingForCapacity
					}

					ch, _, unlock, err := s.AcquireChannel(ctx, ch.Our.Address)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					// onchain balance is enough, request party to sign and execute transaction from our contract
					_, err = s.requestAction(ctx, ch.Our.Address, transport.ExecuteTransactionAction{
						ExternalBody: body,
					})
					if err != nil {
						return fmt.Errorf("request to close conditional failed: %w", err)
					}

					ch.Our.PendingOnchainTransfers[pendingIDWallet(req.Signed.WalletSeqno)] = p

					if err = s.db.UpdateChannel(ctx, ch); err != nil {
						return fmt.Errorf("failed to update channel: %w", err)
					}
				case "exec-tx-external":
					var data db.ExecuteExternalTxTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					ch, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to get channel: %w", err)
					}

					if ch.Status != db.ChannelStateActive {
						return nil
					}

					side := &ch.Our
					if !data.OurSide {
						side = &ch.Their
					}

					if side.PendingOnchainTransfers[pendingIDWallet(data.WalletSeqno)] == nil {
						// executed or expired
						return nil
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.executeSignedExternal(ctxTx, data.Body, address.MustParseAddr(side.Address)); err != nil {
						return fmt.Errorf("failed to execute external tx: %w", err)
					}
				default:
					log.Error().Err(err).Str("type", task.Type).Str("id", task.ID).Msg("unknown task type, skipped")
					return fmt.Errorf("unknown task type")
				}
				return nil
			}()
			if err != nil {
				lg := log.Warn
				if errors.Is(err, ErrChannelIsBusy) || errors.Is(err, db.ErrChannelBusy) ||
					errors.Is(err, transport.ErrNotConnected) || errors.Is(err, db.ErrNotFound) ||
					errors.Is(err, ErrActionStarted) || errors.Is(err, ErrWaitingForCapacity) || errors.Is(err, ErrStillPending) {
					// for not critical retryable errors we will not flood console in normal mode
					lg = log.Debug
				}
				lg().Err(err).Str("type", task.Type).Str("id", task.ID).Msg("task execute err, will be retried")

				// random wait to not lock both sides in same time
				retryAfter := time.Now()
				if !errors.Is(err, ErrChannelIsBusy) && !errors.Is(err, db.ErrChannelBusy) {
					retryAfter = retryAfter.Add(time.Duration(2500+rand.Int63()%8000) * time.Millisecond)
				} else {
					retryAfter = retryAfter.Add(time.Duration(10+rand.Int63()%5000) * time.Millisecond)
				}

				if err = s.db.RetryTask(context.Background(), task, err.Error(), retryAfter); err != nil {
					log.Error().Err(err).Str("id", task.ID).Msg("failed to set failure for task in db")
				}
				return
			}

			if err = s.db.CompleteTask(context.Background(), PaymentsTaskPool, task); err != nil {
				log.Error().Err(err).Str("id", task.ID).Msg("failed to set complete for task in db")
			}
		}()
	}
}

// touchWorker - forces worker to check db tasks
func (s *Service) touchWorker() {
	select {
	case s.workerSignal <- true:
		// ask queue to take new task without waiting
	default:
	}
}

func (s *Service) resolveTxChain(ctx context.Context, addr, completionAddr *address.Address, inMsgHash, completionPrefix []byte, minBlockTime time.Time, limitDepth int) (int64, error) {
	var found bool
	var latestAt int64
	var checkTx func(addr *address.Address, msgHash []byte, depth int) error
	checkTx = func(addr *address.Address, msgHash []byte, depth int) error {
		tx, err := s.ton.GetTransactionByInMsgHash(ctx, addr, msgHash, minBlockTime)
		if err != nil {
			return fmt.Errorf("failed to get transaction: %w", err)
		}
		// TODO: in case of failed contract deploy or destruction, it may stuck in pending,
		//  because LS responds with contract not exists and actually breaks transactions chain
		if tx == nil {
			return ErrStillPending
		}

		if latestAt < tx.At {
			latestAt = tx.At
		}

		if completionAddr != nil && address.MustParseAddr(tx.In.To).Equals(completionAddr) {
			if sz := uint(len(completionPrefix)) * 8; tx.In.Body.BitsSize() >= sz {
				if sz == 0 || bytes.Equal(tx.In.Body.MustBeginParse().MustLoadSlice(sz), completionPrefix) {
					found = true
					return nil
				}
			}
		}

		if limitDepth > 0 && depth >= limitDepth {
			return nil
		}

		var totalErr error
		for _, out := range tx.Out {
			if err = checkTx(address.MustParseAddr(out.To), out.MsgHash, depth+1); err != nil {
				if totalErr == nil {
					totalErr = err
				}

				if !errors.Is(err, ErrStillPending) {
					return err
				}
			}

			if found {
				// skip searches if found already
				return nil
			}
		}

		// still pending or ok if nil
		return totalErr
	}

	err := checkTx(addr, inMsgHash, 0)
	if err != nil {
		return 0, err
	}

	return latestAt, nil
}

func (s *Service) createChannelBalanceChangedEvent(ctx context.Context, ch *db.Channel, amt map[string]*big.Int, isTheir bool) error {
	evData := db.ChannelHistoryActionAmountData{
		IsTheir: isTheir,
		Amounts: s.formatDiff(amt),
	}
	jsonData, err := json.Marshal(evData)
	if err != nil {
		log.Error().Err(err).Int("type", int(db.ChannelHistoryActionBalanceChanged)).Msg("failed to marshal event data")
	}

	if err = s.db.CreateChannelEvent(ctx, ch, time.Now(), db.ChannelHistoryItem{
		Action: db.ChannelHistoryActionBalanceChanged,
		Data:   jsonData,
	}); err != nil {
		return fmt.Errorf("failed to create withdraw channel event %d: %w", db.ChannelHistoryActionBalanceChanged, err)
	}
	return nil
}

func (s *Service) rentCapacityIfNeeded(ctx context.Context, channel *db.Channel, cc *payments.CoinConfig, theirBalances map[string]*payments.BalanceInfo) error {
	theirBalance := theirBalances[cc.BalanceID]
	if theirBalance == nil {
		theirBalance = payments.NewBalanceInfo(cc)
	}

	// if balance is negative we should rent capacity
	if available := theirBalance.Available(); available.Sign() < 0 {
		toGet := new(big.Int).Abs(available)

		ld := channel.Their.LockedDeposits[cc.BalanceID]
		if ld == nil || ld.Available().Cmp(theirBalance.ConditionalLocked) < 0 {
			reqAmount := cc.MinCapacityRequest.Nano()
			if toGet.Cmp(reqAmount) > 0 {
				reqAmount = new(big.Int).Set(toGet)
			}

			maxRentPerAction := cc.VirtualTunnelConfig.MaxCapacityToRentPerTx
			if maxRentPerAction.Nano().Cmp(reqAmount) < 0 {
				// should rent in several actions
				reqAmount = maxRentPerAction.Nano()
			}

			err := func() error {
				channel, lockId, unlock, err := s.AcquireChannel(ctx, channel.Our.Address)
				if err != nil {
					return fmt.Errorf("failed to acquire channel: %w", err)
				}
				defer unlock()

				if channel.Status != db.ChannelStateActive {
					// not needed anymore
					return fmt.Errorf("channel is not active")
				}

				till := time.Now().Add(30 * 24 * time.Hour)

				if ld != nil && ld.Till.After(time.Now()) {
					reqAmount.Add(reqAmount, ld.Amount)
				}

				bid, _ := hex.DecodeString(cc.BalanceID)
				// TheirLockedDeposit will be updated when action proposed successfully
				if err = s.proposeAction(ctx, lockId, channel.Our.Address, transport.RentCapacityAction{
					Till:      uint64(till.Unix()),
					Amount:    reqAmount.Bytes(),
					BalanceID: bid,
				}, nil); err != nil {
					return fmt.Errorf("failed to propose rent capacity action: %w", err)
				}

				return nil
			}()
			if err != nil {
				return fmt.Errorf("failed to rent capacity: %w", err)
			}
		}

		// enough capacity rented, waiting for actual topup from their side
		log.Warn().
			Str("channel", channel.Our.Address).
			Str("locked", cc.MustAmount(ld.Available()).String()).
			Str("need", cc.MustAmount(toGet).String()).
			Str("has", cc.MustAmount(available).String()).
			Msg("not enough capacity, it was rented, waiting for actual topup")
		return ErrWaitingForCapacity
	}
	return nil
}
