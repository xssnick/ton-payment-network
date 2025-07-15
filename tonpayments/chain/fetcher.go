//go:build !(js && wasm)

package chain

import (
	"context"
	"errors"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"time"
)

type accFetchTask struct {
	master   *ton.BlockIDExt
	tx       *tlb.Transaction
	addr     *address.Address
	callback func()
}

func (v *Scanner) accFetcherWorker(ch chan<- any, threads int) {
	for y := 0; y < threads; y++ {
		go func() {
			for {
				task := <-v.taskPool

				func() {
					defer task.callback()

					var acc *tlb.Account
					{
						ctx := context.Background()
						for i := 0; i < 20; i++ { // TODO: retry without loosing
							var err error
							ctx, err = v.api.Client().StickyContextNextNode(ctx)
							if err != nil {
								v.log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
								break
							}

							qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
							acc, err = v.api.WaitForBlock(task.master.SeqNo).GetAccount(qCtx, task.master, task.addr)
							cancel()
							if err != nil {
								v.log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to get account")
								time.Sleep(100 * time.Millisecond)
								continue
							}
							break
						}
					}

					if acc == nil || !acc.IsActive || acc.State.Status != tlb.AccountStatusActive || acc.Code == nil || acc.Data == nil {
						// not active or failed
						return
					}

					p, err := v.client.ParseAsyncChannel(task.addr, acc.Code, acc.Data, true)
					if err != nil {
						if !errors.Is(err, payments.ErrVerificationNotPassed) {
							v.log.Warn().Err(err).Str("addr", task.addr.String()).Msg("failed to parse payment channel")
						}
						return
					}

					log.Debug().Str("address", task.addr.String()).Msg("account fetched and parsed, reporting channel update event")

					res := &client.Transaction{
						PrevTxLT:   task.tx.PrevTxLT,
						Hash:       task.tx.Hash,
						PrevTxHash: task.tx.PrevTxHash,
						LT:         task.tx.LT,
						At:         int64(task.tx.Now),
					}

					if desc, ok := task.tx.Description.(tlb.TransactionDescriptionOrdinary); ok {
						if comp, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM); ok && comp.Success {
							res.Success = true
							if task.tx.IO.In.MsgType == tlb.MsgTypeInternal {
								res.InternalInBody = task.tx.IO.In.AsInternal().Body
							}
						}
					}

					ch <- tonpayments.ChannelUpdatedEvent{
						Transaction: res,
						Channel:     p,
					}
				}()
			}
		}()
	}
}
