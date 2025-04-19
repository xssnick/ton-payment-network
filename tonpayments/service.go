package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/metrics"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"sync"
	"time"
)

var ErrNotActive = errors.New("channel is not active")
var ErrDenied = errors.New("actions denied")
var ErrChannelIsBusy = errors.New("channel is busy")
var ErrNotPossible = errors.New("not possible")

const PaymentsTaskPool = "pn"

type Transport interface {
	AddUrgentPeer(channelKey ed25519.PublicKey)
	RemoveUrgentPeer(channelKey ed25519.PublicKey)
	ProposeChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey, prop transport.ProposeChannelConfig) (*address.Address, error)
	RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action transport.Action) (*transport.Decision, error)
	ProposeAction(ctx context.Context, lockId int64, channelAddr *address.Address, theirChannelKey []byte, state, updateProof *cell.Cell, action transport.Action) (*transport.ProposalDecision, error)
	RequestChannelLock(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64, lock bool) (*transport.Decision, error)
	IsChannelUnlocked(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64) (*transport.Decision, error)
}

type Webhook interface {
	PushChannelEvent(ctx context.Context, ch *db.Channel) error
	PushVirtualChannelEvent(ctx context.Context, event db.VirtualChannelEventType, meta *db.VirtualChannelMeta) error
}

type DB interface {
	Transaction(ctx context.Context, f func(ctx context.Context) error) error
	CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context, poolName string) (*db.Task, error)
	RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, poolName string, task *db.Task) error
	ListActiveTasks(ctx context.Context, poolName string) ([]*db.Task, error)

	GetVirtualChannelMeta(ctx context.Context, key []byte) (*db.VirtualChannelMeta, error)
	UpdateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error
	CreateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error

	SetBlockOffset(ctx context.Context, seqno uint32) error
	GetBlockOffset(ctx context.Context) (*db.BlockOffset, error)

	GetChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error)
	CreateChannel(ctx context.Context, channel *db.Channel) error
	GetChannel(ctx context.Context, addr string) (*db.Channel, error)
	UpdateChannel(ctx context.Context, channel *db.Channel) error
	SetOnChannelUpdated(f func(ctx context.Context, ch *db.Channel, statusChanged bool))
	GetOnChannelUpdated() func(ctx context.Context, ch *db.Channel, statusChanged bool)

	GetUrgentPeers(ctx context.Context) ([][]byte, error)
	AddUrgentPeer(ctx context.Context, peerAddress []byte) error
}

type BlockCheckedEvent struct {
	Seqno uint32
}

type ChannelUpdatedEvent struct {
	Transaction *tlb.Transaction
	Channel     *payments.AsyncChannel
}

type channelLock struct {
	id    int64
	queue chan bool
	mx    sync.Mutex

	// pending bool
}

type balanceControlConfig struct {
	DepositWhenAmountLessThan tlb.Coins
	DepositUpToAmount         tlb.Coins
	WithdrawWhenAmountReached tlb.Coins

	depositStartedAtBalance  *big.Int
	withdrawStartedAtBalance *big.Int
}

type Service struct {
	ton       ton.APIClientWrapped
	transport Transport
	updates   chan any
	db        DB
	webhook   Webhook

	key ed25519.PrivateKey

	wallet                         *wallet.Wallet
	contractMaker                  *payments.Client
	virtualChannelsLimitPerChannel int
	workerSignal                   chan bool

	cfg config.ChannelsConfig

	// TODO: channel based lock
	mx sync.Mutex

	externalLock func()

	channelLocks     map[string]*channelLock
	lockerMx         sync.Mutex
	externalLockerMx sync.Mutex

	supportedJettons   map[string]config.CoinConfig
	supportedEC        map[uint32]config.CoinConfig
	supportedTon       bool
	balanceControllers map[string]*balanceControlConfig
	urgentPeers        map[string]bool

	globalCtx    context.Context
	globalCancel context.CancelFunc

	urgentPeersMx sync.RWMutex
	discoveryMx   sync.Mutex
}

func NewService(api ton.APIClientWrapped, database DB, transport Transport, wallet *wallet.Wallet, updates chan any, key ed25519.PrivateKey, cfg config.ChannelsConfig) (*Service, error) {
	globalCtx, globalCancel := context.WithCancel(context.Background())
	s := &Service{
		ton:                            api,
		transport:                      transport,
		updates:                        updates,
		db:                             database,
		key:                            key,
		wallet:                         wallet,
		contractMaker:                  payments.NewPaymentChannelClient(api),
		cfg:                            cfg,
		virtualChannelsLimitPerChannel: 30000,
		workerSignal:                   make(chan bool, 1),
		channelLocks:                   map[string]*channelLock{},
		supportedJettons:               map[string]config.CoinConfig{},
		supportedEC:                    map[uint32]config.CoinConfig{},
		supportedTon:                   cfg.SupportedCoins.Ton.Enabled,
		balanceControllers:             map[string]*balanceControlConfig{},
		urgentPeers:                    map[string]bool{},
		globalCtx:                      globalCtx,
		globalCancel:                   globalCancel,
	}

	addBalanceControl := func(jetton string, ecID uint32, currency config.CoinConfig) error {
		conf := &balanceControlConfig{
			DepositWhenAmountLessThan: tlb.MustFromDecimal(currency.BalanceControl.DepositWhenAmountLessThan, int(currency.Decimals)),
			DepositUpToAmount:         tlb.MustFromDecimal(currency.BalanceControl.DepositUpToAmount, int(currency.Decimals)),
			WithdrawWhenAmountReached: tlb.MustFromDecimal(currency.BalanceControl.WithdrawWhenAmountReached, int(currency.Decimals)),
		}

		if conf.WithdrawWhenAmountReached.Nano().Sign() != 0 &&
			conf.DepositUpToAmount.Nano().Sign() != 0 && conf.WithdrawWhenAmountReached.Compare(&conf.DepositUpToAmount) < 0 {
			return fmt.Errorf("withdraw amount must be greater than deposit amount")
		}

		if conf.DepositWhenAmountLessThan.Nano().Sign() != 0 &&
			conf.DepositUpToAmount.Nano().Sign() != 0 && conf.DepositWhenAmountLessThan.Compare(&conf.DepositUpToAmount) > 0 {
			return fmt.Errorf("deposit up to amount must be greater than deposit when amount less than")
		}

		s.balanceControllers[ccToKey(jetton, ecID)] = conf
		return nil
	}

	var balanceControl bool
	for addr, currency := range cfg.SupportedCoins.Jettons {
		if !currency.Enabled {
			continue
		}

		a, err := address.ParseAddr(addr)
		if err != nil {
			return nil, err
		}
		addr = a.Bounce(true).String()

		s.supportedJettons[addr] = currency

		if currency.BalanceControl != nil {
			balanceControl = true
			if err = addBalanceControl(addr, 0, currency); err != nil {
				return nil, err
			}
		}
	}

	for id, currency := range cfg.SupportedCoins.ExtraCurrencies {
		if !currency.Enabled {
			continue
		}

		if id == 0 {
			return nil, fmt.Errorf("extra currency id 0 is reserved")
		}

		s.supportedEC[id] = currency

		if currency.BalanceControl != nil {
			balanceControl = true
			if err := addBalanceControl("", id, currency); err != nil {
				return nil, err
			}
		}
	}

	if cfg.SupportedCoins.Ton.BalanceControl != nil {
		balanceControl = true
		if err := addBalanceControl("", 0, cfg.SupportedCoins.Ton); err != nil {
			return nil, err
		}
	}

	if err := s.loadUrgentPeers(context.Background()); err != nil {
		return nil, err
	}

	if balanceControl {
		handler := s.balanceControlCallback
		if current := database.GetOnChannelUpdated(); current != nil {
			handler = func(ctx context.Context, ch *db.Channel, statusChanged bool) {
				current(ctx, ch, statusChanged)
				s.balanceControlCallback(ctx, ch, statusChanged)
			}
		}
		database.SetOnChannelUpdated(handler)

		go func() {
			// some startup delay for indexing
			time.Sleep(10 * time.Second)

			channels, err := s.ListChannels(context.Background(), nil, db.ChannelStateActive)
			if err != nil {
				log.Error().Err(err).Msg("failed to list active channels")
				return
			}

			for _, ch := range channels {
				s.balanceControlCallback(context.Background(), ch, false)
			}
		}()
	}

	return s, nil
}

func ccToKey(jetton string, ecID uint32) string {
	return "J:" + jetton + "E:" + fmt.Sprint(ecID)
}

func (s *Service) Stop() {
	s.globalCancel()
}

func (s *Service) AddUrgentPeer(ctx context.Context, peer []byte) error {
	if err := s.db.AddUrgentPeer(ctx, peer); err != nil {
		return err
	}

	s.urgentPeersMx.Lock()
	s.urgentPeers[hex.EncodeToString(peer)] = true
	s.urgentPeersMx.Unlock()

	s.transport.AddUrgentPeer(peer)
	return nil
}

func (s *Service) loadUrgentPeers(ctx context.Context) error {
	peers, err := s.db.GetUrgentPeers(ctx)
	if err != nil {
		return err
	}

	for _, peer := range peers {
		s.urgentPeersMx.Lock()
		s.urgentPeers[hex.EncodeToString(peer)] = true
		s.urgentPeersMx.Unlock()

		s.transport.AddUrgentPeer(peer)
	}
	return nil
}

func (s *Service) balanceControlCallback(ctx context.Context, ch *db.Channel, _ bool) {
	if ch.Status != db.ChannelStateActive {
		return
	}

	bc := s.balanceControllers[ccToKey(ch.JettonAddress, ch.ExtraCurrencyID)]
	if bc == nil {
		return
	}

	balance, err := ch.CalcBalance(false)
	if err != nil {
		log.Error().Str("address", ch.Address).Err(err).Msg("failed to calc our balance in balance controller")
		return
	}

	if balance.Cmp(bc.DepositWhenAmountLessThan.Nano()) < 0 {
		if bc.depositStartedAtBalance == nil || bc.depositStartedAtBalance.Cmp(ch.OurOnchain.Deposited) < 0 {
			amt := tlb.MustFromNano(new(big.Int).Sub(bc.DepositUpToAmount.Nano(), balance), bc.DepositUpToAmount.Decimals())
			if err = s.TopupChannel(ctx, address.MustParseAddr(ch.Address), amt); err != nil {
				log.Error().Err(err).Str("address", ch.Address).Str("amount", amt.String()).Msg("failed to topup channel")
				return
			}
			bc.depositStartedAtBalance = new(big.Int).Set(ch.OurOnchain.Deposited)
		}
	} else if bc.WithdrawWhenAmountReached.Nano().Sign() > 0 && balance.Cmp(bc.WithdrawWhenAmountReached.Nano()) > 0 {
		if bc.withdrawStartedAtBalance == nil || bc.withdrawStartedAtBalance.Cmp(ch.OurOnchain.Withdrawn) < 0 {
			amt := tlb.MustFromNano(new(big.Int).Sub(balance, bc.DepositUpToAmount.Nano()), bc.DepositUpToAmount.Decimals())
			if err = s.requestWithdraw(ctx, ch, amt); err != nil {
				log.Error().Err(err).Str("address", ch.Address).Str("amount", amt.String()).Msg("failed to withdraw from channel")
				return
			}
			bc.withdrawStartedAtBalance = new(big.Int).Set(ch.OurOnchain.Withdrawn)
		}
	}
}

func (s *Service) SetWebhook(webhook Webhook) {
	s.webhook = webhook
}

func (s *Service) GetPrivateKey() ed25519.PrivateKey {
	return s.key
}

func (s *Service) GetMinSafeTTL() time.Duration {
	return time.Duration(s.cfg.MinSafeVirtualChannelTimeoutSec+s.cfg.BufferTimeToCommit+s.cfg.ConditionalCloseDurationSec+s.cfg.QuarantineDurationSec) * time.Second
}

func (s *Service) ReviewChannelConfig(prop transport.ProposeChannelConfig) (*address.Address, error) {
	var jetton *address.Address
	if !bytes.Equal(prop.JettonAddr, make([]byte, 32)) {
		jetton = address.NewAddress(0, 0, prop.JettonAddr)
	}

	if jetton != nil && prop.ExtraCurrencyID != 0 {
		return nil, fmt.Errorf("both extra currency and jetton are set")
	}

	var cfg = s.cfg.SupportedCoins.Ton
	if prop.ExtraCurrencyID != 0 {
		if c, ok := s.supportedEC[prop.ExtraCurrencyID]; !ok {
			return nil, fmt.Errorf("extra currency is not whitelisted")
		} else {
			cfg = c
		}
	}

	if jetton != nil {
		if c, ok := s.supportedJettons[jetton.Bounce(true).String()]; !ok {
			return nil, fmt.Errorf("jetton currency is not whitelisted")
		} else {
			cfg = c
		}
	}

	ourFine := tlb.MustFromDecimal(cfg.MisbehaviorFine, int(cfg.Decimals))

	if prop.QuarantineDuration != s.cfg.QuarantineDurationSec ||
		prop.ConditionalCloseDuration != s.cfg.ConditionalCloseDurationSec ||
		new(big.Int).SetBytes(prop.MisbehaviorFine).Cmp(ourFine.Nano()) != 0 {
		return nil, fmt.Errorf("node wants different channel config: quarantine %d, cond close %d, fine %s; if you wnat to deploy", s.cfg.QuarantineDurationSec, s.cfg.ConditionalCloseDurationSec, ourFine.String())
	}

	return s.wallet.WalletAddress(), nil
}

func (s *Service) scanSettledConditionals(channelAddr string, tx *tlb.Transaction, settle payments.SettleConditionals) {
	kvs, err := settle.Signed.ConditionalsToSettle.LoadAll()
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr).Msg("failed to load settled conditionals")
		return
	}

	proofBody, err := settle.Signed.ConditionalsProof.PeekRef(0)
	if err != nil {
		log.Warn().Err(err).Str("address", channelAddr).Msg("failed to load settled conditionals proof")
		return
	}

	proofDict := proofBody.AsDict(32)

	for _, kv := range kvs {
		key, err := kv.Key.LoadUInt(32)
		if err != nil {
			log.Warn().Err(err).Str("address", channelAddr).Msg("failed to load settled condition key")
			continue
		}

		var state payments.VirtualChannelState
		if err = tlb.LoadFromCell(&state, kv.Value); err != nil {
			log.Warn().Err(err).Str("address", channelAddr).Uint64("id", key).Msg("failed to load condition state")
			continue
		}

		cond, err := proofDict.LoadValueByIntKey(big.NewInt(int64(key)))
		if err != nil {
			log.Warn().Err(err).Str("address", channelAddr).Uint64("id", key).Msg("failed to load condition code")
			continue
		}

		vch, err := payments.ParseVirtualChannelCond(cond)
		if err != nil {
			log.Warn().Err(err).Str("address", channelAddr).Uint64("id", key).Msg("failed to parse condition")
			continue
		}

		if err = s.AddVirtualChannelResolve(context.Background(), vch.Key, state); err != nil {
			log.Warn().Err(err).Str("address", channelAddr).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to add virtual channel resolve")
			continue
		}

		// close next virtual channels since they commited latest resolve onchain
		if err = s.CloseVirtualChannel(context.Background(), vch.Key); err != nil {
			if !errors.Is(err, ErrCannotCloseOngoingVirtual) {
				log.Warn().Err(err).Str("address", channelAddr).
					Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to create task for close virtual channel")
			}
			continue
		}
	}

	log.Info().Str("address", channelAddr).Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("settlement transaction with condition resolves processed")
}

func (s *Service) Start() {
	if err := s.addPeersForChannels(); err != nil {
		log.Error().Err(err).Msg("failed to add urgent peers")
	}

	go s.taskExecutor()
	if metrics.Registered {
		go s.channelsMonitor()
		go s.walletMonitor()
	}

	for {
		var update any
		select {
		case <-s.globalCtx.Done():
			return
		case update = <-s.updates:
		}

		switch upd := update.(type) {
		case BlockCheckedEvent:
			if err := s.db.SetBlockOffset(context.Background(), upd.Seqno); err != nil {
				log.Error().Err(err).Uint32("seqno", upd.Seqno).Msg("failed to update master seqno in db")
				continue
			}
		case ChannelUpdatedEvent:
			ok, isLeft := s.verifyChannel(upd.Channel)
			if !ok {
				log.Debug().Any("channel", upd.Channel).Msg("not verified")
				continue
			}

			log.Debug().Any("channel", upd.Channel).Msg("verified")

		retry:
			var err error
			var channel *db.Channel
			for {
				select {
				case <-s.globalCtx.Done():
					return
				default:
				}

				// TODO: not block, DLQ?
				channel, err = s.db.GetChannel(context.Background(), upd.Channel.Address().String())
				if err != nil && !errors.Is(err, db.ErrNotFound) {
					log.Error().Err(err).Msg("failed to get channel from db, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}
				break
			}

			if desc, ok := upd.Transaction.Description.(tlb.TransactionDescriptionOrdinary); ok {
				if comp, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM); ok && comp.Success && upd.Transaction.IO.In.MsgType == tlb.MsgTypeInternal {
					msg := upd.Transaction.IO.In.AsInternal()

					var settle payments.SettleConditionals
					if err = tlb.LoadFromCell(&settle, msg.Body.BeginParse()); err == nil {
						if settle.IsFromA != isLeft {
							// we need to check their conditional resolves and add missing if any, to resolve our next channels
							go s.scanSettledConditionals(upd.Channel.Address().String(), upd.Transaction, settle)
						}
					}
				}
			}

			our := db.OnchainState{
				Key:              upd.Channel.Storage.KeyB,
				CommittedSeqno:   upd.Channel.Storage.CommittedSeqnoB,
				WalletAddress:    upd.Channel.Storage.PaymentConfig.DestB.String(),
				Deposited:        upd.Channel.Storage.Balance.DepositB.Nano(),
				Withdrawn:        upd.Channel.Storage.Balance.WithdrawB.Nano(),
				CommittedBalance: upd.Channel.Storage.Balance.BalanceB.Nano(),
			}
			their := db.OnchainState{
				Key:              upd.Channel.Storage.KeyA,
				CommittedSeqno:   upd.Channel.Storage.CommittedSeqnoA,
				WalletAddress:    upd.Channel.Storage.PaymentConfig.DestA.String(),
				Deposited:        upd.Channel.Storage.Balance.DepositA.Nano(),
				Withdrawn:        upd.Channel.Storage.Balance.WithdrawA.Nano(),
				CommittedBalance: upd.Channel.Storage.Balance.BalanceA.Nano(),
			}

			if isLeft {
				our, their = their, our
			}

			isNew := channel == nil
			if isNew || channel.Status == db.ChannelStateInactive {
				if upd.Channel.Status == payments.ChannelStatusUninitialized {
					continue
				}

				createAt := time.Now()
				var version int64
				if channel != nil {
					version = channel.DBVersion
					createAt = channel.CreatedAt
				}

				channel = &db.Channel{
					ID:                     upd.Channel.Storage.ChannelID,
					Address:                upd.Channel.Address().String(),
					WeLeft:                 isLeft,
					OurOnchain:             our,
					TheirOnchain:           their,
					Our:                    db.NewSide(upd.Channel.Storage.ChannelID, uint64(our.CommittedSeqno), uint64(their.CommittedSeqno)),
					Their:                  db.NewSide(upd.Channel.Storage.ChannelID, uint64(their.CommittedSeqno), uint64(our.CommittedSeqno)),
					InitAt:                 time.Unix(int64(upd.Transaction.Now), 0),
					CreatedAt:              createAt,
					AcceptingActions:       upd.Channel.Status == payments.ChannelStatusOpen,
					SafeOnchainClosePeriod: int64(s.cfg.BufferTimeToCommit) + int64(upd.Channel.Storage.ClosingConfig.QuarantineDuration) + int64(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration),
					DBVersion:              version,
				}

				if upd.Channel.Storage.PaymentConfig.CurrencyConfig != nil {
					switch cc := upd.Channel.Storage.PaymentConfig.CurrencyConfig.(type) {
					case payments.CurrencyConfigJetton:
						// TODO: filter allowed masters
						channel.JettonAddress = cc.Info.Master.String()
					case payments.CurrencyConfigEC:
						channel.ExtraCurrencyID = cc.ID
					}
				}
			}

			if upd.Transaction.LT <= channel.LastProcessedLT {
				continue
			}

			seqnoChanged := channel.OurOnchain.CommittedSeqno < our.CommittedSeqno || channel.TheirOnchain.CommittedSeqno < their.CommittedSeqno
			balanceChanged := channel.OurOnchain.Deposited.Cmp(our.Deposited) != 0 || channel.TheirOnchain.Deposited.Cmp(their.Deposited) != 0

			if seqnoChanged || balanceChanged {
				// withdrawal is not pending anymore
				// even if it was not executed, message is not valid anymore
				channel.Our.PendingWithdraw.SetUint64(0)
				channel.Their.PendingWithdraw.SetUint64(0)
			}

			channel.LastProcessedLT = upd.Transaction.LT
			channel.OurOnchain = our
			channel.TheirOnchain = their

			switch upd.Channel.Status {
			case payments.ChannelStatusOpen:
				if channel.Status != db.ChannelStateActive {
					channel.Status = db.ChannelStateActive

					// give 5 sec to other side for tx discovery
					delay := time.Now().Add(5 * time.Second)
					err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "increment-state", channel.Address,
						"exchange-states-"+channel.Address+"-"+fmt.Sprint(channel.InitAt.Unix()),
						db.IncrementStatesTask{ChannelAddress: channel.Address, WantResponse: true}, &delay, nil,
					)
					if err != nil {
						log.Error().Err(err).Str("channel", channel.Address).Msg("failed to create task for exchanging states")
						continue
					}

					log.Info().Str("address", channel.Address).
						Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
						Msg("onchain channel opened")
				}
			case payments.ChannelStatusClosureStarted:
				channel.Status = db.ChannelStateClosing

				if (isLeft && !upd.Channel.Storage.Quarantine.StateCommittedByA) ||
					(!isLeft && upd.Channel.Storage.Quarantine.StateCommittedByA) {
					// if committed not by us, check state
					qOur, qTheir := upd.Channel.Storage.Quarantine.StateB, upd.Channel.Storage.Quarantine.StateA
					if isLeft {
						qOur, qTheir = qTheir, qOur
					}

					log.Info().Str("address", channel.Address).
						Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
						Msg("onchain channel closure started")

					// TODO: maybe check only their?
					if qOur.Seqno < channel.Our.State.Data.Seqno ||
						qTheir.Seqno < channel.Their.State.Data.Seqno {
						// something is outdated, challenge state
						settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+1), 0)
						err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "challenge", channel.Address+"-chain",
							"challenge-"+base64.StdEncoding.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
							db.ChannelTask{Address: channel.Address}, nil, &settleAt,
						)
						if err != nil {
							log.Error().Err(err).Str("channel", channel.Address).Msg("failed to create task for settling conditions")
							time.Sleep(3 * time.Second)
							continue
						}
					}
				}
				fallthrough
			case payments.ChannelStatusSettlingConditionals:
				channel.Status = db.ChannelStateClosing

				settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+3), 0)
				finishAt := settleAt.Add(time.Duration(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration) * time.Second)

				log.Info().Str("address", channel.Address).
					Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
					Time("execute_at", settleAt).
					Msg("onchain channel uncooperative closing event, settling conditions")

				err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "settle", channel.Address+"-settle",
					"settle-"+base64.StdEncoding.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
					db.ChannelTask{Address: channel.Address}, &settleAt, &finishAt,
				)
				if err != nil {
					log.Error().Err(err).Str("channel", channel.Address).Msg("failed to create task for settling conditions")
					time.Sleep(3 * time.Second)
					continue
				}
				fallthrough
			case payments.ChannelStatusAwaitingFinalization:
				channel.Status = db.ChannelStateClosing

				settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+5), 0)
				finishAt := settleAt.Add(time.Duration(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration+5) * time.Second)

				log.Info().Str("address", channel.Address).
					Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
					Float64("till_finalize_sec", time.Until(finishAt).Seconds()).
					Msg("onchain channel awaiting finalization")

				err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "finalize", channel.Address+"-finalize",
					"finalize-"+base64.StdEncoding.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
					db.ChannelTask{Address: channel.Address}, &finishAt, nil,
				)
				if err != nil {
					log.Error().Err(err).Str("channel", channel.Address).Msg("failed to create task for finalizing channel")
					time.Sleep(3 * time.Second)
					continue
				}
			case payments.ChannelStatusUninitialized:
				if channel.Status != db.ChannelStateInactive {
					channel.Status = db.ChannelStateInactive

					log.Info().Str("address", channel.Address).
						Str("with", base64.StdEncoding.EncodeToString(channel.TheirOnchain.Key)).
						Msg("onchain channel closed")
				}
			}

			fc := s.db.UpdateChannel
			if isNew {
				fc = s.db.CreateChannel
			}

			if err = fc(context.Background(), channel); err != nil {
				log.Error().Err(err).Msg("failed to set channel in db, retrying...")
				time.Sleep(1 * time.Second)

				// we retry full process because we need to reproduce all changes in case of concurrent update
				goto retry
			}

			if s.webhook != nil {
				for {
					if err = s.webhook.PushChannelEvent(context.Background(), channel); err != nil {
						log.Error().Err(err).Msg("failed to push channel webhook to queue, retrying...")
						time.Sleep(1 * time.Second)
						continue
					}
					break
				}
			}
		}
	}
}

func (s *Service) DebugPrintVirtualChannels() {
	chs, err := s.db.GetChannels(context.Background(), nil, db.ChannelStateActive)
	if err != nil {
		log.Error().Err(err).Msg("failed to get active channels")
		return
	}

	if len(chs) == 0 {
		log.Info().Msg("no active channels")
		return
	}

	for _, ch := range chs {
		inBalance, outBalance := "?", "?"
		val, err := ch.CalcBalance(false)
		if err == nil {
			outBalance = tlb.FromNanoTON(val).String()
		}

		val, err = ch.CalcBalance(true)
		if err == nil {
			inBalance = tlb.FromNanoTON(val).String()
		}

		log.Info().Str("address", ch.Address).
			Str("with", base64.StdEncoding.EncodeToString(ch.TheirOnchain.Key)).
			Str("out_deposit", tlb.FromNanoTON(ch.OurOnchain.Deposited).String()).
			Str("out_withdrawn", tlb.FromNanoTON(ch.OurOnchain.Withdrawn).String()).
			Str("sent_out", ch.Our.State.Data.Sent.String()).
			Str("balance_out", outBalance).
			Str("in_deposit", tlb.FromNanoTON(ch.TheirOnchain.Deposited).String()).
			Str("in_withdrawn", tlb.FromNanoTON(ch.TheirOnchain.Withdrawn).String()).
			Str("sent_in", ch.Their.State.Data.Sent.String()).
			Str("balance_in", inBalance).
			Uint64("seqno_their", ch.Their.State.Data.Seqno).
			Uint64("seqno_our", ch.Our.State.Data.Seqno).
			Bool("accepting_actions", ch.AcceptingActions).
			Bool("we_master", ch.WeLeft).
			Str("jetton", ch.JettonAddress).
			Uint32("ec", ch.ExtraCurrencyID).
			Msg("active onchain channel")
		for _, kv := range ch.Our.Conditionals.All() {
			vch, _ := payments.ParseVirtualChannelCond(kv.Value.BeginParse())
			till := time.Unix(vch.Deadline, 0).Sub(time.Now())
			log.Info().
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("till_deadline", till.String()).
				Str("till_safe_deadline", (till-time.Duration(ch.SafeOnchainClosePeriod)*time.Second).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Hex("key", vch.Key).
				Msg("virtual from us")
		}
		for _, kv := range ch.Their.Conditionals.All() {
			vch, _ := payments.ParseVirtualChannelCond(kv.Value.BeginParse())

			log.Info().
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("till_deadline", time.Unix(vch.Deadline, 0).Sub(time.Now()).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Hex("key", vch.Key).
				Msg("virtual to us")
		}
	}
}

func (s *Service) GetActiveChannel(ctx context.Context, channelAddr string) (*db.Channel, error) {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return nil, err
	}

	if channel.Status != db.ChannelStateActive {
		return nil, ErrNotActive
	}

	if !channel.Our.IsReady() || !channel.Their.IsReady() {
		return nil, fmt.Errorf("states not exchanged yet")
	}

	return channel, nil
}

func (s *Service) GetVirtualChannelMeta(ctx context.Context, key ed25519.PublicKey) (*db.VirtualChannelMeta, error) {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return nil, err
	}

	return meta, nil
}

func (s *Service) requestAction(ctx context.Context, channelAddress string, action any) ([]byte, error) {
	channel, err := s.db.GetChannel(ctx, channelAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	decision, err := s.transport.RequestAction(ctx, address.MustParseAddr(channel.Address), channel.TheirOnchain.Key, action)
	if err != nil {
		return nil, fmt.Errorf("failed to request actions: %w", err)
	}

	if !decision.Agreed {
		log.Warn().Str("reason", decision.Reason).Msg("actions request denied")
		return nil, ErrDenied
	}
	return decision.Signature, nil
}

// proposeAction - Update our state and send it to party.
// It should be called in strict order, to avoid state unsync due to network or other problems.
// Call should be considered as finished only when nil or ErrDenied was returned.
// That's why all calls to proposeAction must be done via worker jobs.
// Repeatable calls with the same state should be ok, other side's ProcessAction supports idempotency.
func (s *Service) proposeAction(ctx context.Context, lockId int64, channelAddress string, action transport.Action, details any) error {
	channel, err := s.db.GetChannel(ctx, channelAddress)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	onSuccess, state, updProof, err := s.updateOurStateWithAction(channel, action, details)
	if err != nil {
		return fmt.Errorf("failed to prepare actions for the next node - %w: %v", ErrNotPossible, err)
	}

	res, err := s.transport.ProposeAction(ctx, lockId, address.MustParseAddr(channel.Address), channel.TheirOnchain.Key, state, updProof, action)
	if err != nil {
		return fmt.Errorf("failed to propose actions: %w", err)
	}

	if !res.Agreed {
		if res.Reason == db.ErrChannelBusy.Error() {
			// we can retry later, no need to revert
			return ErrChannelIsBusy
		}
		log.Warn().Str("reason", res.Reason).Msg("actions request denied")
		return ErrDenied
	}

	var theirState payments.SignedSemiChannel
	if err = tlb.LoadFromCellAsProof(&theirState, res.SignedState.BeginParse()); err != nil {
		return fmt.Errorf("failed to parse their updated channel state: %w", err)
	}

	// should be unchanged
	theirState.State.Data.ConditionalsHash = channel.Their.SignedSemiChannel.State.Data.ConditionalsHash

	if err = theirState.Verify(channel.TheirOnchain.Key); err != nil {
		return fmt.Errorf("failed to verify their state signature: %w", err)
	}

	if err = theirState.State.CheckSynchronized(&channel.Our.State); err != nil {
		return fmt.Errorf("states are not syncronized: %w", err)
	}

	// renew their state to update their reference to our state
	channel.Their.SignedSemiChannel = theirState
	if err = s.db.UpdateChannel(ctx, channel); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	if onSuccess != nil {
		onSuccess()
	}

	return nil
}

func (s *Service) verifyChannel(p *payments.AsyncChannel) (ok bool, isLeft bool) {
	isLeft = bytes.Equal(p.Storage.KeyA, s.key.Public().(ed25519.PublicKey))

	if !isLeft && !bytes.Equal(p.Storage.KeyB, s.key.Public().(ed25519.PublicKey)) {
		return false, false
	}

	if !isLeft && p.Storage.PaymentConfig.DestB.Bounce(false).String() != s.wallet.WalletAddress().String() {
		return false, false
	}

	if isLeft && p.Storage.PaymentConfig.DestA.Bounce(false).String() != s.wallet.WalletAddress().String() {
		return false, false
	}

	var cc config.CoinConfig
	switch v := p.Storage.PaymentConfig.CurrencyConfig.(type) {
	case payments.CurrencyConfigEC:
		cc, ok = s.supportedEC[v.ID]
		if !ok {
			return false, false
		}
	case payments.CurrencyConfigJetton:
		cc, ok = s.supportedJettons[v.Info.Master.Bounce(true).String()]
		if !ok {
			return false, false
		}
	case payments.CurrencyConfigTon:
		if !s.supportedTon {
			return false, false
		}
		cc = s.cfg.SupportedCoins.Ton
	default:
		return false, false
	}

	if p.Storage.PaymentConfig.StorageFee.String() != tlb.MustFromTON(cc.ExcessFeeTon).String() {
		return false, false
	}

	if p.Storage.ClosingConfig.ConditionalCloseDuration != s.cfg.ConditionalCloseDurationSec ||
		p.Storage.ClosingConfig.QuarantineDuration != s.cfg.QuarantineDurationSec ||
		p.Storage.ClosingConfig.MisbehaviorFine.Nano().Cmp(tlb.MustFromDecimal(cc.MisbehaviorFine, int(cc.Decimals)).Nano()) != 0 {
		return false, false
	}
	return true, isLeft
}

func (s *Service) IncrementStates(ctx context.Context, channelAddr string, wantResponse bool) error {
	channel, err := s.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Address,
		"increment-state-"+channel.Address+"-force-"+fmt.Sprint(time.Now().UnixNano()),
		db.IncrementStatesTask{
			ChannelAddress: channel.Address,
			WantResponse:   wantResponse,
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create increment-state task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) RequestRemoveVirtual(ctx context.Context, key ed25519.PublicKey) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return err
	}

	if meta.Incoming == nil {
		return fmt.Errorf("virtual channel has no incoming channel")
	}

	if meta.Outgoing != nil && !meta.Outgoing.UncooperativeDeadline.Before(time.Now()) {
		return fmt.Errorf("outgoing direction is not timed out yet, not safe")
	}

	if meta.Status != db.VirtualChannelStateActive && meta.Status != db.VirtualChannelStatePending {
		return fmt.Errorf("virtual channel is not active or pending")
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-remove-virtual", meta.Incoming.ChannelAddress,
		"ask-remove-virtual-"+base64.StdEncoding.EncodeToString(meta.Key)+"-desire",
		db.AskRemoveVirtualTask{
			ChannelAddress: meta.Incoming.ChannelAddress,
			Key:            meta.Key,
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
	}
	return nil
}

func (s *Service) ProcessIsChannelLocked(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64) error {
	if id <= 0 {
		return fmt.Errorf("id must be positive")
	}
	id = -id // negative id to not collide with our own locks

	addrStr := addr.String()
	channel, err := s.db.GetChannel(ctx, addrStr)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if s.discoveryMx.TryLock() {
				go func() {
					// our party proposed action with channel we don't know,
					// we will try to find it onchain and register (asynchronously)
					s.discoverChannel(addr)
					s.discoveryMx.Unlock()
				}()
			}
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if !bytes.Equal(channel.TheirOnchain.Key, key) {
		return fmt.Errorf("unauthorized channel")
	}

	s.lockerMx.Lock()
	defer s.lockerMx.Unlock()

	l, ok := s.channelLocks[channel.Address]
	if !ok || l.id != id {
		// not locked by this lock
		return nil
	}

	// if we locked it, then it was unlocked
	if l.mx.TryLock() {
		// unlock immediately, because we did it only to check
		l.mx.Unlock()
		return nil
	}

	return fmt.Errorf("still locked")
}

// ProcessExternalChannelLock - we have a master-slave lock system for channel communication, where left side of channel is a lock master,
// when some side wants to do some actions on a channel (for example open virtual), it first locks channel on a master
// to make sure there will be no parallel executions and colliding locks.
func (s *Service) ProcessExternalChannelLock(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64, lock bool) error {
	if id <= 0 {
		return fmt.Errorf("id must be positive")
	}
	id = -id // negative id to not collide with our own locks

	addrStr := addr.String()
	channel, err := s.db.GetChannel(ctx, addrStr)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			if s.discoveryMx.TryLock() {
				go func() {
					// our party proposed action with channel we don't know,
					// we will try to find it onchain and register (asynchronously)
					s.discoverChannel(addr)
					s.discoveryMx.Unlock()
				}()
			}
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if !bytes.Equal(channel.TheirOnchain.Key, key) {
		return fmt.Errorf("unauthorized channel")
	}

	if !channel.WeLeft {
		return fmt.Errorf("not a lock master")
	}

	s.externalLockerMx.Lock()
	defer s.externalLockerMx.Unlock()

	unlockFunc := s.externalLock
	if !lock {
		if unlockFunc != nil {
			s.externalLock = nil
			unlockFunc()
			log.Debug().Type("channel", addr).Int64("id", id).Msg("external lock unlocked")
		}
		// already unlocked (idempotency)
		return nil
	}

	if unlockFunc != nil {
		// already locked by other party (idempotency)
		log.Debug().Type("channel", addr).Int64("id", id).Msg("external lock already locked")

		return ErrChannelIsBusy
	}

	// this call is fast, because we are master
	_, _, unlock, err := s.AcquireChannel(ctx, channel.Address, id)
	if err != nil {
		return err
	}

	ch := make(chan bool, 1)
	s.externalLock = func() {
		unlock()
		close(ch)
	}

	go func() {
		// we start this routine to unlock in case of other side crashes and forget the lock
		for {
			select {
			case <-ch:
				return
			case <-time.After(5 * time.Second):
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			res, err := s.transport.IsChannelUnlocked(ctx, channel.TheirOnchain.Key, addr, -id)
			cancel()
			if err != nil {
				continue
			}

			if !res.Agreed {
				log.Warn().Type("channel", addr).Int64("id", id).Str("reason", res.Reason).Msg("external lock seems still locked")

				continue
			}

			// TODO: check is other side still holds the lock

			s.externalLockerMx.Lock()
			s.externalLock = nil
			s.externalLockerMx.Unlock()

			// unlock our side only, no need to request them because they don't know this lock
			unlock()
			return
		}
	}()

	log.Debug().Type("channel", addr).Int64("id", id).Msg("external lock accepted")

	return nil
}

func (s *Service) AcquireChannel(ctx context.Context, addr string, id ...int64) (*db.Channel, int64, func(), error) {
	s.lockerMx.Lock()
	// TODO: optimize for global lockless?
	channel, err := s.db.GetChannel(ctx, addr)
	if err != nil {
		s.lockerMx.Unlock()

		return nil, 0, nil, err
	}

	l, ok := s.channelLocks[channel.Address]
	if !ok {
		l = &channelLock{
			queue: make(chan bool, 1),
		}
		s.channelLocks[channel.Address] = l
	}

	// master re-locks our pending lock, we can do this because we know that RequestChannelLock will fail
	/*if !channel.WeLeft && l.id > 0 && len(id) > 0 && id[0] < 0 && l.pending {
		l = &channelLock{}
		s.channelLocks[channel.Address] = l
	}*/

	if !l.mx.TryLock() {
		// TODO: wait for 1s if lock is ours to catch it after and continue to hold without unlocking
		/*if l.id > 0 && len(id) == 0 {
			select {
			case <-time.After(1 * time.Second):
				break
			case <-l.queue:

			}
		}*/

		defer s.lockerMx.Unlock()
		if len(id) > 0 && id[0] == l.id {
			// already locked in this context
			return channel, l.id, func() {}, nil
		}
		return nil, 0, nil, db.ErrChannelBusy
	}

	if len(id) == 0 {
		l.id = time.Now().UnixNano()
	} else {
		l.id = id[0]
	}

	log.Debug().Str("channel", addr).Bool("master", channel.WeLeft).Int64("id", l.id).Msg("acquiring lock")

	s.lockerMx.Unlock()

	// left side is lock master, negative means lock from other side (master locks us)
	if channel.WeLeft || l.id < 0 {
		return channel, l.id, func() {
			l.mx.Unlock()

			log.Debug().Str("channel", addr).Int64("id", l.id).Msg("local lock released")
		}, nil
	}

	// l.pending = true
	chAddr := address.MustParseAddr(channel.Address)
	res, err := s.transport.RequestChannelLock(ctx, channel.TheirOnchain.Key, chAddr, l.id, true)
	// l.pending = false
	if err != nil {
		l.mx.Unlock()

		return nil, 0, nil, err
	}

	if !res.Agreed {
		l.mx.Unlock()

		log.Debug().Str("channel", addr).Int64("id", l.id).Str("reason", res.Reason).Msg("external lock not obtained")

		return nil, 0, nil, db.ErrChannelBusy
	}

	return channel, l.id, func() {
		l.mx.Unlock()

		res, err := s.transport.RequestChannelLock(ctx, channel.TheirOnchain.Key, chAddr, l.id, false)
		if err != nil {
			log.Warn().Str("channel", addr).Int64("id", l.id).Err(err).Msg("external lock release failed, but state can be fetched by another party, no worries")
		} else if !res.Agreed {
			log.Warn().Str("channel", addr).Int64("id", l.id).Str("reason", res.Reason).Msg("external lock release failed, not accepted by party")
		} else {
			log.Debug().Str("channel", addr).Int64("id", l.id).Msg("external lock released")
		}
	}, nil
}
