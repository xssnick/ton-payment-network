package tonpayments

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"math/rand"
	"time"
)

func (s *Service) addPeersForChannels() error {
	list, err := s.db.GetChannels(context.Background(), nil, db.ChannelStateActive)
	if err != nil {
		return err
	}

	for _, v := range list {
		s.transport.AddUrgentPeer(v.TheirOnchain.Key)
	}
	return nil
}

func (s *Service) taskExecutor() {
	tick := time.Tick(1 * time.Second)

	for {
		task, err := s.db.AcquireTask(context.Background(), PaymentsTaskPool)
		if err != nil {
			log.Error().Err(err).Msg("failed to acquire task from db")
			time.Sleep(3 * time.Second)
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
			err = func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				switch task.Type {
				case "increment-state":
					var data db.IncrementStatesTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.getVerifiedChannel(data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					unlock, err := s.db.AcquireChannelLock(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel lock: %w", err)
					}
					defer unlock()

					if err := s.proposeAction(ctx, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: data.WantResponse}, nil); err != nil {
						return fmt.Errorf("failed to increment state with party: %w", err)
					}
				case "confirm-close-virtual":
					var data db.ConfirmCloseVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
					if err != nil {
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					resolve := meta.GetKnownResolve(data.VirtualKey)
					if resolve == nil {
						return fmt.Errorf("failed to load virtual channel resolve: %w", err)
					}

					state, err := tlb.ToCell(resolve)
					if err != nil {
						return fmt.Errorf("failed to serialize virtual channel resolve: %w", err)
					}

					unlock, err := s.db.AcquireChannelLock(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel lock: %w", err)
					}
					defer unlock()

					toChannel, err := s.db.GetChannel(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if meta.Incoming != nil {
						channel, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to load 'from' channel: %w", err)
						}

						_, vch, err := channel.Their.State.FindVirtualChannel(data.VirtualKey)
						if err != nil {
							return fmt.Errorf("failed to find virtual channel with 'from': %w", err)
						}

						if toChannel.Status == db.ChannelStateActive {
							err = s.proposeAction(ctx, meta.Outgoing.ChannelAddress, transport.ConfirmCloseAction{
								Key:   data.VirtualKey,
								State: state,
							}, nil)
							if err != nil {
								return fmt.Errorf("failed to propose action: %w", err)
							}
						}

						tryTill := time.Unix(vch.Deadline, 0)
						if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-next-virtual", meta.Incoming.ChannelAddress,
							"close-next-"+hex.EncodeToString(data.VirtualKey),
							db.CloseNextVirtualTask{
								VirtualKey: data.VirtualKey,
								State:      state.ToBOC(),
							}, nil, &tryTill,
						); err != nil {
							return fmt.Errorf("failed to create close-next-virtual task: %w", err)
						}
					} else if toChannel.Status == db.ChannelStateActive {
						err = s.proposeAction(ctx, meta.Outgoing.ChannelAddress, transport.ConfirmCloseAction{
							Key:   data.VirtualKey,
							State: state,
						}, nil)
						if err != nil {
							return fmt.Errorf("failed to propose action: %w", err)
						}
					}
				case "close-next-virtual":
					var data db.CloseNextVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					vStateCell, err := cell.FromBOC(data.State)
					if err != nil {
						return fmt.Errorf("failed parse state boc: %w", err)
					}

					var vState payments.VirtualChannelState
					if err = tlb.LoadFromCell(&vState, vStateCell.BeginParse()); err != nil {
						return fmt.Errorf("failed to load virtual channel state cell: %w", err)
					}

					if err = s.CloseVirtualChannel(ctx, data.VirtualKey); err != nil {
						return fmt.Errorf("failed to request virtual channel close: %w", err)
					}

					if data.IsTransfer && s.webhook != nil {
						meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
						if err != nil {
							return fmt.Errorf("failed to load virtual channel meta: %w", err)
						}

						if err = s.webhook.PushVirtualChannelEvent(ctx, db.VirtualChannelEventTypeTransfer, meta); err != nil {
							return fmt.Errorf("failed to push virtual channel transfer event: %w", err)
						}
					}
					return nil
				case "open-virtual":
					var data db.OpenVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.getVerifiedChannel(data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					unlock, err := s.db.AcquireChannelLock(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel lock: %w", err)
					}
					defer unlock()

					nextCap, _ := new(big.Int).SetString(data.Capacity, 10)
					nextFee, _ := new(big.Int).SetString(data.Fee, 10)

					meta := &db.VirtualChannelMeta{
						Key:    data.VirtualKey,
						Status: db.VirtualChannelStatePending,
						Outgoing: &db.VirtualChannelMetaSide{
							ChannelAddress: data.ChannelAddress,
							Capacity:       tlb.FromNanoTON(nextCap).String(),
							Fee:            tlb.FromNanoTON(nextFee).String(),
							Deadline:       time.Unix(data.Deadline, 0),
						},
						CreatedAt: time.Now(),
						UpdatedAt: time.Now(),
					}

					if data.PrevChannelAddress != "" {
						prev, err := s.db.GetChannel(ctx, data.PrevChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to get prev channel: %w", err)
						}

						_, prevVch, err := prev.Their.State.FindVirtualChannel(meta.Key)
						if err != nil {
							return fmt.Errorf("failed to find prev virtual channel: %w", err)
						}

						meta.Incoming = &db.VirtualChannelMetaSide{
							ChannelAddress: data.PrevChannelAddress,
							Capacity:       tlb.FromNanoTON(prevVch.Capacity).String(),
							Fee:            tlb.FromNanoTON(prevVch.Fee).String(),
							Deadline:       time.Unix(prevVch.Deadline, 0),
						}
					}

					if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
						return fmt.Errorf("failed to create virtual channel meta: %w", err)
					}

					if err = s.proposeAction(ctx, data.ChannelAddress, data.Action, payments.VirtualChannel{
						Key:      data.VirtualKey,
						Capacity: nextCap,
						Fee:      nextFee,
						Deadline: data.Deadline,
					}); err != nil {
						if errors.Is(err, ErrDenied) {
							// ensure that state was not modified on the other side by sending newer state without this conditional
							if err := s.proposeAction(ctx, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: false}, nil); err != nil {
								return fmt.Errorf("failed to increment states on virtual channel revert: %w", err)
							}

							return s.db.Transaction(ctx, func(ctx context.Context) error {
								meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
								if err != nil {
									return fmt.Errorf("failed to load virtual channel meta: %w", err)
								}

								meta.Status = db.VirtualChannelStateWantRemove
								meta.UpdatedAt = time.Now()
								if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
									return fmt.Errorf("failed to update virtual channel meta: %w", err)
								}

								// if we are not the first node of the tunnel
								if data.PrevChannelAddress != "" {
									tryTill := time.Unix(data.Deadline, 0)
									// consider virtual channel unsuccessful and gracefully removed
									// and notify previous party that we are ready to release locked coins.
									err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", data.PrevChannelAddress,
										"ask-remove-virtual-"+hex.EncodeToString(data.VirtualKey),
										db.AskRemoveVirtualTask{
											ChannelAddress: data.PrevChannelAddress,
											Key:            data.VirtualKey,
										}, nil, &tryTill,
									)
									if err != nil {
										return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
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

					meta.Status = db.VirtualChannelStateActive
					meta.UpdatedAt = time.Now()
					if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
						return fmt.Errorf("failed to update virtual channel meta: %w", err)
					}

					if s.webhook != nil {
						if err = s.webhook.PushVirtualChannelEvent(context.Background(), db.VirtualChannelEventTypeOpen, meta); err != nil {
							return fmt.Errorf("failed to push virtual channel open event: %w", err)
						}
					}

					log.Info().Hex("key", data.VirtualKey).
						Str("next_capacity", tlb.FromNanoTON(nextCap).String()).
						Str("next_fee", tlb.FromNanoTON(nextFee).String()).
						Str("target", data.ChannelAddress).
						Msg("channel successfully tunnelled through us")
				case "ask-remove-virtual":
					var data db.AskRemoveVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.getVerifiedChannel(data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					err = s.requestAction(ctx, data.ChannelAddress, transport.RequestRemoveVirtualAction{
						Key: data.Key,
					})
					if err != nil && !errors.Is(err, ErrDenied) {
						return fmt.Errorf("request to remove virtual action failed: %w", err)
					}
				case "remove-virtual":
					var data db.RemoveVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.Key)
					if err != nil {
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					unlock, err := s.db.AcquireChannelLock(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel lock: %w", err)
					}
					defer unlock()

					if err = s.proposeAction(ctx, meta.Outgoing.ChannelAddress, transport.RemoveVirtualAction{
						Key: data.Key,
					}, nil); err != nil {
						if !errors.Is(err, ErrNotPossible) {
							// We start uncooperative close at specific moment to have time
							// to commit resolve onchain in case partner is irresponsible.
							// But in the same time we give our partner time to
							uncooperativeAfter := time.Now().Add(5 * time.Minute)

							// Creating aggressive onchain close task, for the future,
							// in case we will not be able to communicate with party
							if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", meta.Outgoing.ChannelAddress+"-uncoop",
								"uncooperative-close-"+meta.Outgoing.ChannelAddress+"-vc-"+hex.EncodeToString(data.Key),
								db.ChannelUncooperativeCloseTask{
									Address:                 meta.Outgoing.ChannelAddress,
									CheckVirtualStillExists: data.Key,
								}, &uncooperativeAfter, nil,
							); err != nil {
								log.Warn().Err(err).Str("channel", meta.Outgoing.ChannelAddress).Msg("failed to create uncooperative close task")
							}
						}

						if errors.Is(err, ErrNotPossible) || errors.Is(err, ErrDenied) {
							// we don't have this channel or they don't
							log.Warn().Err(err).Msg("it is not possible to remove virtual channel")
							return nil
						}
						return fmt.Errorf("failed to propose remove virtual action: %w", err)
					}

					// next party accepted remove, so we are ready to release coins to previous party
					meta.Status = db.VirtualChannelStateWantRemove
					meta.UpdatedAt = time.Now()
					if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
						return fmt.Errorf("failed to update virtual channel meta: %w", err)
					}

					// if we are not the first node of the tunnel
					if meta.Incoming != nil {
						channel, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to load 'from' channel: %w", err)
						}

						_, vch, err := channel.Their.State.FindVirtualChannel(data.Key)
						if err != nil {
							return fmt.Errorf("failed to find virtual channel with 'from': %w", err)
						}

						tryTill := time.Unix(vch.Deadline, 0)
						// consider virtual channel unsuccessful and gracefully removed
						// and notify previous party that we are ready to release locked coins.
						err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", meta.Incoming.ChannelAddress,
							"ask-remove-virtual-"+hex.EncodeToString(data.Key),
							db.AskRemoveVirtualTask{
								ChannelAddress: meta.Incoming.ChannelAddress,
								Key:            data.Key,
							}, nil, &tryTill,
						)
						if err != nil {
							return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
						}
					}
				case "cooperative-close":
					var data db.ChannelCooperativeCloseTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					unlock, err := s.db.AcquireChannelLock(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to acquire channel lock: %w", err)
					}
					defer unlock()

					req, ch, err := s.getCooperativeCloseRequest(data.Address, nil)
					if err != nil {
						if errors.Is(err, ErrNotActive) {
							// expected channel already closed
							return nil
						}
						return fmt.Errorf("failed to prepare close channel request: %w", err)
					}

					if ch.InitAt.Before(data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					cl, err := tlb.ToCell(req)
					if err != nil {
						return fmt.Errorf("failed to serialize request to cell: %w", err)
					}

					log.Info().Str("address", ch.Address).Msg("trying cooperative close")

					if err = s.requestAction(ctx, ch.Address, transport.CooperativeCloseAction{
						SignedCloseRequest: cl,
					}); err != nil {
						return fmt.Errorf("failed to request action from the node: %w", err)
					}
				case "uncooperative-close":
					var data db.ChannelUncooperativeCloseTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.getVerifiedChannel(data.Address)
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

					if data.CheckVirtualStillExists != nil {
						_, _, err = channel.Their.State.FindVirtualChannel(data.CheckVirtualStillExists)
						if err != nil {
							if errors.Is(err, payments.ErrNotFound) {
								return nil
							}
							return fmt.Errorf("failed to find virtual channel: %w", err)
						}
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.startUncooperativeClose(ctxTx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to start uncooperative close")
						return err
					}
				case "challenge":
					var data db.ChannelTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
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

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.settleChannelConditionals(ctxTx, data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle conditionals")
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
				default:
					log.Error().Err(err).Str("type", task.Type).Str("id", task.ID).Msg("unknown task type, skipped")
					return fmt.Errorf("unknown task type")
				}
				return nil
			}()
			if err != nil {
				lg := log.Warn
				if errors.Is(err, ErrChannelIsBusy) || errors.Is(err, db.ErrChannelBusy) || errors.Is(err, transport.ErrNotConnected) {
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

			s.touchWorker()
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
