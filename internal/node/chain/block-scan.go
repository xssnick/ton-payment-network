package chain

import (
	"context"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"sync"
	"time"
)

type Scanner struct {
	api       ton.APIClientWrapped
	client    *payments.Client
	codeHash  []byte
	lastBlock uint32
}

type Contract struct {
	Name     string
	CodeHash []byte
}

func NewScanner(api ton.APIClientWrapped, codeHash []byte, lastBlock uint32) *Scanner {
	return &Scanner{api: api, codeHash: codeHash, lastBlock: lastBlock, client: payments.NewPaymentChannelClient(api)}
}

func (v *Scanner) GetLag(ctx context.Context) (uint64, error) {
	master, err := v.api.GetMasterchainInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("get masterchain info err: %w", err)
	}
	return uint64(master.SeqNo - v.lastBlock), nil
}

func (v *Scanner) Start(ctx context.Context, ch chan<- any) error {
	master, err := v.api.GetMasterchainInfo(ctx)

	taskPool := make(chan accFetchTask, 100)
	go v.accFetcherWorker(ch, taskPool, 20)

	if err != nil {
		return fmt.Errorf("get masterchain info err: %w", err)
	}

	if v.lastBlock > 0 {
		master, err = v.api.LookupBlock(ctx, master.Workchain, master.Shard, v.lastBlock)
		if err != nil {
			return fmt.Errorf("lookup block err: %w", err)
		}
	}
	v.lastBlock = master.SeqNo

	// storage for last seen shard seqno
	shardLastSeqno := map[string]uint32{}

	firstShards, err := v.api.GetBlockShardsInfo(ctx, master)
	if err != nil {
		return fmt.Errorf("get shards err: %w", err)
	}
	for _, shard := range firstShards {
		shardLastSeqno[getShardID(shard)] = shard.SeqNo
	}

	go func() {
		for {
			// log.Debug().Uint32("seqno", master.SeqNo).Msg("scanning master")

			// getting information about other work-chains and shards of master block
			currentShards, err := v.api.GetBlockShardsInfo(ctx, master)
			if err != nil {
				log.Error().Err(err).Uint32("master", master.SeqNo).Msg("failed to get shards on block")
				time.Sleep(500 * time.Millisecond)
				continue
			}
			// log.Debug().Uint32("seqno", master.SeqNo).Dur("took", time.Since(tm)).Msg("shards fetched")

			// shards in master block may have holes, e.g. shard seqno 2756461, then 2756463, and no 2756462 in master chain
			// thus we need to scan a bit back in case of discovering a hole, till last seen, to fill the misses.
			var newShards []*ton.BlockIDExt
			var blockTime time.Time
			for _, shard := range currentShards {
				for {
					notSeen, tm, err := getNotSeenShards(ctx, v.api, shard, shardLastSeqno)
					if err != nil {
						log.Error().Err(err).Uint32("master", master.SeqNo).Msg("failed to get not seen shards on block")
						time.Sleep(500 * time.Millisecond)
						continue
					}
					blockTime = tm
					newShards = append(newShards, notSeen...)
					shardLastSeqno[getShardID(shard)] = shard.SeqNo
					break
				}
			}
			// log.Debug().Uint32("seqno", master.SeqNo).Msg("not seen shards fetched")

			affectedAccounts := map[string]bool{}

			// for each shard block getting transactions
			for _, shard := range newShards {
				// log.Debug().Uint32("seqno", shard.SeqNo).Uint64("shard", uint64(shard.Shard)).Int32("wc", shard.Workchain).Msg("scanning shard")

				var fetchedIDs []ton.TransactionShortInfo
				var after *ton.TransactionID3
				var more = true

				// load all transactions in batches with 100 transactions in each while exists
				for more {
					fetchedIDs, more, err = v.api.WaitForBlock(master.SeqNo).GetBlockTransactionsV2(ctx, shard, 100, after)
					if err != nil {
						log.Error().Err(err).Uint32("master", master.SeqNo).Msg("failed to get tx ids on block")
						time.Sleep(500 * time.Millisecond)
						continue
					}

					if more {
						// set load offset for next query (pagination)
						after = fetchedIDs[len(fetchedIDs)-1].ID3()
					}

					lag := time.Since(blockTime).Round(10 * time.Millisecond).Seconds()
					if lag > 60 {
						log.Warn().Uint32("seqno", shard.SeqNo).Float64("lag", lag).Msg("chain scanner is out of sync")
					}
					log.Debug().Uint32("seqno", shard.SeqNo).Float64("lag", lag).Uint64("shard", uint64(shard.Shard)).Int32("wc", shard.Workchain).Int("transactions", len(fetchedIDs)).Msg("scanning transactions")

					wg := sync.WaitGroup{}
					for i := range fetchedIDs {
						addr := address.NewAddress(0, byte(shard.Workchain), fetchedIDs[i].Account)

						checked := affectedAccounts[addr.String()]
						if checked {
							// skip if already checked and it is not our account
							continue
						}

						wg.Add(1)
						taskPool <- accFetchTask{
							master:   master,
							shard:    shard,
							lt:       fetchedIDs[i].LT,
							addr:     addr,
							callback: wg.Done,
						}
						affectedAccounts[addr.String()] = true
					}
					wg.Wait()
				}
			}

			ch <- node.BlockCheckedEvent{
				Seqno: master.SeqNo,
			}

			for {
				nextMaster, err := v.api.WaitForBlock(master.SeqNo+1).LookupBlock(ctx, master.Workchain, master.Shard, master.SeqNo+1)
				if err != nil {
					log.Error().Err(err).Uint32("seqno", master.SeqNo+1).Msg("failed to get next block")
					time.Sleep(1 * time.Second)
					continue
				}
				master = nextMaster
				v.lastBlock = master.SeqNo
				break
			}
		}
	}()
	return nil
}

type accFetchTask struct {
	master   *ton.BlockIDExt
	shard    *ton.BlockIDExt
	lt       uint64
	addr     *address.Address
	callback func()
}

func (v *Scanner) accFetcherWorker(ch chan<- any, tasks chan accFetchTask, threads int) {
	for y := 0; y < threads; y++ {
		go func() {
			for {
				task := <-tasks

				func() {
					defer task.callback()

					var acc *tlb.Account
					{
						ctx := context.Background()
						for i := 0; i < 20; i++ {
							var err error
							ctx, err = v.api.Client().StickyContextNextNode(ctx)
							if err != nil {
								log.Error().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
								break
							}

							qCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
							acc, err = v.api.WaitForBlock(task.master.SeqNo).GetAccount(qCtx, task.master, task.addr)
							cancel()
							if err != nil {
								log.Error().Err(err).Str("addr", task.addr.String()).Msg("failed to get account")
								time.Sleep(100 * time.Millisecond)
								continue
							}
							break
						}
					}

					if acc == nil || !acc.IsActive || acc.State.Status != tlb.AccountStatusActive {
						// not active or failed
						return
					}

					p, err := v.client.ParseAsyncChannel(task.addr, acc.Code, acc.Data, true)
					if err != nil {
						if !errors.Is(err, payments.ErrVerificationNotPassed) {
							log.Error().Err(err).Str("addr", task.addr.String()).Msg("failed to parse payment channel")
						}
						return
					}

					var tx *tlb.Transaction
					{
						ctx := context.Background()
						for z := 0; z < 20; z++ {
							ctx, err = v.api.Client().StickyContextNextNode(ctx)
							if err != nil {
								log.Error().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
								break
							}

							qCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
							tx, err = v.api.WaitForBlock(task.master.SeqNo).GetTransaction(qCtx, task.shard, task.addr, task.lt)
							cancel()
							if err != nil {
								log.Error().Err(err).Str("addr", task.addr.String()).Msg("failed to get transaction")
								time.Sleep(200 * time.Millisecond)
								continue
							}
							break
						}
					}

					if tx == nil {
						// TODO: maybe fill with something
						return
					}

					ch <- node.ChannelUpdatedEvent{
						Transaction: tx,
						Channel:     p,
					}
				}()
			}
		}()
	}
}

// func to get storage map key
func getShardID(shard *ton.BlockIDExt) string {
	return fmt.Sprintf("%d|%d", shard.Workchain, shard.Shard)
}

func getNotSeenShards(ctx context.Context, api ton.APIClientWrapped, shard *ton.BlockIDExt, shardLastSeqno map[string]uint32) (ret []*ton.BlockIDExt, lastTime time.Time, err error) {
	if shard.Workchain != 0 {
		// skip non basechain
		return nil, time.Time{}, nil
	}

	if no, ok := shardLastSeqno[getShardID(shard)]; ok && no == shard.SeqNo {
		return nil, time.Time{}, nil
	}

	b, err := api.GetBlockData(ctx, shard)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get block data: %w", err)
	}

	parents, err := b.BlockInfo.GetParentBlocks()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get parent blocks (%d:%x:%d): %w", shard.Workchain, uint64(shard.Shard), shard.Shard, err)
	}

	genTime := time.Unix(int64(b.BlockInfo.GenUtime), 0)

	for _, parent := range parents {
		ext, _, err := getNotSeenShards(ctx, api, parent, shardLastSeqno)
		if err != nil {
			return nil, time.Time{}, err
		}
		ret = append(ret, ext...)
	}
	return append(ret, shard), genTime, nil
}
