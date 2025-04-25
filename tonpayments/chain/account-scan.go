package chain

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"time"
)

func (v *Scanner) StartSmall(events chan<- any) error {
	go v.accFetcherWorker(events, 3)

	return nil
}

func (v *Scanner) OnChannelUpdate(_ context.Context, ch *db.Channel, statusChanged bool) {
	if !statusChanged {
		return
	}

	v.mx.Lock()
	defer v.mx.Unlock()

	if ch.Status == db.ChannelStateInactive {
		if c := v.activeChannels[ch.Address]; c != nil {
			c() // stop listener
			delete(v.activeChannels, ch.Address)
		}
		log.Info().Str("address", ch.Address).Msg("stop listening for channel events")
		return
	}

	if v.activeChannels[ch.Address] == nil {
		ctx, cancel := context.WithCancel(v.globalCtx)
		v.activeChannels[ch.Address] = cancel

		lt := uint64(0)
		if ch.LastProcessedLT > 0 {
			// to report last tx
			lt = ch.LastProcessedLT - 1
		}

		log.Info().Str("address", ch.Address).Msg("start listening for channel events")
		go v.startForContract(ctx, address.MustParseAddr(ch.Address), lt)
	}
}

func (v *Scanner) startForContract(ctx context.Context, addr *address.Address, sinceLT uint64) {
	originalCtx := ctx
	for {
		ch := make(chan *tlb.Transaction, 1)
		go v.api.SubscribeOnTransactions(ctx, addr, sinceLT, ch)

		for transaction := range ch {
			for {
				m, err := v.api.CurrentMasterchainInfo(ctx)
				if err != nil {
					time.Sleep(1 * time.Second)
					log.Warn().Str("address", addr.String()).Msg("failed to fetch master block, will retry in 1s")
					continue
				}

				log.Debug().Str("address", addr.String()).Msg("found new transaction, fetching account")
				v.taskPool <- accFetchTask{
					master:   m,
					tx:       transaction,
					addr:     addr,
					callback: func() {},
				}
				break
			}
		}

		log.Warn().Str("address", addr.String()).Msg("SubscribeOnTransactions stopped listening because of LS reported tx not in DB, will retry with another LS...")

		var err error
		ctx, err = v.api.Client().StickyContextNextNode(ctx)
		if err != nil {
			log.Error().Err(err).Msg("all nodes failed, will retry all again :(")
			ctx = originalCtx
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
