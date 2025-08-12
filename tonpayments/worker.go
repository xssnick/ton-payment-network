package tonpayments

import (
	"context"
	"encoding/base64"
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
	"math/rand"
	"time"
)

var ErrWaitingForCapacity = errors.New("capacity request was sent, waiting for it")
var ErrActionStarted = errors.New("action was started, waiting for completion from other side")

func (s *Service) taskExecutor() {
	if s.useMetrics {
		go s.taskMonitor()
	}

	tick := time.Tick(1 * time.Second)

	for {
		select {
		case <-s.globalCtx.Done():
			return
		default:
		}

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
				case "confirm-close-virtual":
					var data db.ConfirmCloseVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					if meta.Status != db.VirtualChannelStateActive {
						log.Debug().Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).Msg("is not active, skip closing")
						return nil
					}

					var state *cell.Cell
					if data.State == nil {
						// reverse compatibility: get latest known state
						resolve := meta.GetKnownResolve()
						if resolve == nil {
							return fmt.Errorf("failed to load virtual channel resolve: %w", err)
						}

						state, err = tlb.ToCell(resolve)
						if err != nil {
							return fmt.Errorf("failed to serialize virtual channel resolve: %w", err)
						}
					} else {
						state, err = cell.FromBOC(data.State)
						if err != nil {
							return fmt.Errorf("failed to parse state boc: %w", err)
						}
					}

					var vState payments.VirtualChannelState
					if err = tlb.LoadFromCell(&vState, state.BeginParse()); err != nil {
						return fmt.Errorf("failed to load virtual channel state cell: %w", err)
					}

					evData := db.ChannelHistoryActionTransferOutData{
						Amount: vState.Amount.String(),
						To:     meta.FinalDestination,
					}
					jsonData, err := json.Marshal(evData)
					if err != nil {
						log.Error().Err(err).Msg("failed to marshal event data")
					}

					toChannel, lockId, unlock, err := s.AcquireChannel(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire 'to' channel: %w", err)
					}
					defer unlock()

					if meta.Incoming != nil {
						if toChannel.Status == db.ChannelStateActive {
							err = s.proposeAction(ctx, lockId, meta.Outgoing.ChannelAddress, transport.ConfirmCloseAction{
								Key:   data.VirtualKey,
								State: state,
							}, nil)
							if err != nil {
								return fmt.Errorf("failed to propose action: %w", err)
							}
						}

						if err = s.CloseVirtualChannel(ctx, data.VirtualKey); err != nil {
							return fmt.Errorf("failed to request virtual channel close: %w", err)
						}
					} else if toChannel.Status == db.ChannelStateActive {
						err = s.proposeAction(ctx, lockId, meta.Outgoing.ChannelAddress, transport.ConfirmCloseAction{
							Key:   data.VirtualKey,
							State: state,
						}, nil)
						if err != nil {
							return fmt.Errorf("failed to propose action: %w", err)
						}
					}

					if err = s.db.CreateChannelEvent(ctx, toChannel, time.Now(), db.ChannelHistoryItem{
						Action: db.ChannelHistoryActionTransferOut,
						Data:   jsonData,
					}); err != nil {
						return fmt.Errorf("failed to create channel event: %w", err)
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
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					if meta.Status != db.VirtualChannelStateActive {
						// not needed anymore
						return nil
					}

					_, vch, err := payments.FindVirtualChannel(channel.Our.Conditionals, data.VirtualKey)
					if err != nil {
						if errors.Is(err, payments.ErrNotFound) {
							// no need
							return nil
						}
						return fmt.Errorf("failed to find virtual channel: %w", err)
					}

					resolve := meta.GetKnownResolve()
					if resolve == nil {
						// nothing to commit
						return nil
					}

					toPrepay := new(big.Int).Add(resolve.Amount, vch.Fee)
					if vch.Prepay.Cmp(toPrepay) >= 0 {
						// already commited
						return nil
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, transport.CommitVirtualAction{
						Key:          data.VirtualKey,
						PrepayAmount: toPrepay.Bytes(),
					}, nil); err != nil {
						// reversal is not mandatory, because 'sent amount' is atomic with conditional prepay, and no actual balance change
						return fmt.Errorf("failed to propose action: %w", err)
					}
				case "open-virtual":
					var data db.OpenVirtualTask
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

					cc, err := s.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, false)
					if err != nil {
						return fmt.Errorf("failed to resolve coin config: %w", err)
					}

					nextCap, _ := new(big.Int).SetString(data.Capacity, 10)
					nextFee, _ := new(big.Int).SetString(data.Fee, 10)

					meta := &db.VirtualChannelMeta{
						Key:    data.VirtualKey,
						Status: db.VirtualChannelStatePending,
						Outgoing: &db.VirtualChannelMetaSide{
							ChannelAddress:        data.ChannelAddress,
							Capacity:              tlb.MustFromNano(nextCap, int(cc.Decimals)).String(),
							Fee:                   tlb.MustFromNano(nextFee, int(cc.Decimals)).String(),
							UncooperativeDeadline: time.Unix(data.Deadline, 0),
							SafeDeadline:          time.Unix(data.Deadline, 0).Add(-time.Duration(channel.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						},
						FinalDestination: data.FinalDestinationKey,
						CreatedAt:        time.Now(),
						UpdatedAt:        time.Now(),
					}

					if data.PrevChannelAddress != "" {
						prev, err := s.db.GetChannel(ctx, data.PrevChannelAddress)
						if err != nil {
							return fmt.Errorf("failed to get prev channel: %w", err)
						}

						_, prevVch, err := payments.FindVirtualChannel(prev.Their.Conditionals, meta.Key)
						if err != nil {
							return fmt.Errorf("failed to find prev virtual channel: %w", err)
						}

						meta.Incoming = &db.VirtualChannelMetaSide{
							SenderKey:             data.SenderKey,
							ChannelAddress:        data.PrevChannelAddress,
							Capacity:              tlb.MustFromNano(prevVch.Capacity, int(cc.Decimals)).String(),
							Fee:                   tlb.MustFromNano(prevVch.Fee, int(cc.Decimals)).String(),
							UncooperativeDeadline: time.Unix(prevVch.Deadline, 0),
							SafeDeadline:          time.Unix(prevVch.Deadline, 0).Add(-time.Duration(prev.SafeOnchainClosePeriod+int64(s.cfg.MinSafeVirtualChannelTimeoutSec)) * time.Second),
						}
					}

					if err = s.db.CreateVirtualChannelMeta(ctx, meta); err != nil && !errors.Is(err, db.ErrAlreadyExists) {
						return fmt.Errorf("failed to create virtual channel meta: %w", err)
					}

					if err = s.proposeAction(ctx, lockId, data.ChannelAddress, data.Action, payments.VirtualChannel{
						Key:      data.VirtualKey,
						Capacity: nextCap,
						Fee:      nextFee,
						Prepay:   big.NewInt(0),
						Deadline: data.Deadline,
					}); err != nil {
						if errors.Is(err, ErrDenied) {
							// ensure that state was not modified on the other side by sending newer state without this conditional
							if err := s.proposeAction(ctx, lockId, data.ChannelAddress, transport.IncrementStatesAction{WantResponse: false}, nil); err != nil {
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
									// consider virtual channel unsuccessful and gracefully removed
									// and notify previous party that we are ready to release locked coins.
									err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", data.PrevChannelAddress,
										"ask-remove-virtual-"+base64.StdEncoding.EncodeToString(data.VirtualKey),
										db.AskRemoveVirtualTask{
											ChannelAddress: data.PrevChannelAddress,
											Key:            data.VirtualKey,
										}, nil, nil,
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
						if err = s.webhook.PushVirtualChannelEvent(context.Background(), db.VirtualChannelEventTypeOpen, meta, cc); err != nil {
							return fmt.Errorf("failed to push virtual channel open event: %w", err)
						}
					}

					log.Info().Str("key", base64.StdEncoding.EncodeToString(data.VirtualKey)).
						Str("next_capacity", tlb.MustFromNano(nextCap, int(cc.Decimals)).String()).
						Str("next_fee", tlb.MustFromNano(nextFee, int(cc.Decimals)).String()).
						Str("target", data.ChannelAddress).
						Msg("channel successfully tunnelled through us")
				case "ask-remove-virtual":
					var data db.AskRemoveVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						// not possible anymore
						return nil
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.Key)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					if meta.Status == db.VirtualChannelStateRemoved || meta.Status == db.VirtualChannelStateClosed {
						return nil
					}

					log.Debug().Str("channel", channel.Address).Str("key", base64.StdEncoding.EncodeToString(data.Key)).Msg("asking to remove virtual channel")
					_, err = s.requestAction(ctx, data.ChannelAddress, transport.RequestRemoveVirtualAction{
						Key: data.Key,
					})
					if err != nil && !errors.Is(err, ErrDenied) {
						return fmt.Errorf("request to remove virtual action failed: %w", err)
					}
				case "ask-close-virtual":
					var data db.AskCloseVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					channel, err := s.db.GetChannel(ctx, data.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load channel: %w", err)
					}

					if channel.Status != db.ChannelStateActive {
						log.Warn().Str("channel", channel.Address).Str("key", base64.StdEncoding.EncodeToString(data.Key)).Msg("onchain channel is not active, cannot close virtual")

						// not needed anymore
						return nil
					}

					_, vch, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.Key)
					if err != nil {
						if errors.Is(err, payments.ErrNotFound) {
							log.Debug().Str("channel", channel.Address).Str("key", base64.StdEncoding.EncodeToString(data.Key)).Msg("virtual to close task completion confirmed")

							// nothing to close anymore
							return nil
						}
						return fmt.Errorf("failed to find virtual channel: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, vch.Key)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							log.Warn().Str("channel", channel.Address).Str("key", base64.StdEncoding.EncodeToString(data.Key)).Msg("nothing virtual to close, meta not exists")
							return nil
						}
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					state := meta.GetKnownResolve()
					if state == nil {
						return ErrNoResolveExists
					}

					stateCell, err := state.ToCell()
					if err != nil {
						return fmt.Errorf("failed to serialize state to cell: %w", err)
					}

					theirBalance, theirHoldBalance, err := channel.CalcBalance(true)
					if err != nil {
						return fmt.Errorf("failed to calc other side balance: %w", err)
					}
					if channel.Their.PendingWithdraw != nil {
						pw := new(big.Int).Sub(channel.Their.PendingWithdraw, channel.TheirOnchain.Withdrawn)
						if pw.Sign() > 0 {
							theirHoldBalance.Sub(theirHoldBalance, pw)
						}
					}

					// if balance is negative we should rent capacity
					if theirBalance.Sign() < 0 {
						toGet := new(big.Int).Abs(theirBalance)

						cc, err := s.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, true)
						if err != nil {
							return fmt.Errorf("failed to resolve coin config: %w", err)
						}

						if channel.TheirLockedDeposit == nil || channel.TheirLockedDeposit.Available().Cmp(theirHoldBalance) < 0 {
							reqAmount := cc.MustAmountDecimal(cc.MinCapacityRequest).Nano()
							if toGet.Cmp(reqAmount) > 0 {
								reqAmount = new(big.Int).Set(toGet)
							}

							maxRentPerAction := cc.MustAmountDecimal(cc.VirtualTunnelConfig.MaxCapacityToRentPerTx)
							if maxRentPerAction.Nano().Cmp(reqAmount) < 0 {
								// should rent in several actions
								reqAmount = maxRentPerAction.Nano()
							}

							err = func() error {
								channel, lockId, unlock, err := s.AcquireChannel(ctx, channel.Address)
								if err != nil {
									return fmt.Errorf("failed to acquire channel: %w", err)
								}
								defer unlock()

								if channel.Status != db.ChannelStateActive {
									// not needed anymore
									return fmt.Errorf("channel is not active")
								}

								till := time.Now().Add(30 * 24 * time.Hour)

								if channel.TheirLockedDeposit != nil && channel.TheirLockedDeposit.Till.After(time.Now()) {
									reqAmount.Add(reqAmount, channel.TheirLockedDeposit.Amount)
								}

								// TheirLockedDeposit will be updated when action proposed successfully
								if err = s.proposeAction(ctx, lockId, channel.Address, transport.RentCapacityAction{
									Till:   uint64(till.Unix()),
									Amount: reqAmount.Bytes(),
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
							Str("channel", channel.Address).
							Str("locked", cc.MustAmount(channel.TheirLockedDeposit.Available()).String()).
							Str("need", cc.MustAmount(toGet).String()).
							Str("has", cc.MustAmount(theirBalance).String()).
							Str("key", base64.StdEncoding.EncodeToString(data.Key)).
							Msg("not enough capacity to close virtual channel, it was rented, waiting for actual topup")
						return ErrWaitingForCapacity
					}

					_, err = s.requestAction(ctx, channel.Address, transport.CloseVirtualAction{
						Key:   vch.Key,
						State: stateCell,
					})
					if err != nil {
						return fmt.Errorf("request to close virtual channel failed: %w", err)
					}

					// return err on success request and waiting for actual close from their side
					// before considering a task completed
					return ErrActionStarted
				case "rent-capacity":
					var data db.RentCapacityTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}
					reqAmount, ok := new(big.Int).SetString(data.Amount, 10)
					if !ok {
						return fmt.Errorf("invalid amount: %s", data.Amount)
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

					till := time.Now().Add(30 * 24 * time.Hour)

					if channel.TheirLockedDeposit != nil && channel.TheirLockedDeposit.Till.After(time.Now()) {
						reqAmount.Add(reqAmount, channel.TheirLockedDeposit.Amount)
					}

					// TheirLockedDeposit will be updated when action proposed successfully
					if err = s.proposeAction(ctx, lockId, channel.Address, transport.RentCapacityAction{
						Till:   uint64(till.Unix()),
						Amount: reqAmount.Bytes(),
					}, nil); err != nil {
						return fmt.Errorf("failed to propose rent capacity action: %w", err)
					}

					return nil
				case "remove-virtual":
					var data db.RemoveVirtualTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					meta, err := s.db.GetVirtualChannelMeta(ctx, data.Key)
					if err != nil {
						if errors.Is(err, db.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to load virtual channel meta: %w", err)
					}

					channel, lockId, unlock, err := s.AcquireChannel(ctx, meta.Outgoing.ChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					if channel.Status != db.ChannelStateActive {
						// not needed anymore
						return nil
					}

					if err = s.proposeAction(ctx, lockId, meta.Outgoing.ChannelAddress, transport.RemoveVirtualAction{
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
								"uncooperative-close-"+meta.Outgoing.ChannelAddress+"-vc-"+base64.StdEncoding.EncodeToString(data.Key),
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

						_, vch, err := payments.FindVirtualChannel(channel.Their.Conditionals, data.Key)
						if err != nil {
							if errors.Is(err, payments.ErrNotFound) {
								return nil
							}
							return fmt.Errorf("failed to find virtual channel with 'from': %w", err)
						}

						tryTill := time.Unix(vch.Deadline, 0)
						// consider virtual channel unsuccessful and gracefully removed
						// and notify previous party that we are ready to release locked coins.
						err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", meta.Incoming.ChannelAddress,
							"ask-remove-virtual-"+base64.StdEncoding.EncodeToString(data.Key),
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

					req, dataCell, _, err := s.getCooperativeCloseRequest(ch)
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

					log.Info().Str("address", ch.Address).Msg("trying cooperative close")

					if ch.AcceptingActions {
						ch.AcceptingActions = false
						if err = s.db.UpdateChannel(ctx, ch); err != nil {
							return fmt.Errorf("failed to update channel: %w", err)
						}
					}

					partySignature, err := s.requestAction(ctx, ch.Address, transport.CooperativeCloseAction{
						SignedCloseRequest: cl,
					})
					if err != nil {
						return fmt.Errorf("failed to request action from the node: %w", err)
					}

					if !dataCell.Verify(ch.TheirOnchain.Key, partySignature) {
						return fmt.Errorf("incorrect party signature")
					}

					if ch.WeLeft {
						req.SignatureB.Value = partySignature
					} else {
						req.SignatureA.Value = partySignature
					}

					if ch.ActiveOnchain {
						ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()

						if err = s.executeCooperativeClose(ctxTx, req, ch); err != nil {
							return fmt.Errorf("failed to execute cooperative close: %w", err)
						}
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

					if data.CheckVirtualStillExists != nil {
						_, _, err = payments.FindVirtualChannel(channel.Their.Conditionals, data.CheckVirtualStillExists)
						if err != nil {
							if errors.Is(err, payments.ErrNotFound) {
								return nil
							}
							return fmt.Errorf("failed to find virtual channel: %w", err)
						}
					}

					if !channel.ActiveOnchain {
						log.Warn().Str("channel", channel.Address).Msg("channel is not active onchain, uncoop close skipped")
						return nil
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

					if err = s.settleChannelConditionals(context.Background(), data.Address); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle conditionals")
						return err
					}
				case "settle-step":
					var data db.SettleStepTask
					if err = json.Unmarshal(task.Data, &data); err != nil {
						return fmt.Errorf("invalid json: %w", err)
					}

					var messages []*cell.Cell
					for i, message := range data.Messages {
						m, err := cell.FromBOC(message)
						if err != nil {
							return fmt.Errorf("invalid message %d boc: %w", i, err)
						}
						messages = append(messages, m)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
					defer cancel()

					if err = s.executeSettleStep(ctxTx, data.Address, messages, data.Step); err != nil {
						log.Error().Err(err).Str("channel", data.Address).Int("step", data.Step).Msg("failed to settle conditionals step")
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

					cc, err := s.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
					if err != nil {
						return fmt.Errorf("failed to resolve coin config: %w", err)
					}

					if err = s.ExecuteTopup(ctx, data.Address, tlb.MustFromDecimal(data.Amount, int(cc.Decimals))); err != nil {
						return fmt.Errorf("failed to execute topup: %w", err)
					}
				case "withdraw":
					var data db.WithdrawTask
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

					ch, _, unlock, err := s.AcquireChannel(ctx, data.Address)
					if err != nil {
						return fmt.Errorf("failed to acquire channel: %w", err)
					}
					defer unlock()

					// TODO: check if not withdrawn already
					if ch.InitAt.Before(data.ChannelInitiatedAt) {
						// expected channel already closed
						return nil
					}

					cc, err := s.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
					if err != nil {
						return fmt.Errorf("failed to resolve coin config: %w", err)
					}

					maxTheirWithdraw := new(big.Int).Set(ch.Their.PendingWithdraw)
					if ch.TheirOnchain.Withdrawn.Cmp(maxTheirWithdraw) > 0 {
						maxTheirWithdraw.Set(ch.TheirOnchain.Withdrawn)
					}
					amountTheir, err := tlb.FromNano(maxTheirWithdraw, int(cc.Decimals))
					if err != nil {
						return fmt.Errorf("failed to convert amount to nano: %w", err)
					}

					amount := tlb.MustFromDecimal(data.Amount, int(cc.Decimals))
					req, dataCell, _, err := s.getCommitRequest(amount, amountTheir, ch)
					if err != nil {
						if errors.Is(err, ErrNotActive) {
							// expected channel already closed
							return nil
						}
						return fmt.Errorf("failed to prepare withdraw channel request: %w", err)
					}

					cl, err := tlb.ToCell(req)
					if err != nil {
						return fmt.Errorf("failed to serialize request to cell: %w", err)
					}

					log.Info().Str("address", ch.Address).Msg("trying cooperative commit to withdraw")

					partySignature, err := s.requestAction(ctx, ch.Address, transport.CooperativeCommitAction{
						SignedCommitRequest: cl,
					})
					if err != nil {
						return fmt.Errorf("failed to request action from the node: %w", err)
					}

					if !dataCell.Verify(ch.TheirOnchain.Key, partySignature) {
						return fmt.Errorf("incorrect party signature")
					}

					log.Info().Str("address", ch.Address).Msg("cooperative commit party signature received, executing transaction...")

					if ch.WeLeft {
						req.SignatureB.Value = partySignature
					} else {
						req.SignatureA.Value = partySignature
					}

					ch.Our.PendingWithdraw = amount.Nano()
					if err = s.db.UpdateChannel(ctx, ch); err != nil {
						return fmt.Errorf("failed to update channel: %w", err)
					}

					ctxTx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
					defer cancel()

					if err = s.executeCooperativeCommit(ctxTx, req, ch); err != nil {
						return fmt.Errorf("failed to execute cooperative close: %w", err)
					}

					if err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", ch.Address,
						"increment-state-"+ch.Address+"-"+fmt.Sprint(ch.Our.State.Data.Seqno),
						db.IncrementStatesTask{
							ChannelAddress: ch.Address,
							WantResponse:   true,
						}, nil, nil,
					); err != nil {
						return fmt.Errorf("failed to create increment-state task: %w", err)
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
					errors.Is(err, ErrActionStarted) || errors.Is(err, ErrWaitingForCapacity) {
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
