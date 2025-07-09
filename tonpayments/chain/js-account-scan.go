//go:build js && wasm

package chain

import (
	"context"
	"errors"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"sync"
	"time"
)

type Scanner struct {
	ton            *client.TON
	client         *payments.Client
	activeChannels map[string]context.CancelFunc
	events         chan<- any

	globalCtx context.Context
	stopper   func()

	mx sync.RWMutex
}

func NewScanner(api *client.TON, events chan<- any) *Scanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scanner{
		events:         events,
		ton:            api,
		client:         payments.NewPaymentChannelClient(api),
		activeChannels: map[string]context.CancelFunc{},
		globalCtx:      ctx,
		stopper:        cancel,
	}
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
	for {
		ch := make(chan *client.Transaction, 1)
		go v.subscribeOnTransactions(ctx, addr, sinceLT, ch)

		for tx := range ch {
			log.Debug().Str("address", addr.String()).Msg("found new transaction, fetching account")

			var acc *client.Account
			for {
				var err error
				acc, err = v.ton.GetAccount(ctx, addr)
				if err != nil {
					log.Error().Err(err).Str("address", addr.String()).Msg("failed to fetch account")
					time.Sleep(1 * time.Second)
					continue
				}
				break
			}

			if !acc.IsActive || acc.Code == nil || acc.Data == nil {
				continue
			}

			p, err := v.client.ParseAsyncChannel(addr, acc.Code, acc.Data, true)
			if err != nil {
				if !errors.Is(err, payments.ErrVerificationNotPassed) {
					log.Warn().Err(err).Str("addr", addr.String()).Msg("failed to parse payment channel")
				}
				continue
			}

			log.Debug().Str("address", addr.String()).Msg("account fetched and parsed, reporting channel update event")

			v.events <- tonpayments.ChannelUpdatedEvent{
				Transaction: tx,
				Channel:     p,
			}
		}
	}
}

func (v *Scanner) subscribeOnTransactions(workerCtx context.Context, addr *address.Address, lastProcessedLT uint64, channel chan<- *client.Transaction) {
	defer close(channel)

	wait := 0 * time.Second
	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-workerCtx.Done():
			return
		case <-timer.C:
		}
		wait = 3 * time.Second

		ctx, cancel := context.WithTimeout(workerCtx, 10*time.Second)
		acc, err := v.ton.GetAccount(ctx, addr)
		cancel()
		if err != nil {
			timer.Reset(wait)
			continue
		}
		if !acc.IsActive || acc.LastTxLT == 0 {
			// no transactions
			timer.Reset(wait)
			continue
		}

		if lastProcessedLT == acc.LastTxLT {
			// already processed all
			timer.Reset(wait)
			continue
		}

		var transactions []client.Transaction
		lastHash, lastLT := acc.LastTxHash, acc.LastTxLT

		waitList := 0 * time.Second
		listTimer := time.NewTimer(waitList)

	list:
		for {
			select {
			case <-workerCtx.Done():
				listTimer.Stop()
				return
			case <-listTimer.C:
			}

			if lastLT == 0 {
				// exhausted all transactions
				break
			}

			ctx, cancel = context.WithTimeout(workerCtx, 10*time.Second)
			res, err := v.ton.GetTransactionsList(ctx, addr, lastLT, lastHash)
			cancel()
			if err != nil {
				waitList = 3 * time.Second
				listTimer.Reset(waitList)
				continue
			}

			if len(res) == 0 {
				break
			}

			// reverse slice
			for i, j := 0, len(res)-1; i < j; i, j = i+1, j-1 {
				res[i], res[j] = res[j], res[i]
			}

			for i, tx := range res {
				if tx.LT <= lastProcessedLT {
					transactions = append(transactions, res[:i]...)
					break list
				}
			}

			lastLT, lastHash = res[len(res)-1].PrevTxLT, res[len(res)-1].PrevTxHash
			transactions = append(transactions, res...)
			waitList = 0 * time.Second
			listTimer.Reset(waitList)
		}

		listTimer.Stop()

		if len(transactions) > 0 {
			lastProcessedLT = transactions[0].LT // mark last transaction as known to not trigger twice

			// reverse slice to send in correct time order (from old to new)
			for i, j := 0, len(transactions)-1; i < j; i, j = i+1, j-1 {
				transactions[i], transactions[j] = transactions[j], transactions[i]
			}

			for _, tx := range transactions {
				channel <- &tx
			}

			wait = 0 * time.Second
		}

		timer.Reset(wait)
	}
}
