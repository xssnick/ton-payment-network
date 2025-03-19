package chain

import (
	"context"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"log"
	"time"
)

func (v *Scanner) StartSmall(events chan<- any) error {
	go v.accFetcherWorker(events, 3)

	return nil
}

func (v *Scanner) OnChannelUpdate(ch *db.Channel) {
	v.mx.Lock()
	defer v.mx.Unlock()

	if ch.Status == db.ChannelStateInactive {
		if c := v.activeChannels[ch.Address]; c != nil {
			c() // stop listener
			delete(v.activeChannels, ch.Address)
		}
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

		go v.startForContract(ctx, address.MustParseAddr(ch.Address), lt)
	}
}

func (v *Scanner) startForContract(ctx context.Context, addr *address.Address, sinceLT uint64) {
	ch := make(chan *tlb.Transaction, 1)
	go v.api.SubscribeOnTransactions(ctx, addr, sinceLT, ch)

	for transaction := range ch {
		for {
			m, err := v.api.CurrentMasterchainInfo(ctx)
			if err != nil {
				time.Sleep(1 * time.Second)
				log.Println("failed to fetch master block, will retry in 1s")
				continue
			}

			println("REPORT", addr.String())
			v.taskPool <- accFetchTask{
				master:   m,
				tx:       transaction,
				addr:     addr,
				callback: func() {},
			}
			break
		}
	}
}
