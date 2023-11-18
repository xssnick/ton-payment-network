package node

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/internal/node/transport"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

func (s *Service) peerDiscovery() {
	for {
		list, err := s.db.GetActiveChannels(context.Background())
		if err != nil {
			log.Error().Err(err).Msg("failed to get active channels from db")
			time.Sleep(5 * time.Second)
			continue
		}

		keys := map[string][]byte{}
		// get distinct keys to try to connect with them
		for _, v := range list {
			keys[string(v.TheirOnchain.Key)] = v.TheirOnchain.Key
		}

		for _, k := range keys {
			go func(key []byte) {
				// ping
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := s.transport.GetChannelConfig(ctx, key)
				cancel()
				if err != nil {
					log.Debug().Err(err).Hex("key", key).Msg("failed to connect with peer")
					return
				}
			}(k)
		}
		time.Sleep(30 * time.Second)
	}
}

func (s *Service) taskExecutor() {
	for {
		task, err := s.db.AcquireTask(context.Background())
		if err != nil {
			log.Error().Err(err).Msg("failed to acquire task from db")
			time.Sleep(3 * time.Second)
			continue
		}

		if task == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		err = func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			switch task.Type {
			case "exchange-states":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.incrementStates(ctx, data.Address, true); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to exchange states")
					return err
				}
			case "increment-state":
				var data db.IncrementStatesTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.incrementStates(ctx, data.ChannelAddress, data.WantResponse); err != nil {
					return fmt.Errorf("failed to increment state with party: %w", err)
				}
			case "confirm-close-virtual":
				var data db.ConfirmCloseVirtualTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				vStateCell, err := cell.FromBOC(data.State)
				if err != nil {
					return fmt.Errorf("failed parse state boc: %w", err)
				}

				meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
				if err != nil {
					return fmt.Errorf("failed to load virtual channel meta: %w", err)
				}

				if meta.FromChannelAddress != "" {
					channel, err := s.db.GetChannel(ctx, meta.FromChannelAddress)
					if err != nil {
						return fmt.Errorf("failed to load 'from' channel: %w", err)
					}

					_, vch, err := channel.Their.State.FindVirtualChannel(data.VirtualKey)
					if err != nil {
						return fmt.Errorf("failed to find virtual channel with 'from': %w", err)
					}

					println("CONFIRM CLOSING:" + meta.ToChannelAddress)
					err = s.proposeAction(ctx, meta.ToChannelAddress, transport.ConfirmCloseAction{
						Key:   data.VirtualKey,
						State: vStateCell,
					}, nil)
					if err != nil {
						return fmt.Errorf("failed to propose action: %w", err)
					}
					println("CONFIRM CLOSING DONE:" + meta.ToChannelAddress)

					tryTill := time.Unix(vch.Deadline, 0)
					if err = s.db.CreateTask(ctx, "close-next-virtual", meta.FromChannelAddress,
						"close-next-"+hex.EncodeToString(data.VirtualKey),
						db.CloseNextVirtualTask{
							VirtualKey: data.VirtualKey,
							State:      vStateCell.ToBOC(),
						}, nil, &tryTill,
					); err != nil {
						return fmt.Errorf("failed to create close-next-virtual task: %w", err)
					}
					println("CONFIRM CLOSING TASK CREATED:" + meta.ToChannelAddress + " " + meta.FromChannelAddress)
				} else {
					err = s.proposeAction(ctx, meta.ToChannelAddress, transport.ConfirmCloseAction{
						Key:   data.VirtualKey,
						State: vStateCell,
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

				println("WRK CLOSING:" + hex.EncodeToString(data.VirtualKey))

				if err = s.CloseVirtualChannel(ctx, data.VirtualKey, vState); err != nil {
					return fmt.Errorf("failed to request virtual channel close: %w", err)
				}
			case "open-virtual":
				var data db.OpenVirtualTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				nextCap, _ := new(big.Int).SetString(data.Capacity, 10)
				nextFee, _ := new(big.Int).SetString(data.Fee, 10)
				if err = s.proposeAction(ctx, data.ChannelAddress, data.Action, payments.VirtualChannel{
					Key:      data.VirtualKey,
					Capacity: nextCap,
					Fee:      nextFee,
					Deadline: data.Deadline,
				}); err != nil {
					if errors.Is(err, ErrDenied) {
						// ensure that state was not modified on the other side by sending newer state without this conditional
						if err = s.incrementStates(ctx, data.ChannelAddress, false); err != nil {
							return fmt.Errorf("failed to increment states: %w", err)
						}

						return s.db.Transaction(ctx, func(ctx context.Context) error {
							meta, err := s.db.GetVirtualChannelMeta(ctx, data.VirtualKey)
							if err != nil {
								return fmt.Errorf("failed to load virtual channel meta: %w", err)
							}

							meta.ReadyToReleaseCoins = true
							if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
								return fmt.Errorf("failed to update virtual channel meta: %w", err)
							}

							// if we are not the first node of the tunnel
							if data.PrevChannelAddress != "" {
								tryTill := time.Unix(data.Deadline, 0)
								// consider virtual channel unsuccessful and gracefully removed
								// and notify previous party that we are ready to release locked coins.
								err = s.db.CreateTask(ctx, "ask-remove-virtual", data.PrevChannelAddress,
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
					}
					return fmt.Errorf("failed to propose actions to the next node: %w", err)
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

				err = s.proposeAction(ctx, meta.ToChannelAddress, transport.RemoveVirtualAction{
					Key: data.Key,
				}, nil)
				if err != nil {
					return fmt.Errorf("failed to propose remove virtual action: %w", err)
				}

				// next party accepted remove, so we are ready to release coins to previous party
				meta.ReadyToReleaseCoins = true
				if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
					return fmt.Errorf("failed to update virtual channel meta: %w", err)
				}

				// if we are not the first node of the tunnel
				if meta.FromChannelAddress != "" {
					channel, err := s.db.GetChannel(ctx, meta.FromChannelAddress)
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
					err = s.db.CreateTask(ctx, "ask-remove-virtual", meta.FromChannelAddress,
						"ask-remove-virtual-"+hex.EncodeToString(data.Key),
						db.AskRemoveVirtualTask{
							ChannelAddress: meta.FromChannelAddress,
							Key:            data.Key,
						}, nil, &tryTill,
					)
					if err != nil {
						return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
					}
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

				if data.CheckVirtualStillExists != nil {
					_, _, err = channel.Their.State.FindVirtualChannel(data.CheckVirtualStillExists)
					if err != nil {
						if errors.Is(err, payments.ErrNotFound) {
							return nil
						}
						return fmt.Errorf("failed to find virtual channel: %w", err)
					}
				}

				if err = s.StartUncooperativeClose(ctx, data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to start uncooperative close")
					return err
				}
			case "challenge":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.ChallengeChannelState(ctx, data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to challenge state")
					return err
				}
			case "settle":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.SettleChannelConditionals(ctx, data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle conditionals")
					return err
				}
			case "finalize":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.FinishUncooperativeChannelClose(ctx, data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to finish close")
					return err
				}
			case "deploy-inbound":
				var data db.DeployInboundTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				capacity, _ := new(big.Int).SetString(data.Capacity, 10)
				addr, err := s.deployChannelWithNode(context.Background(), data.ID, data.Key, address.MustParseAddr(data.WalletAddress), tlb.FromNanoTON(capacity))
				if err != nil {
					return fmt.Errorf("deploy of requested channel is failed: %w", err)
				}
				log.Info().Str("addr", addr.String()).Msg("requested channel is deployed")
			default:
				log.Error().Err(err).Str("type", task.Type).Str("id", task.ID).Msg("unknown task type, skipped")
				return fmt.Errorf("unknown task type")
			}
			return nil
		}()
		if err != nil {
			log.Warn().Err(err).Msg("task execute err, will be retried")

			retryAfter := time.Now().Add(10 * time.Second)
			if err = s.db.RetryTask(context.Background(), task, err.Error(), retryAfter); err != nil {
				log.Error().Err(err).Str("id", task.ID).Msg("failed to set failure for task in db")
				time.Sleep(3 * time.Second)
			}
			continue
		}

		if err = s.db.CompleteTask(context.Background(), task); err != nil {
			log.Error().Err(err).Str("id", task.ID).Msg("failed to set complete for task in db")
			time.Sleep(3 * time.Second)
		}
	}
}
