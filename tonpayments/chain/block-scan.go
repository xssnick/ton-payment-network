//go:build !(js && wasm)

package chain

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
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
	lastBlock uint32

	taskPool       chan accFetchTask
	shardLastSeqno map[string]uint32

	activeChannels map[string]context.CancelFunc

	globalCtx context.Context
	stopper   func()

	log zerolog.Logger

	mx sync.RWMutex
}

func NewScanner(api ton.APIClientWrapped, lastBlock uint32, lg zerolog.Logger) *Scanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scanner{
		api:            api,
		log:            lg,
		client:         payments.NewPaymentChannelClient(client.NewTON(api)),
		lastBlock:      lastBlock,
		taskPool:       make(chan accFetchTask, 1000),
		shardLastSeqno: map[string]uint32{},
		activeChannels: map[string]context.CancelFunc{},
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
		outOfSync := false
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
					v.log.Debug().Err(err).Uint32("seqno", lastProcessed.SeqNo+1).Msg("failed to get last block")
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

					v.log.Warn().Uint32("lag_master_blocks", diff).
						Int("processed_master_blocks", blocksNum).
						Uint64("processed_shard_blocks", shardBlocksNum).
						Uint64("processed_transactions", transactionsNum).
						Dur("took_ms_per_block", rd).
						Msg("chain scanner is out of sync")
					outOfSync = true
				} else if diff <= 1 && outOfSync {
					v.log.Info().Msg("chain scanner is synchronized")
					outOfSync = false
				}

				v.log.Debug().Uint32("lag_master_blocks", diff).Uint64("processed_transactions", transactionsNum).Msg("scanner delay")

				if diff > 100 {
					diff = 100
				}

				for i := lastProcessed.SeqNo + 1; i <= lastProcessed.SeqNo+diff; i++ {
					for {
						nextMaster, err := v.api.WaitForBlock(i).LookupBlock(ctx, lastProcessed.Workchain, lastProcessed.Shard, i)
						if err != nil {
							v.log.Debug().Err(err).Uint32("seqno", i).Msg("failed to get next block")
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

	parents, err := ton.GetParentBlocks(&b.BlockInfo)
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
	v.log.Debug().Uint32("seqno", master.SeqNo).Msg("scanning master")

	tm := time.Now()
	for {
		select {
		case <-v.globalCtx.Done():
			v.log.Warn().Uint32("master", master.SeqNo).Msg("scanner stopped")
			return
		case <-ctx.Done():
			v.log.Warn().Uint32("master", master.SeqNo).Msg("ctx done")
			return
		default:
		}

		prevMaster, err := v.api.WaitForBlock(master.SeqNo-1).LookupBlock(ctx, master.Workchain, master.Shard, master.SeqNo-1)
		if err != nil {
			v.log.Debug().Err(err).Uint32("seqno", master.SeqNo-1).Msg("failed to get prev master block")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		prevShards, err := v.api.GetBlockShardsInfo(ctx, prevMaster)
		if err != nil {
			v.log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get shards on block")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		// getting information about other work-chains and shards of master block
		currentShards, err := v.api.GetBlockShardsInfo(ctx, master)
		if err != nil {
			v.log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get shards on block")
			time.Sleep(300 * time.Millisecond)
			continue
		}
		v.log.Debug().Uint32("seqno", master.SeqNo).Dur("took", time.Since(tm)).Msg("shards fetched")

		// shards in master block may have holes, e.g. shard seqno 2756461, then 2756463, and no 2756462 in master chain
		// thus we need to scan a bit back in case of discovering a hole, till last seen, to fill the misses.
		var newShards []*ton.BlockIDExt
		for _, shard := range currentShards {
			for {
				select {
				case <-v.globalCtx.Done():
					v.log.Warn().Uint32("master", master.SeqNo).Msg("scanner stopped")
					return
				case <-ctx.Done():
					v.log.Warn().Uint32("master", master.SeqNo).Msg("ctx done")
					return
				default:
				}

				notSeen, _, err := v.getNotSeenShards(ctx, v.api, shard, prevShards)
				if err != nil {
					v.log.Debug().Err(err).Uint32("master", master.SeqNo).Msg("failed to get not seen shards on block")
					time.Sleep(300 * time.Millisecond)
					continue
				}

				newShards = append(newShards, notSeen...)
				break
			}
		}
		v.log.Debug().Uint32("seqno", master.SeqNo).Dur("took", time.Since(tm)).Msg("not seen shards fetched")

		var shardsWg sync.WaitGroup
		shardsWg.Add(len(newShards))
		shardBlocksNum = uint64(len(newShards))
		// for each shard block getting transactions
		for _, shard := range newShards {
			v.log.Debug().Uint32("seqno", shard.SeqNo).Str("shard", shardHex(uint64(shard.Shard))).Int32("wc", shard.Workchain).Msg("scanning shard")

			go func(shard *ton.BlockIDExt) {
				defer shardsWg.Done()

				var block *tlb.Block
				{
					ctx := ctx
					for z := 0; z < 20; z++ { // TODO: retry without loosing
						ctx, err = v.api.Client().StickyContextNextNode(ctx)
						if err != nil {
							v.log.Debug().Err(err).Uint32("master", master.SeqNo).Str("shard", shardHex(uint64(shard.Shard))).
								Uint32("shard_seqno", shard.SeqNo).Msg("failed to pick next node")
							break
						}

						qCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
						block, err = v.api.WaitForBlock(master.SeqNo).GetBlockData(qCtx, shard)
						cancel()
						if err != nil {
							v.log.Debug().Err(err).Uint32("master", master.SeqNo).Str("shard", shardHex(uint64(shard.Shard))).
								Uint32("shard_seqno", shard.SeqNo).Msg("failed to get block")
							time.Sleep(200 * time.Millisecond)
							continue
						}
						break
					}
				}

				err = func() error {
					if block == nil {
						return fmt.Errorf("failed to fetch block")
					}

					shr := block.Extra.ShardAccountBlocks.BeginParse()
					shardAccBlocks, err := shr.LoadDict(256)
					if err != nil {
						return fmt.Errorf("faled to load shard account blocks dict: %w", err)
					}

					var wg sync.WaitGroup

					sab := shardAccBlocks.All()
					for _, kv := range sab {
						slc := kv.Value.BeginParse()
						if err = tlb.LoadFromCell(&tlb.CurrencyCollection{}, slc); err != nil {
							return fmt.Errorf("faled to load aug currency collection of account block dict: %w", err)
						}

						var ab tlb.AccountBlock
						if err = tlb.LoadFromCell(&ab, slc); err != nil {
							return fmt.Errorf("faled to parse account block: %w", err)
						}

						allTx := ab.Transactions.All()
						transactionsNum += uint64(len(allTx))
						for _, txKV := range allTx {
							slcTx := txKV.Value.BeginParse()
							if err = tlb.LoadFromCell(&tlb.CurrencyCollection{}, slcTx); err != nil {
								return fmt.Errorf("faled to load aug currency collection of transactions dict: %w", err)
							}

							var tx tlb.Transaction
							if err = tlb.LoadFromCell(&tx, slcTx.MustLoadRef()); err != nil {
								return fmt.Errorf("faled to parse transaction: %w", err)
							}

							wg.Add(1)
							v.taskPool <- accFetchTask{
								master:   master,
								tx:       &tx,
								addr:     address.NewAddress(0, byte(shard.Workchain), ab.Addr),
								callback: wg.Done,
							}
							// 1 tx for account is enough for us, as a reference
							break
						}
					}

					v.log.Debug().Uint32("seqno", shard.SeqNo).
						Str("shard", shardHex(uint64(shard.Shard))).
						Int32("wc", shard.Workchain).
						Int("affected_accounts", len(sab)).
						Uint64("transactions", transactionsNum).
						Msg("scanning transactions")

					wg.Wait()
					return nil
				}()
				if err != nil {
					v.log.Error().Uint32("seqno", shard.SeqNo).Str("shard", shardHex(uint64(shard.Shard))).Int32("wc", shard.Workchain).Msg("failed to parse block, skipping. Fix issue and rescan later")
				}
			}(shard)
		}
		shardsWg.Wait()
		return
	}
}

func shardHex(shard uint64) string {
	v := make([]byte, 8)
	binary.LittleEndian.PutUint64(v, shard)
	return hex.EncodeToString(v)
}
