//go:build !(js && wasm)

package tonpayments

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/metrics"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"strconv"
	"time"
)

func (s *Service) walletMonitor() {
	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		err := func() error {
			ctx, cancel := context.WithTimeout(s.globalCtx, 10*time.Second)
			defer cancel()

			acc, err := s.ton.GetAccount(ctx, s.wallet.WalletAddress())
			if err != nil {
				return fmt.Errorf("failed to get ton balance: %w", err)
			}

			if !acc.HasState {
				return fmt.Errorf("account is not yet initialized")
			}

			for ec, config := range s.cfg.SupportedCoins.ExtraCurrencies {
				var balance float64
				if !acc.ExtraCurrencies.IsEmpty() {
					val, err := acc.ExtraCurrencies.LoadValueByIntKey(big.NewInt(int64(ec)))
					if err != nil {
						log.Trace().Err(err).Msg("failed to get ec key")
						continue
					}

					x, err := val.LoadVarUInt(32)
					if err != nil {
						log.Trace().Err(err).Msg("failed to get ec value")
						continue
					}

					balance, _ = strconv.ParseFloat(tlb.MustFromNano(x, int(config.Decimals)).String(), 64)
				}

				metrics.WalletBalance.WithLabelValues(config.Symbol).Set(balance)
			}

			for jettonAddr, config := range s.cfg.SupportedCoins.Jettons {
				var balance float64
				jb, err := s.ton.GetJettonBalance(ctx, address.MustParseAddr(jettonAddr), s.wallet.WalletAddress())
				if err != nil {
					log.Trace().Err(err).Msg("failed to get jetton balance")
					continue
				}

				balance, _ = strconv.ParseFloat(tlb.MustFromNano(jb, int(config.Decimals)).String(), 64)

				metrics.WalletBalance.WithLabelValues(config.Symbol).Set(balance)
			}

			balance, _ := strconv.ParseFloat(acc.Balance.String(), 64)
			metrics.WalletBalance.WithLabelValues("TON").Set(balance)

			return nil
		}()
		if err != nil {
			log.Trace().Err(err).Msg("failed to monitor wallet balance")
		}
	}
}

func (s *Service) channelsMonitor() {
	type virtualData struct {
		decimals int
		num      int
		capacity *big.Int
		fees     *big.Int
	}

	type split struct {
		balance map[string]float64
		virtual map[bool]*virtualData
	}
	stats := map[string]map[string]map[bool]*split{}

	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		list, err := s.ListChannels(context.Background(), nil, db.ChannelStateActive)
		if err != nil {
			log.Error().Err(err).Msg("failed to list active channels")
			continue
		}

	next:
		for _, channel := range list {
			coinConfig, err := s.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, false)
			if err != nil {
				log.Error().Err(err).Msg("failed to resolve coin config")
			}

			channelName := "other"
			s.urgentPeersMx.RLock()
			key := base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)
			if s.urgentPeers[key] {
				channelName = key[:16]
			}
			s.urgentPeersMx.RUnlock()

			channelStats, exists := stats[channelName]
			if !exists {
				channelStats = map[string]map[bool]*split{}
				stats[channelName] = channelStats
			}

			coinStats, exists := channelStats[coinConfig.Symbol]
			if !exists {
				coinStats = map[bool]*split{}
				channelStats[coinConfig.Symbol] = coinStats
			}

			for _, isOurSide := range []bool{true, false} {
				sideStats, exists := coinStats[isOurSide]
				if !exists {
					sideStats = &split{
						balance: map[string]float64{},
						virtual: map[bool]*virtualData{},
					}
					coinStats[isOurSide] = sideStats
				}

				onchainState, virtuals := channel.TheirOnchain, channel.Their.Conditionals
				if isOurSide {
					onchainState, virtuals = channel.OurOnchain, channel.Our.Conditionals
				}

				for _, kv := range virtuals.All() {
					vch, err := payments.ParseVirtualChannelCond(kv.Value.BeginParse())
					if err != nil {
						log.Error().Err(err).Msg("failed to parse virtual channel")
						continue next
					}

					outdated := time.Now().After(time.Unix(vch.Deadline, 0))
					v := sideStats.virtual[outdated]
					if v == nil {
						v = &virtualData{
							decimals: int(coinConfig.Decimals),
							num:      0,
							capacity: big.NewInt(0),
							fees:     big.NewInt(0),
						}
					}
					v.num++
					v.capacity.Add(v.capacity, vch.Capacity)
					v.fees.Add(v.fees, vch.Fee)

					sideStats.virtual[outdated] = v
				}

				for _, category := range []string{"deposited", "balance", "sent", "withdrawn"} {
					var value *big.Int
					switch category {
					case "deposited":
						value = onchainState.Deposited
					case "sent":
						value = onchainState.Sent
					case "withdrawn":
						value = onchainState.Withdrawn
					case "balance":
						value, err = channel.CalcBalance(isOurSide)
						if err != nil {
							log.Error().Err(err).Msg("failed to calc balance")
							continue next
						}
					}

					parsedValue, _ := strconv.ParseFloat(tlb.MustFromNano(value, int(coinConfig.Decimals)).String(), 64)
					sideStats.balance[category] += parsedValue
				}
			}
		}

		for channelName, channelStats := range stats {
			for coinSymbol, coinStats := range channelStats {
				for isOurSide, sideStats := range coinStats {
					for wantRemove, v := range sideStats.virtual {
						capacity, _ := strconv.ParseFloat(tlb.MustFromNano(v.capacity, v.decimals).String(), 64)
						fee, _ := strconv.ParseFloat(tlb.MustFromNano(v.fees, v.decimals).String(), 64)

						metrics.ActiveVirtualChannels.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide), strconv.FormatBool(wantRemove)).Set(float64(v.num))
						metrics.ActiveVirtualChannelsCapacity.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide), strconv.FormatBool(wantRemove)).Set(capacity)
						metrics.ActiveVirtualChannelsFee.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide), strconv.FormatBool(wantRemove)).Set(fee)

						sideStats.virtual[wantRemove] = &virtualData{
							decimals: v.decimals,
							num:      0,
							capacity: big.NewInt(0),
							fees:     big.NewInt(0),
						} // reset to calc in next iteration
					}

					for category, balance := range sideStats.balance {
						metrics.ChannelBalance.WithLabelValues(channelName, coinSymbol, strconv.FormatBool(isOurSide), category).Set(balance)

						sideStats.balance[category] = 0 // reset to calc in next iteration
					}
				}
			}
		}
	}
}

func (s *Service) taskMonitor() {
	taskStats := map[string]map[bool]map[bool]float64{}

	for {
		select {
		case <-s.globalCtx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		list, err := s.db.ListActiveTasks(context.Background(), PaymentsTaskPool)
		if err != nil {
			log.Error().Err(err).Msg("failed to list active tasks")
			continue
		}

		for _, task := range list {
			hasError := task.LastError != ""
			executeLater := task.ExecuteAfter.After(time.Now())

			typeStats, exists := taskStats[task.Type]
			if !exists {
				typeStats = map[bool]map[bool]float64{}
				taskStats[task.Type] = typeStats
			}

			errorStats, exists := typeStats[hasError]
			if !exists {
				errorStats = map[bool]float64{}
				typeStats[hasError] = errorStats
			}

			errorStats[executeLater] += 1
		}

		for jobType, errorStats := range taskStats {
			for hasError, timingStats := range errorStats {
				for executeLater, taskCount := range timingStats {
					metrics.QueuedTasks.WithLabelValues(jobType, strconv.FormatBool(hasError), strconv.FormatBool(executeLater)).Set(taskCount)

					timingStats[executeLater] = 0 // reset to calc in next iteration
				}
			}
		}
	}
}
