package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

func (s *Service) peerDiscovery() {
	for {
		list, err := s.db.GetActiveChannels()
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
					log.Warn().Err(err).Hex("key", key).Msg("failed to connect with peer")
					return
				}
			}(k)
		}
		time.Sleep(30 * time.Second)
	}
}

func (s *Service) taskExecutor() {
	for {
		task, err := s.db.AcquireTask()
		if err != nil {
			log.Error().Err(err).Msg("failed to acquire task from db")
			time.Sleep(3 * time.Second)
			continue
		}

		if task == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		retryAfter := time.Now().Add(30 * time.Second)
		err = func() error {
			switch task.Type {
			case "exchange-states":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.IncrementStates(context.Background(), data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to exchange states")
					return err
				}
				// TODO: wait for their state to confirm task success
			case "close-next":
				var data db.CloseNextTask
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

				if err = s.CloseVirtualChannel(context.Background(), data.VirtualKey, vState); err != nil {
					return fmt.Errorf("failed to request virtual channel close: %w", err)
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

				if err = s.StartUncooperativeClose(context.Background(), data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to start uncooperative close")
					return err
				}
			case "challenge":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.ChallengeChannelState(context.Background(), data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to challenge state")
					return err
				}
			case "settle":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.SettleChannelConditionals(context.Background(), data.Address); err != nil {
					log.Error().Err(err).Str("channel", data.Address).Msg("failed to settle conditionals")
					return err
				}
			case "finalize":
				var data db.ChannelTask
				if err = json.Unmarshal(task.Data, &data); err != nil {
					return fmt.Errorf("invalid json: %w", err)
				}

				if err = s.FinishUncooperativeChannelClose(context.Background(), data.Address); err != nil {
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
			if err = s.db.RetryTask(task.ID, err.Error(), retryAfter); err != nil {
				log.Error().Err(err).Str("id", task.ID).Msg("failed to set failure for task in db")
				time.Sleep(3 * time.Second)
			}
			continue
		}

		if err = s.db.CompleteTask(task.ID); err != nil {
			log.Error().Err(err).Str("id", task.ID).Msg("failed to set complete for task in db")
			time.Sleep(3 * time.Second)
		}
	}
}
