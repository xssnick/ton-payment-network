package chain

import (
	"context"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"sync"
	"sync/atomic"
	"time"
)

type Scanner struct {
	api       ton.APIClientWrapped
	client    *payments.Client
	codeHash  []byte
	lastBlock uint32

	taskPool       chan accFetchTask
	shardLastSeqno map[string]uint32

	globalCtx context.Context
	stopper   func()

	mx sync.RWMutex
}

type Contract struct {
	Name     string
	CodeHash []byte
}

func NewScanner(api ton.APIClientWrapped, codeHash []byte, lastBlock uint32) *Scanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scanner{
		api:            api,
		client:         payments.NewPaymentChannelClient(api),
		codeHash:       codeHash,
		lastBlock:      lastBlock,
		taskPool:       make(chan accFetchTask, 1000),
		shardLastSeqno: map[string]uint32{},
		globalCtx:      ctx,
		stopper:        cancel,
	}
}

func (v *Scanner) GetLag(ctx context.Context) (uint64, error) {
	master, err := v.api.GetMasterchainInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("get masterchain info err: %w", err)
	}
	return uint64(master.SeqNo - v.lastBlock), nil
}

func (v *Scanner) Stop() {
	v.stopper()
	// TODO: wait for completion
}

func (v *Scanner) Start(ctx context.Context, ch chan<- any) error {
	master, err := v.api.GetMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("get masterchain info err: %w", err)
	}

	go v.accFetcherWorker(ch, 60)

	if v.lastBlock > 0 {
		master, err = v.api.LookupBlock(ctx, master.Workchain, master.Shard, v.lastBlock)
		if err != nil {
			return fmt.Errorf("lookup block err: %w", err)
		}
	}
	v.lastBlock = master.SeqNo

	firstShards, err := v.api.GetBlockShardsInfo(ctx, master)
	if err != nil {
		return fmt.Errorf("get shards err: %w", err)
	}
	for _, shard := range firstShards {
		v.shardLastSeqno[getShardID(shard)] = shard.SeqNo
	}

	masters := []*ton.BlockIDExt{master}
	go func() {
		for {
			start := time.Now()

			var transactionsNum, shardBlocksNum uint64
			wg := sync.WaitGroup{}
			wg.Add(len(masters))
			for _, m := range masters {
				go func(m *ton.BlockIDExt) {
					txNum, bNum := v.fetchBlock(context.Background(), m)
					atomic.AddUint64(&transactionsNum, txNum)
					atomic.AddUint64(&shardBlocksNum, bNum)
					wg.Done()
				}(m)
			}

			wg.Wait()
			took := time.Since(start)

			for _, m := range masters {
				ch <- tonpayments.BlockCheckedEvent{
					Seqno: m.SeqNo,
				}
			}

			lastProcessed := masters[len(masters)-1]
			blocksNum := len(masters)
			masters = masters[:0]

			for {
				lastMaster, err := v.api.WaitForBlock(lastProcessed.SeqNo + 1).GetMasterchainInfo(ctx)
				if err != nil {
					log.Debug().Err(err).Uint32("seqno", lastProcessed.SeqNo+1).Msg("failed to get last block")
					time.Sleep(1 * time.Second)
					continue
				}

				if lastMaster.SeqNo <= lastProcessed.SeqNo {
					time.Sleep(1 * time.Second)
					continue
				}

				diff := lastMaster.SeqNo - lastProcessed.SeqNo
				if diff > 60 {
					rd := took.Round(time.Millisecond)
					if shardBlocksNum > 0 {
						rd /= time.Duration(shardBlocksNum)
					}

					log.Warn().Uint32("lag_master_blocks", diff).
						Int("processed_master_blocks", blocksNum).
						Uint64("processed_shard_blocks", shardBlocksNum).
						Uint64("processed_transactions", transactionsNum).
						Dur("took_ms_per_block", rd).
						Msg("chain scanner is out of sync")
				}
				log.Debug().Uint32("lag_master_blocks", diff).Uint64("processed_transactions", transactionsNum).Msg("scanner delay")

				if diff > 100 {
					diff = 100
				}

				for i := lastProcessed.SeqNo + 1; i <= lastProcessed.SeqNo+diff; i++ {
					for {
						nextMaster, err := v.api.WaitForBlock(i).LookupBlock(ctx, lastProcessed.Workchain, lastProcessed.Shard, i)
						if err != nil {
							log.Debug().Err(err).Uint32("seqno", i).Msg("failed to get next block")
							time.Sleep(1 * time.Second)
							continue
						}
						masters = append(masters, nextMaster)
						break
					}
				}
				v.lastBlock = masters[len(masters)-1].SeqNo
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
								log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
								break
							}

							qCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
							acc, err = v.api.WaitForBlock(task.master.SeqNo).GetAccount(qCtx, task.master, task.addr)
							cancel()
							if err != nil {
								log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to get account")
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
							log.Warn().Err(err).Str("addr", task.addr.String()).Msg("failed to parse payment channel")
						}
						return
					}

					var tx *tlb.Transaction
					{
						ctx := context.Background()
						for z := 0; z < 20; z++ { // TODO: retry without loosing
							ctx, err = v.api.Client().StickyContextNextNode(ctx)
							if err != nil {
								log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to pick next node")
								break
							}

							qCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
							tx, err = v.api.WaitForBlock(task.master.SeqNo).GetTransaction(qCtx, task.shard, task.addr, task.lt)
							cancel()
							if err != nil {
								log.Debug().Err(err).Str("addr", task.addr.String()).Msg("failed to get transaction")
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

					ch <- tonpayments.ChannelUpdatedEvent{
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

func (v *Scanner) getNotSeenShards(ctx context.Context, api ton.APIClientWrapped, shard *ton.BlockIDExt, prevShards []*ton.BlockIDExt) (ret []*ton.BlockIDExt, lastTime time.Time, err error) {
	if shard.Workchain != 0 {
		// skip non basechain
		return nil, time.Time{}, nil
	}

	for _, prevShard := range prevShards {
		if shard.Equals(prevShard) {
			return nil, time.Time{}, nil
		}
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
		ext, _, err := v.getNotSeenShards(ctx, api, parent, prevShards)
		if err != nil {
			return nil, time.Time{}, err
		}
		ret = append(ret, ext...)
	}
	return append(ret, shard), genTime, nil
}

func (v *Scanner) fetchBlock(ctx context.Context, master *ton.BlockIDExt) (transactionsNum, shardBlocksNum uint64) {
	log.Debug().Uint32("seqno", master.SeqNo).Msg("scanning master")

	tm := time.Now()
	for {
		select {
		case <-v.globalCtx.Done():
			log.Warn().Uint32("master", master.SeqNo).Msg("scanner stopped")
			return
		case <-ctx.Done():
			log.Warn().Uint32("master", master.SeqNo).Msg("ctx done")
			return
		default:
		}

		prevMaster, err := v.api.WaitForBlock(master.SeqNo-1).LookupBlock(ctx, master.Workchain, master.Shard, master.SeqNo-1)
		if err != nil {
			log.Debug().Err(err).Uint32("seqno", master.SeqNo-1).Msg("failed to get prev master block")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		prevShards, err := v.api.GetBlockShardsInfo(ctx, prevMaster)
		if err != nil {
			log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get shards on block")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		// getting information about other work-chains and shards of master block
		currentShards, err := v.api.GetBlockShardsInfo(ctx, master)
		if err != nil {
			log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get shards on block")
			time.Sleep(300 * time.Millisecond)
			continue
		}
		log.Debug().Uint32("seqno", master.SeqNo).Dur("took", time.Since(tm)).Msg("shards fetched")

		// shards in master block may have holes, e.g. shard seqno 2756461, then 2756463, and no 2756462 in master chain
		// thus we need to scan a bit back in case of discovering a hole, till last seen, to fill the misses.
		var newShards []*ton.BlockIDExt
		for _, shard := range currentShards {
			for {
				select {
				case <-v.globalCtx.Done():
					log.Warn().Uint32("master", master.SeqNo).Msg("scanner stopped")
					return
				case <-ctx.Done():
					log.Warn().Uint32("master", master.SeqNo).Msg("ctx done")
					return
				default:
				}

				notSeen, _, err := v.getNotSeenShards(ctx, v.api, shard, prevShards)
				if err != nil {
					log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get not seen shards on block")
					time.Sleep(300 * time.Millisecond)
					continue
				}

				newShards = append(newShards, notSeen...)
				break
			}
		}
		log.Debug().Uint32("seqno", master.SeqNo).Dur("took", time.Since(tm)).Msg("not seen shards fetched")

		var shardsWg sync.WaitGroup
		shardsWg.Add(len(newShards))
		shardBlocksNum = uint64(len(newShards))
		// for each shard block getting transactions
		for _, shard := range newShards {
			log.Debug().Uint32("seqno", shard.SeqNo).Uint64("shard", uint64(shard.Shard)).Int32("wc", shard.Workchain).Msg("scanning shard")

			go func(shard *ton.BlockIDExt) {
				defer shardsWg.Done()

				affectedAccounts := map[string]bool{}

				var fetchedIDs []ton.TransactionShortInfo
				var after *ton.TransactionID3
				var more = true

				// load all transactions in batches with 100 transactions in each while exists
				for more {
					select {
					case <-v.globalCtx.Done():
						log.Warn().Uint32("master", master.SeqNo).Msg("scanner stopped")
						return
					case <-ctx.Done():
						log.Warn().Uint32("master", master.SeqNo).Msg("ctx done")
						return
					default:
					}

					fetchedIDs, more, err = v.api.WaitForBlock(master.SeqNo).GetBlockTransactionsV2(ctx, shard, 100, after)
					if err != nil {
						log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get tx ids on block")
						time.Sleep(500 * time.Millisecond)
						continue
					}
					log.Debug().Uint32("seqno", shard.SeqNo).Uint64("shard", uint64(shard.Shard)).Int32("wc", shard.Workchain).Int("transactions", len(fetchedIDs)).Msg("scanning transactions")

					if more {
						// set load offset for next query (pagination)
						after = fetchedIDs[len(fetchedIDs)-1].ID3()
					}

					atomic.AddUint64(&transactionsNum, uint64(len(fetchedIDs)))

					wg := sync.WaitGroup{}
					for i := range fetchedIDs {
						addr := address.NewAddress(0, byte(shard.Workchain), fetchedIDs[i].Account)

						checked := affectedAccounts[addr.String()]
						if checked {
							// skip if already checked and it is not our account
							continue
						}

						wg.Add(1)
						v.taskPool <- accFetchTask{
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
			}(shard)
		}
		shardsWg.Wait()
		return
	}
}
