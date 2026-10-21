package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	paymentvault "github.com/xssnick/ton-payment-network/pkg/payments/vault"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var ErrNotActive = errors.New("channel is not active")
var ErrDenied = errors.New("actions denied")
var ErrChannelIsBusy = errors.New("channel is busy")
var ErrNotPossible = errors.New("not possible")

const PaymentsTaskPool = "pn"

type Transport interface {
	AddUrgentPeer(channelKey ed25519.PublicKey)
	RemoveUrgentPeer(channelKey ed25519.PublicKey)
	ProposeChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey, prop transport.ProposeChannelConfig) error
	RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action transport.Action) (*transport.Decision, error)
	ProposeAction(ctx context.Context, lockId int64, channelAddr *address.Address, theirChannelKey []byte, state *cell.Cell, action transport.Action) (*transport.ProposalDecision, error)
	RequestChannelLock(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64, lock bool) (*transport.Decision, error)
	IsChannelUnlocked(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64) (*transport.Decision, error)
	OpenOffchainChannel(ctx context.Context, theirChannelKey, codeHash []byte, cfg payments.OpenConfigContainer) (*address.Address, []byte, error)
}

type Webhook interface {
	PushChannelEvent(ctx context.Context, ch *db.Channel) error
	PushVirtualChannelEvent(ctx context.Context, event db.VirtualChannelEventType, meta *db.ConditionalMeta) error
}

type DB interface {
	Transaction(ctx context.Context, f func(ctx context.Context) error) error
	CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context, poolName string) (*db.Task, error)
	RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, poolName string, task *db.Task) error
	ListActiveTasks(ctx context.Context, poolName string) ([]*db.Task, error)

	GetVirtualChannelMeta(ctx context.Context, key []byte) (*db.ConditionalMeta, error)
	UpdateVirtualChannelMeta(ctx context.Context, meta *db.ConditionalMeta) error
	CreateVirtualChannelMeta(ctx context.Context, meta *db.ConditionalMeta) error
	ClosePairMeta(ctx context.Context, key []byte, status db.ConditionalStatus) error
	ForEachActiveSpecialMetaKey(ctx context.Context, fn func(key ed25519.PublicKey) error) error

	SetBlockOffset(ctx context.Context, seqno uint32) error
	GetBlockOffset(ctx context.Context) (*db.BlockOffset, error)

	GetChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error)
	CreateChannel(ctx context.Context, channel *db.Channel) error
	GetChannel(ctx context.Context, addr string) (*db.Channel, error)
	UpdateChannel(ctx context.Context, channel *db.Channel) error
	SetOnChannelUpdated(f func(ctx context.Context, ch *db.Channel, statusChanged bool))
	GetOnChannelUpdated() func(ctx context.Context, ch *db.Channel, statusChanged bool)
	CreateChannelEvent(ctx context.Context, channel *db.Channel, at time.Time, item db.ChannelHistoryItem) error

	GetUrgentPeers(ctx context.Context) ([][]byte, error)

	GetChannelsHistoryByPeriod(ctx context.Context, addr string, limit int, before, after *time.Time) ([]db.ChannelHistoryItem, error)

	CreateActionCode(ctx context.Context, action *cell.Cell) error
	GetActionCode(ctx context.Context, hash []byte) (*cell.Cell, error)

	SaveChannelPendingState(ctx context.Context, channel *db.Channel, body payments.StateBody) error
	GetChannelPendingState(ctx context.Context, channel *db.Channel, body payments.StateBody) (*db.PendingChannelState, error)
	CleanupChannelPendingStates(ctx context.Context, channel *db.Channel, body payments.StateBody) error

	Close()
}

type WalletMessage struct {
	To        *address.Address
	Amount    tlb.Coins
	Body      *cell.Cell
	StateInit *tlb.StateInit
	EC        map[uint32]tlb.Coins
}

type Wallet interface {
	WalletAddress() *address.Address
	DoTransactionMany(ctx context.Context, reason string, messages []WalletMessage) ([]byte, error)
	DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error)
}

type BlockCheckedEvent struct {
	Seqno uint32
}

type ChannelUpdatedEvent struct {
	Transaction   *client.Transaction
	LatestChannel *payments.ChannelContract
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

	channels map[string]*balanceControlChannel
	mx       sync.Mutex
}

type balanceControlChannel struct {
	depositLockedTill  *time.Time
	withdrawLockedTill *time.Time
}

type ChainAPI interface {
	GetAccount(ctx context.Context, addr *address.Address, blockAfter time.Time) (*client.Account, error)
	GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error)
	GetJettonBalance(ctx context.Context, root, addr *address.Address, blockAfter time.Time) (*big.Int, error)
	GetLastTransaction(ctx context.Context, addr *address.Address, blockAfter time.Time) (*client.Transaction, *client.Account, error)
	GetTransactionByInMsgHash(ctx context.Context, addr *address.Address, msgHash []byte, after time.Time) (*client.Transaction, error)
	SendWaitExternalMessage(ctx context.Context, to *address.Address, body *cell.Cell) ([]byte, error)
}

type SwapHook func(ctx context.Context, ch *db.Channel, fromCC, toCC *payments.CoinConfig, from, to tlb.Coins) error

type Service struct {
	ton              ChainAPI
	regularTransport Transport
	webTransport     Transport
	updates          chan any
	db               DB
	webhook          Webhook

	key ed25519.PrivateKey

	wallet                         Wallet
	channelClient                  *payments.Client
	virtualChannelsLimitPerChannel int
	workerSignal                   chan bool

	cfg      config.ChannelsConfig
	vaultCfg config.VaultConfig

	// TODO: channel based lock
	mx sync.Mutex

	externalLock func()

	channelLocks     map[string]*channelLock
	lockerMx         sync.Mutex
	externalLockerMx sync.Mutex

	knownBalanceTypes        map[string]*payments.CoinConfig
	knownBalanceTypesSymbols map[string]*payments.CoinConfig
	balanceControllers       map[string]*balanceControlConfig
	urgentPeers              map[string]int
	useMetrics               bool

	onSwap SwapHook

	cacheMx      sync.RWMutex
	actionsCache map[string]payments.Action

	globalCtx    context.Context
	globalCancel context.CancelFunc

	urgentPeersMx sync.RWMutex
	discoveryMx   sync.Mutex

	vaultManager *paymentvault.Manager
}

func NewService(api ChainAPI, database DB, transport, webTransport Transport, wallet Wallet, updates chan any, key ed25519.PrivateKey, cfg config.ChannelsConfig, vaultCfg config.VaultConfig, useMetrics bool) (*Service, error) {
	globalCtx, globalCancel := context.WithCancel(context.Background())
	s := &Service{
		ton:                            api,
		regularTransport:               transport,
		webTransport:                   webTransport,
		updates:                        updates,
		db:                             database,
		key:                            key,
		wallet:                         wallet,
		channelClient:                  payments.NewPaymentChannelClient(api),
		virtualChannelsLimitPerChannel: 30000,
		workerSignal:                   make(chan bool, 1),
		cfg:                            cfg,
		vaultCfg:                       vaultCfg,
		channelLocks:                   map[string]*channelLock{},
		knownBalanceTypes:              map[string]*payments.CoinConfig{},
		knownBalanceTypesSymbols:       map[string]*payments.CoinConfig{},
		balanceControllers:             map[string]*balanceControlConfig{},
		urgentPeers:                    map[string]int{},
		useMetrics:                     useMetrics,
		actionsCache:                   map[string]payments.Action{},
		globalCtx:                      globalCtx,
		globalCancel:                   globalCancel,
	}

	if strings.TrimSpace(cfg.DerivativesHedge.WebhookURL) != "" {
		if _, _, err := s.derivativeHedgeAuth(); err != nil {
			return nil, fmt.Errorf("invalid derivatives hedge config: %w", err)
		}
	}

	addBalanceControl := func(balanceId string, currency config.CoinConfig) error {
		if currency.BalanceControl == nil {
			currency.BalanceControl = &config.BalanceControlConfig{
				DepositWhenAmountLessThan: "0",
				DepositUpToAmount:         "0",
				WithdrawWhenAmountReached: "0",
			}
		}

		conf := &balanceControlConfig{
			DepositWhenAmountLessThan: tlb.MustFromDecimal(currency.BalanceControl.DepositWhenAmountLessThan, int(currency.Decimals)),
			DepositUpToAmount:         tlb.MustFromDecimal(currency.BalanceControl.DepositUpToAmount, int(currency.Decimals)),
			WithdrawWhenAmountReached: tlb.MustFromDecimal(currency.BalanceControl.WithdrawWhenAmountReached, int(currency.Decimals)),
			channels:                  map[string]*balanceControlChannel{},
		}

		if conf.WithdrawWhenAmountReached.Nano().Sign() != 0 &&
			conf.DepositUpToAmount.Nano().Sign() != 0 && conf.WithdrawWhenAmountReached.Compare(conf.DepositUpToAmount) < 0 {
			return fmt.Errorf("withdraw amount must be greater than deposit amount")
		}

		if conf.DepositWhenAmountLessThan.Nano().Sign() != 0 &&
			conf.DepositUpToAmount.Nano().Sign() != 0 && conf.DepositWhenAmountLessThan.Compare(conf.DepositUpToAmount) > 0 {
			return fmt.Errorf("deposit up to amount must be greater than deposit when amount less than")
		}

		s.balanceControllers[balanceId] = conf
		return nil
	}

	convertConfig := func(cfg config.CoinConfig, id string) *payments.CoinConfig {
		return &payments.CoinConfig{
			Enabled: cfg.Enabled,
			VirtualTunnelConfig: payments.VirtualConfig{
				MaxCapacityToRentPerTx:      tlb.MustFromDecimal(cfg.VirtualTunnelConfig.MaxCapacityToRentPerTx, int(cfg.Decimals)),
				CapacityDepositFee:          tlb.MustFromDecimal(cfg.VirtualTunnelConfig.CapacityDepositFee, int(cfg.Decimals)),
				CapacityFeePercentPer30Days: cfg.VirtualTunnelConfig.CapacityFeePercentPer30Days,
				ProxyMaxCapacity:            tlb.MustFromDecimal(cfg.VirtualTunnelConfig.ProxyMaxCapacity, int(cfg.Decimals)),
				ProxyMinFee:                 tlb.MustFromDecimal(cfg.VirtualTunnelConfig.ProxyMinFee, int(cfg.Decimals)),
				ProxyFeePercent:             cfg.VirtualTunnelConfig.ProxyFeePercent,
				DerivativeFeePercent:        cfg.VirtualTunnelConfig.DerivativeFeePercent,
				AllowTunneling:              cfg.VirtualTunnelConfig.AllowTunneling,
			},
			Symbol:                strings.ToUpper(cfg.Symbol),
			Decimals:              cfg.Decimals,
			MinCapacityRequest:    tlb.MustFromDecimal(cfg.MinCapacityRequest, int(cfg.Decimals)),
			FeePerWithdrawPropose: tlb.MustFromDecimal(cfg.FeePerWithdrawPropose, int(cfg.Decimals)),
			BalanceID:             id,
			VaultResolver:         s,
		}
	}

	for addr, currency := range cfg.SupportedCoins.Jettons {
		if !currency.Enabled {
			continue
		}

		a, err := address.ParseAddr(addr)
		if err != nil {
			return nil, err
		}
		a = a.Bounce(true)

		bId := payments.GetJettonBalanceID(a)
		c := convertConfig(currency, bId)
		c.JettonClient = s.NewJettonCacher(a)
		s.knownBalanceTypes[bId] = c

		if err = addBalanceControl(bId, currency); err != nil {
			return nil, err
		}
	}

	for id, currency := range cfg.SupportedCoins.ExtraCurrencies {
		if !currency.Enabled {
			continue
		}

		if id == 0 {
			return nil, fmt.Errorf("extra currency id 0 is reserved")
		}

		bId := payments.GetECBalanceID(id)
		s.knownBalanceTypes[bId] = convertConfig(currency, bId)

		if err := addBalanceControl(bId, currency); err != nil {
			return nil, err
		}
	}

	if cfg.SupportedCoins.Ton.Enabled {
		bId := payments.GetTONBalanceID()
		s.knownBalanceTypes[bId] = convertConfig(cfg.SupportedCoins.Ton, bId)

		if err := addBalanceControl(bId, cfg.SupportedCoins.Ton); err != nil {
			return nil, err
		}
	}

	for _, b := range s.knownBalanceTypes {
		if s.knownBalanceTypesSymbols[b.Symbol] != nil {
			return nil, fmt.Errorf("duplicate currency '%s' cannot be configured", b.Symbol)
		}
		s.knownBalanceTypesSymbols[b.Symbol] = b
	}

	if err := s.initVaultManager(); err != nil {
		return nil, err
	}

	if err := s.loadUrgentPeers(context.Background()); err != nil {
		return nil, err
	}

	handler := s.channelCallback
	if current := database.GetOnChannelUpdated(); current != nil {
		handler = func(ctx context.Context, ch *db.Channel, statusChanged bool) {
			current(ctx, ch, statusChanged)
			s.channelCallback(ctx, ch, statusChanged)
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
			s.channelCallback(context.Background(), ch, false)
		}
	}()

	return s, nil
}

func (s *Service) Stop() {
	s.globalCancel()
}

func (s *Service) SetOnSwap(sh SwapHook) {
	s.onSwap = sh
}

func (s *Service) GetChannelsHistoryByPeriod(ctx context.Context, addr string, limit int, before, after *time.Time) ([]db.ChannelHistoryItem, error) {
	return s.db.GetChannelsHistoryByPeriod(ctx, addr, limit, before, after)
}

func (s *Service) AddUrgentPeer(peer []byte) {
	s.urgentPeersMx.Lock()
	refs := s.urgentPeers[string(peer)]
	s.urgentPeers[string(peer)] = refs + 1
	s.urgentPeersMx.Unlock()

	if refs == 0 {
		s.regularTransport.AddUrgentPeer(peer)
	}
}

func (s *Service) RemoveUrgentPeer(peer []byte) {
	s.urgentPeersMx.Lock()
	refs := s.urgentPeers[string(peer)]
	if refs > 1 {
		s.urgentPeers[string(peer)] = refs - 1
	} else {
		delete(s.urgentPeers, string(peer))
	}
	s.urgentPeersMx.Unlock()

	if refs == 1 {
		s.regularTransport.RemoveUrgentPeer(peer)
	}
}

func (s *Service) loadUrgentPeers(ctx context.Context) error {
	channels, err := s.ListChannels(ctx, nil, db.ChannelStateActive)
	if err != nil {
		return err
	}

	for _, ch := range channels {
		if ch.UrgentForUs {
			s.AddUrgentPeer(ch.Their.OnchainInfo.Key)
		}
	}
	return nil
}

func (s *Service) channelCallback(ctx context.Context, ch *db.Channel, statusChanged bool) {
	if ch.UrgentForUs && statusChanged {
		if ch.Status == db.ChannelStateActive {
			s.AddUrgentPeer(ch.Their.OnchainInfo.Key)
		} else {
			s.RemoveUrgentPeer(ch.Their.OnchainInfo.Key)
		}
	}

	s.balanceControlCallback(ctx, ch, statusChanged)
}

func (s *Service) balanceControlCallback(ctx context.Context, ch *db.Channel, _ bool) {
	if ch.LoadSignedState().IsEmpty() {
		log.Debug().Str("address", ch.Our.Address).Msg("not ready, skipping balance control callback")
		return
	}

	balances, err := ch.CalcBalance(ctx, false, s)
	if err != nil {
		log.Error().Str("address", ch.Our.Address).Err(err).Msg("failed to calc our balance in balance controller")
		return
	}

	for _, balanceInfo := range balances {
		bc := s.balanceControllers[balanceInfo.CoinConfig.BalanceID]
		if bc == nil {
			continue
		}
		// no onchain actions for pending balance
		balance := new(big.Int).Add(balanceInfo.Available(), balanceInfo.OnHold)
		balance.Add(balance, balanceInfo.ConditionalLocked)

		if ch.Status != db.ChannelStateActive {
			bc.mx.Lock()
			delete(bc.channels, ch.Our.Address)
			bc.mx.Unlock()
			return
		}

		bc.mx.Lock()
		ctrl := bc.channels[ch.Our.Address]
		if ctrl == nil {
			ctrl = &balanceControlChannel{}
			bc.channels[ch.Our.Address] = ctrl
		}

		canDeposit := ctrl.depositLockedTill == nil || ctrl.depositLockedTill.Before(time.Now())
		canWithdraw := ctrl.withdrawLockedTill == nil || ctrl.withdrawLockedTill.Before(time.Now())
		bc.mx.Unlock()

		log.Debug().Str("address", ch.Our.Address).Msg("balance control callback triggered")

		depWhenLess := new(big.Int).Set(bc.DepositWhenAmountLessThan.Nano())
		depUpTo := new(big.Int).Set(bc.DepositUpToAmount.Nano())
		wdAt := new(big.Int).Set(bc.WithdrawWhenAmountReached.Nano())

		locked := big.NewInt(0)
		ld := ch.Our.LockedDeposits[balanceInfo.CoinConfig.BalanceID]
		if ld != nil {
			// we must always keep available onchain till expire
			locked = ld.Available()
			minBalance := new(big.Int).Set(balance)

			if minBalance.Cmp(locked) < 0 {
				minBalance = new(big.Int).Set(locked)

				if depWhenLess.Sign() > 0 && depUpTo.Sign() > 0 {
					if depWhenLess.Cmp(minBalance) < 0 {
						depWhenLess = new(big.Int).Set(minBalance)
					}
					if depUpTo.Cmp(minBalance) < 0 {
						depUpTo = new(big.Int).Set(minBalance)
					}
				} else {
					depWhenLess = new(big.Int).Set(minBalance)
					depUpTo = new(big.Int).Set(minBalance)
				}
			}

			if wdAt.Sign() > 0 {
				wdAt.Add(wdAt, locked)
			}
		}

		if (depWhenLess.Sign() > 0 || locked.Sign() > 0) && balance.Cmp(depWhenLess) < 0 {
			if canDeposit {
				amt := tlb.MustFromNano(new(big.Int).Sub(depUpTo, balance), bc.DepositUpToAmount.Decimals())
				if err = s.TopupChannel(ctx, ch, balanceInfo.CoinConfig.BalanceID, amt, true); err != nil {
					log.Error().Err(err).Str("address", ch.Our.Address).Str("amount", amt.String()).Msg("failed to topup channel")
					return
				}
				till := time.Now().Add(30 * time.Minute)
				// we lock it till timeout in case of transaction loss or something, to not lock forever
				bc.mx.Lock()
				ctrl.depositLockedTill = &till
				bc.mx.Unlock()
			}
		} else if wdAt.Sign() > 0 && balance.Cmp(wdAt) > 0 {
			if canWithdraw {
				amt := tlb.MustFromNano(new(big.Int).Sub(balance, depUpTo), bc.DepositUpToAmount.Decimals())
				if err = s.requestWithdrawToAddr(ctx, ch, s.wallet.WalletAddress(), balanceInfo.CoinConfig, amt.Nano()); err != nil {
					log.Error().Err(err).Str("address", ch.Our.Address).Str("amount", amt.String()).Msg("failed to withdraw from channel")
					return
				}
				till := time.Now().Add(30 * time.Minute)
				// we lock it till timeout in case of transaction loss or something, to not lock forever
				bc.mx.Lock()
				ctrl.withdrawLockedTill = &till
				bc.mx.Unlock()
			}
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

func (s *Service) ReviewChannelConfig(prop transport.ProposeChannelConfig) error {
	known := false
	for _, code := range payments.PaymentChannelCodes {
		if bytes.Equal(code.Hash(), prop.CodeHash) {
			known = true
			break
		}
	}

	if !known {
		return errors.New("payment channel code is unknown")
	}

	ourAttach := tlb.MustFromTON(s.cfg.ReplicationMessageAttachAmount)

	// prop.NodeVersion check when big changes happen

	if prop.QuarantineDuration != s.cfg.QuarantineDurationSec ||
		prop.ConditionalCloseDuration != s.cfg.ConditionalCloseDurationSec ||
		prop.ActionsExecuteDuration != s.cfg.ActionsDuration ||
		new(big.Int).SetBytes(prop.ReplicateAttachAmount).Cmp(ourAttach.Nano()) != 0 {
		return fmt.Errorf("ecpected different channel config: quarantine %d, cond close %d, act close %d, attach %s; if you want to deploy", s.cfg.QuarantineDurationSec, s.cfg.ConditionalCloseDurationSec, s.cfg.ActionsDuration, ourAttach.String())
	}

	return nil
}

func (s *Service) scanSettledConditionals(ctx context.Context, ch *db.Channel, tx *client.Transaction, settle payments.SettleMsg, isOur bool) {
	var newActionsHash []byte
	for _, info := range tx.Out {
		if info.Type == tlb.MsgTypeExternalOut {
			var ev payments.ConditionalsSettledEvent
			if err := tlb.Parse(&ev, info.Body); err != nil {
				continue
			}

			newActionsHash = ev.NewActionsHash
			break
		}
	}

	if newActionsHash == nil {
		log.Warn().Bool("our", isOur).Str("address", ch.Our.Address).Msg("no valid external out in settlement, looks unsuccessful")
		return
	}

	kvs, err := settle.Signed.ToSettle.LoadAll()
	if err != nil {
		log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Msg("failed to load settled conditionals")
		return
	}

	condProofBody, err := settle.Signed.ConditionalsProof.PeekRef(0)
	if err != nil {
		log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Msg("failed to load settled conditionals proof")
		return
	}

	actProofBody, err := settle.Signed.ActionsInputProof.PeekRef(0)
	if err != nil {
		log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Msg("failed to load settled cond actions proof")
		return
	}

	condProofDict := condProofBody.AsDict(256)
	actProofDict := actProofBody.AsDict(256)

	otherSide := &ch.Their
	if !isOur {
		otherSide = &ch.Our
	}

	updatedActStates := otherSide.Data.ActionStates.Copy()

	for _, kv := range kvs {
		key, err := kv.Key.LoadSlice(256)
		if err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Msg("failed to load settled condition key")
			continue
		}
		keyCell := cell.BeginCell().MustStoreSlice(key, 256).EndCell()

		state := kv.Value.MustToCell()

		condCode, err := condProofDict.LoadValue(keyCell)
		if err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(key)).Msg("failed to load condition code")
			continue
		}

		cond, err := payments.CodeToConditional(ctx, condCode.MustToCell(), s)
		if err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(key)).Msg("failed to parse condition")
			continue
		}

		actState, err := actProofDict.LoadValue(cond.GetAction().IDCell())
		if err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(key)).Msg("failed to load action state")
			continue
		}

		newActState, err := cond.Execute(actState.MustToCell(), state, make(map[string]*payments.LockedDepositInfo))
		if err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(key)).Msg("failed to execute condition")
			continue
		}

		// updating action states to execute onchain after conditionals settle (must match onchain)
		if err = updatedActStates.Set(cond.GetAction().IDCell(), newActState); err != nil {
			log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).Msg("failed to set action state")
			continue
		}

		// not updating conditionals to have ability to recover execution state by reassembling (can be implemented later)

		if !isOur {
			if err = s.AddConditionalResolve(ctx, cond.GetKey(), state); err != nil {
				log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).Msg("failed to add virtual channel resolve")
				// not return error because we may have better resolve already
			}

			// close next virtual channels since they commited latest resolve onchain
			if err = s.CloseConditional(ctx, cond.GetKey()); err != nil && !errors.Is(err, ErrCannotCloseOutgoingVirtual) {
				log.Warn().Err(err).Bool("our", isOur).Str("address", ch.Our.Address).
					Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).Msg("failed to create task for close virtual channel")
			}
		}
	}

	if bytes.Equal(newActionsHash, updatedActStates.AsCell().Hash()) {
		// hash match after updates, save it to execute in the next phase
		otherSide.Data.ActionStates = updatedActStates

		log.Info().Bool("our", isOur).Str("address", ch.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("settlement transaction with condition resolves processed")
	} else {
		log.Warn().Bool("our", isOur).Str("address", ch.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("settlement transaction action states hash mismatch, it can lead to problems with execution, looks like a problem with synchronization")
	}
}

func (s *Service) processSideUpdate(ctx context.Context, ch *db.Channel, isOur bool, upd *ChannelUpdatedEvent) error {
	side := &ch.Our
	if !isOur {
		side = &ch.Their
	}

	if side.LatestProcessedLT > 0 && side.LatestProcessedLT != upd.Transaction.PrevTxLT {
		log.Warn().
			Uint64("lt_processed", side.LatestProcessedLT).
			Uint64("lt_received_prev", upd.Transaction.PrevTxLT).
			Uint64("lt_received", upd.Transaction.LT).
			Bool("our", isOur).
			Str("channel", ch.Our.Address).
			Msg("gap between transaction events discovered, looks like scanner missed transaction between lt")
	}

	if upd.LatestChannel.Status == payments.ChannelStatusOpen {
		side.ActiveOnchain = true
	} else {
		ch.AcceptingActions = false
		if upd.LatestChannel.Status != payments.ChannelStatusUninitialized {
			if ch.Status == db.ChannelStateActive {
				ch.Status = db.ChannelStateClosing

				if err := s.db.CreateChannelEvent(ctx, ch, time.Unix(upd.Transaction.At, 0), db.ChannelHistoryItem{
					Action: db.ChannelHistoryActionUncooperativeCloseStarted,
				}); err != nil {
					return fmt.Errorf("failed to create channel event %d: %w", db.ChannelHistoryActionUncooperativeCloseStarted, err)
				}
			}
			side.ActiveOnchain = true
		} else {
			side.ActiveOnchain = false
		}
	}

	if isOur {
		switch upd.LatestChannel.Status {
		case payments.ChannelStatusOpen:
			if ch.Status != db.ChannelStateActive {
				ch.Status = db.ChannelStateActive

				err := s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", ch.Our.Address,
					"exchange-states-"+ch.Our.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
					db.IncrementStatesTask{ChannelAddress: ch.Our.Address, WantResponse: true}, nil, nil,
				)
				if err != nil {
					return fmt.Errorf("failed to create task for exchanging states: %w", err)
				}

				log.Info().Str("address", ch.Our.Address).
					Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
					Msg("onchain channel opened")
			}
		case payments.ChannelStatusClosureStarted:
			if ch.UncoopCloseStarted {
				break
			}

			if !upd.LatestChannel.Storage.Quarantine.CommittedByOwner {
				// if committed not by us, check state
				log.Info().Str("address", ch.Our.Address).
					Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
					Msg("onchain channel closure started")

				body := ch.LoadSignedState().Body
				if upd.LatestChannel.Storage.Quarantine.Seqno < body.Seqno {
					// something is outdated, challenge state
					settleAt := time.Unix(int64(upd.LatestChannel.Storage.Quarantine.QuarantineStarts+upd.LatestChannel.Storage.ClosingConfig.QuarantineDuration+1), 0)
					err := s.db.CreateTask(ctx, PaymentsTaskPool, "challenge", ch.Our.Address+"-chain",
						"challenge-"+base64.StdEncoding.EncodeToString(ch.ID)+"-"+fmt.Sprint(ch.InitAt.Unix()),
						db.ChannelTask{Address: ch.Our.Address}, nil, &settleAt,
					)
					if err != nil {
						return fmt.Errorf("failed to create task for challenge: %w", err)
					}
				}
			}
			fallthrough
		case payments.ChannelStatusSettlingConditionals:
			if ch.UncoopCloseStarted {
				break
			}

			settleAt := time.Unix(int64(upd.LatestChannel.Storage.Quarantine.QuarantineStarts+upd.LatestChannel.Storage.ClosingConfig.QuarantineDuration+3), 0)
			finishAt := settleAt.Add(time.Duration(upd.LatestChannel.Storage.ClosingConfig.ConditionalCloseDuration) * time.Second)

			log.Info().Str("address", ch.Our.Address).
				Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
				Time("execute_at", settleAt).
				Msg("onchain channel uncooperative closing event, settling conditions")

			err := s.db.CreateTask(ctx, PaymentsTaskPool, "settle", ch.Our.Address+"-settle",
				"settle-"+base64.StdEncoding.EncodeToString(ch.ID)+"-"+fmt.Sprint(ch.InitAt.Unix()),
				db.ChannelTask{Address: ch.Our.Address}, &settleAt, &finishAt,
			)
			if err != nil {
				return fmt.Errorf("failed to create task for settling conditions: %w", err)
			}
			fallthrough
		case payments.ChannelStatusExecutingActions:
			if ch.UncoopCloseStarted {
				break
			}

			settleAt := time.Unix(int64(upd.LatestChannel.Storage.Quarantine.QuarantineStarts+
				upd.LatestChannel.Storage.ClosingConfig.QuarantineDuration+
				upd.LatestChannel.Storage.ClosingConfig.ConditionalCloseDuration+3), 0)
			finishAt := settleAt.Add(time.Duration(upd.LatestChannel.Storage.ClosingConfig.ActionsDuration) * time.Second)

			log.Info().Str("address", ch.Our.Address).
				Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
				Time("execute_at", settleAt).
				Msg("onchain channel uncooperative closing event, settling actions")

			err := s.db.CreateTask(ctx, PaymentsTaskPool, "settle-act", ch.Our.Address+"-settle-act",
				"settle-act-"+base64.StdEncoding.EncodeToString(ch.ID)+"-"+fmt.Sprint(ch.InitAt.Unix()),
				db.ChannelTask{Address: ch.Our.Address}, &settleAt, &finishAt,
			)
			if err != nil {
				return fmt.Errorf("failed to create task for settling actions: %w", err)
			}
			fallthrough
		case payments.ChannelStatusAwaitingFinalization:
			if ch.UncoopCloseStarted {
				break
			}

			at := time.Unix(int64(upd.LatestChannel.Storage.Quarantine.QuarantineStarts+
				upd.LatestChannel.Storage.ClosingConfig.QuarantineDuration+
				upd.LatestChannel.Storage.ClosingConfig.ConditionalCloseDuration+
				upd.LatestChannel.Storage.ClosingConfig.ActionsDuration+5), 0)

			log.Info().Str("address", ch.Our.Address).
				Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
				Msg("onchain channel awaiting finalization")

			err := s.db.CreateTask(ctx, PaymentsTaskPool, "finalize", ch.Our.Address+"-finalize",
				"finalize-"+base64.StdEncoding.EncodeToString(ch.ID)+"-"+fmt.Sprint(ch.InitAt.Unix()),
				db.ChannelTask{Address: ch.Our.Address}, &at, nil,
			)
			if err != nil {
				return fmt.Errorf("failed to create task for finalizing channel: %w", err)
			}

			ch.UncoopCloseStarted = true
		case payments.ChannelStatusUninitialized:
			if ch.Status != db.ChannelStateInactive {
				if err := s.db.CreateChannelEvent(ctx, ch, time.Unix(upd.Transaction.At, 0), db.ChannelHistoryItem{
					Action: db.ChannelHistoryActionClosed,
				}); err != nil {
					return fmt.Errorf("failed to create channel event %d: %w", db.ChannelHistoryActionClosed, err)
				}

				log.Info().Str("address", ch.Our.Address).
					Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
					Msg("onchain channel closed")
			}

			ch.Status = db.ChannelStateInactive
		}
	}

	// only when a channel is active because in inactive state wallet seqno can be increased by owner
	if upd.LatestChannel.Status == payments.ChannelStatusOpen {
		needRefresh := true

		pendingCommitKey := pendingIDCommit(upd.LatestChannel.Storage.CommittedSeqno)
		pendingCommit := side.PendingOnchainTransfers[pendingCommitKey]

		if side.LatestCommitedSeqno < upd.LatestChannel.Storage.CommittedSeqno && pendingCommit != nil {
			ch.PendingCommit = nil

			err := s.db.CreateTask(ctx, PaymentsTaskPool, "wait-pending-tx-completion", side.Address+"-chain-balance",
				"wait-pending-tx-completion-"+side.Address+"-"+pendingCommitKey,
				db.WaitPendingTxTask{
					ChannelAddress: ch.Our.Address,
					IsOurSide:      isOur,
					PendingID:      pendingCommitKey,
					MsgHash:        upd.Transaction.In.MsgHash,
					StartedAt:      time.Unix(upd.Transaction.At, 0),
				}, nil, nil,
			)
			if err != nil {
				return fmt.Errorf("failed creating follow pending task: %w", err)
			}
			needRefresh = false
		}

		if side.LatestWalletSeqno < upd.LatestChannel.Storage.WalletSeqno {
			pendingWalletKey := pendingIDWallet(side.LatestWalletSeqno)
			pendingMsg := side.PendingOnchainTransfers[pendingWalletKey]

			if pendingMsg != nil {
				err := s.db.CreateTask(ctx, PaymentsTaskPool, "wait-pending-tx-completion", side.Address+"-chain-balance",
					"wait-pending-tx-completion-"+side.Address+"-"+pendingWalletKey,
					db.WaitPendingTxTask{
						ChannelAddress: ch.Our.Address,
						IsOurSide:      isOur,
						PendingID:      pendingWalletKey,
						MsgHash:        upd.Transaction.In.MsgHash,
						StartedAt:      time.Unix(upd.Transaction.At, 0),
					}, nil, nil,
				)
				if err != nil {
					return fmt.Errorf("failed creating follow pending task: %w", err)
				}
				needRefresh = false
			}
		}

		if needRefresh {
			err := s.db.CreateTask(ctx, PaymentsTaskPool, "refresh-onchain-balance", side.Address+"-chain-balance",
				"refresh-onchain-balance-"+side.Address+"-"+fmt.Sprint(upd.Transaction.LT),
				db.RefreshOnchainBalanceTask{
					ChannelAddress: ch.Our.Address,
					IsOurSide:      isOur,
					BlockAfter:     upd.Transaction.At,
				}, nil, nil,
			)
			if err != nil {
				return fmt.Errorf("failed creating refresh balance task: %w", err)
			}
		}
	}

	side.IsSettlementFinalized = upd.LatestChannel.Storage.Quarantine != nil &&
		upd.LatestChannel.Storage.Quarantine.OurSettlementFinalized

	side.LatestCommitedSeqno = upd.LatestChannel.Storage.CommittedSeqno
	side.LatestWalletSeqno = upd.LatestChannel.Storage.WalletSeqno
	side.LatestProcessedLT = upd.Transaction.LT
	side.LastProcessedTxAt = time.Unix(upd.Transaction.At, 0)

	return nil
}

func (s *Service) Start() {
	go s.taskExecutor()
	go s.derivativePriceWorker()
	s.startVaultWorker()
	if s.useMetrics {
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
		case *ChannelUpdatedEvent:
			channelJson, _ := json.Marshal(upd.LatestChannel)
			ok, weA, isOur := s.verifyChannel(upd.LatestChannel)
			if !ok {
				log.Debug().Str("channel", string(channelJson)).Msg("not verified")
				continue
			}

		retry:
			var err error
			var channel *db.Channel
			for {
				select {
				case <-s.globalCtx.Done():
					return
				default:
				}

				addr := upd.LatestChannel.Address.String()
				if !isOur {
					addr = upd.LatestChannel.GetPartyAddr().String()
				}

				// TODO: not block, DLQ?
				channel, err = s.db.GetChannel(context.Background(), addr)
				if err != nil && !errors.Is(err, db.ErrNotFound) {
					log.Error().Err(err).Msg("failed to get channel from db, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}
				break
			}

			if (channel == nil || (!channel.Our.ActiveOnchain && !channel.Their.ActiveOnchain)) &&
				upd.LatestChannel.Status == payments.ChannelStatusUninitialized {
				// to not reset the offchain channel which is not yet activated onchain
				continue
			}

			if !isOur {
				log.Debug().Str("channel", string(channelJson)).Msg("their verified, processing update")

				if channel != nil {
					if upd.Transaction.LT <= channel.Their.LatestProcessedLT {
						// repeated, but already processed
						continue
					}

					if upd.Transaction.Success && upd.Transaction.In.Type == tlb.MsgTypeExternalIn {
						var settle payments.SettleMsg
						if err := tlb.Parse(&settle, upd.Transaction.In.Body); err == nil {
							// we need to check their conditional resolves and add missing if any, to resolve our next channels
							// it will also update channel actions to execute later
							s.scanSettledConditionals(context.Background(), channel, upd.Transaction, settle, false)
						}
					}

					prevStatus := channel.Status
					err = s.db.Transaction(context.Background(), func(ctx context.Context) error {
						if err = s.processSideUpdate(ctx, channel, false, upd); err != nil {
							return fmt.Errorf("failed to process side update: %w", err)
						}

						if err = s.db.UpdateChannel(ctx, channel); err != nil {
							return fmt.Errorf("failed to set channel in db: %w", err)
						}
						return nil
					})
					if err != nil {
						log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to set channel in db")
						// we retry full process because we need to reproduce all changes in case of concurrent update
						goto retry
					}
					if prevStatus != db.ChannelStateInactive && channel.Status == db.ChannelStateInactive {
						if err = s.finalizeInactiveChannelDerivatives(context.Background(), channel); err != nil {
							log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to finalize inactive channel derivatives")
						}
					}
				} else {
					// TODO: queue transactions
					go func() {
						log.Debug().Str("channel", string(channelJson)).Msg("not yet initialized, waiting for our side contract init")

						time.Sleep(3 * time.Second)
						// wait for our channel init, should happen in a couple seconds, then retry
						s.updates <- update
					}()
				}

				continue
			}

			// process main events on our channel for consistency (onchain state replicated on contract level)
			log.Debug().Str("channel", string(channelJson)).Msg("our verified, processing update")

			isNew := channel == nil
			if isNew || channel.Status == db.ChannelStateInactive {
				if upd.LatestChannel.Status == payments.ChannelStatusUninitialized {
					continue
				}

				createAt := time.Now()
				var version int64
				if channel != nil {
					version = channel.DBVersion
					createAt = channel.CreatedAt
				}

				ourAddr, theirAddr := upd.LatestChannel.Address, upd.LatestChannel.GetPartyAddr()

				our := db.OnchainState{
					Key: upd.LatestChannel.Storage.KeyB,
				}
				their := db.OnchainState{
					Key: upd.LatestChannel.Storage.KeyA,
				}

				if weA {
					our, their = their, our
				}

				safeDur := int64(s.cfg.BufferTimeToCommit) + int64(upd.LatestChannel.Storage.ClosingConfig.QuarantineDuration) +
					int64(upd.LatestChannel.Storage.ClosingConfig.ConditionalCloseDuration) + int64(upd.LatestChannel.Storage.ClosingConfig.ActionsDuration)

				channel = &db.Channel{
					ID:                     upd.LatestChannel.Storage.ChannelID,
					WeLeft:                 weA,
					SafeOnchainClosePeriod: safeDur,
					AcceptingActions:       upd.LatestChannel.Status == payments.ChannelStatusOpen,
					Our: db.Side{
						Address:                 ourAddr.String(),
						OnchainBalances:         map[string]*big.Int{},
						OnchainInfo:             our,
						Data:                    db.NewAgreedData(),
						LockedDeposits:          make(map[string]*payments.LockedDepositInfo),
						PendingOnchainTransfers: make(map[string]*payments.PendingMessageInfo),
					},
					Their: db.Side{
						Address:                 theirAddr.String(),
						OnchainBalances:         map[string]*big.Int{},
						OnchainInfo:             their,
						Data:                    db.NewAgreedData(),
						LockedDeposits:          make(map[string]*payments.LockedDepositInfo),
						PendingOnchainTransfers: make(map[string]*payments.PendingMessageInfo),
					},
					InitAt:    time.Unix(upd.Transaction.At, 0),
					CreatedAt: createAt,
					CodeHash:  upd.LatestChannel.Code.Hash(),
					DBVersion: version,
				}
			}

			// TODO: if last lt not prev tx, list transactions and process
			if upd.Transaction.LT <= channel.Our.LatestProcessedLT {
				continue
			}

			fc := s.db.UpdateChannel
			if isNew {
				fc = s.db.CreateChannel
			}

			if upd.Transaction.Success && upd.Transaction.In.Type == tlb.MsgTypeExternalIn {
				var settle payments.SettleMsg
				if err := tlb.Parse(&settle, upd.Transaction.In.Body); err == nil {
					// it will update channel actions to execute later
					s.scanSettledConditionals(context.Background(), channel, upd.Transaction, settle, true)
				}
			}

			if upd.Transaction.Success && upd.Transaction.In.Type == tlb.MsgTypeInternal && upd.LatestChannel.Status == payments.ChannelStatusClosureStarted {
				ourState := channel.LoadSignedState()
				var msg payments.UncoopCloseReplicateMsg
				if err := tlb.Parse(&msg, upd.Transaction.In.Body); err == nil &&
					msg.State != nil && msg.State.Body.Seqno >= ourState.Body.Seqno {
					// We are checking their committed state from tx data and comparing it with ours,
					// it can happen that party will use pending state signed by us, but not respond with his signature.
					// In this case we will take this state from transaction and recover pending data from db,
					// and apply this state in the same way as if we got a signature from the party. Then we will use an updated state to commit data.
					func() {
						if err = msg.State.Verify(channel.SideA().OnchainInfo.Key, channel.SideB().OnchainInfo.Key); err != nil {
							log.Warn().Err(err).Str("channel", channel.Our.Address).Msg("failed to verify channel state from transaction, skipped")
							return
						}

						tb, err := tlb.ToCell(msg.State.Body)
						if err != nil {
							log.Error().Err(err).Msg("failed to serialize state body from transaction")
							return
						}

						ob, err := tlb.ToCell(ourState.Body)
						if err != nil {
							log.Error().Err(err).Msg("failed to serialize state body from db")
						}

						if !bytes.Equal(tb.Hash(), ob.Hash()) {
							log.Warn().Str("address", channel.Our.Address).
								Msg("we detected that party has committed pending channel state, we will recover it from db and will try to apply")

							ps, err := s.db.GetChannelPendingState(context.Background(), channel, msg.State.Body)
							if err != nil {
								log.Error().Err(err).Str("address", channel.Our.Address).
									Msg("we cannot find such pending state in our db, cannot challenge")
							} else {
								updState, err := tlb.ToCell(msg.State)
								if err != nil {
									log.Error().Err(err).Msg("failed to serialize updated state from transaction")
									return
								}

								channel.Our.Data = ps.OurData
								channel.Their.Data = ps.TheirData
								channel.SignedState = updState

								log.Info().Str("address", channel.Our.Address).Msg("state was successfully recovered form pending data")
							}
						}
					}()

				}
			}

			prevStatus := channel.Status
			err = s.db.Transaction(context.Background(), func(ctx context.Context) error {
				if err = s.processSideUpdate(ctx, channel, true, upd); err != nil {
					return fmt.Errorf("failed to process our side update: %w", err)
				}

				if err = fc(ctx, channel); err != nil {
					return fmt.Errorf("failed to set channel in db: %w", err)
				}
				return nil
			})
			if err != nil {
				log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to set channel in db")
				// we retry full process because we need to reproduce all changes in case of concurrent update
				goto retry
			}
			if prevStatus != db.ChannelStateInactive && channel.Status == db.ChannelStateInactive {
				if err = s.finalizeInactiveChannelDerivatives(context.Background(), channel); err != nil {
					log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to finalize inactive channel derivatives")
				}
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

func (s *Service) DebugPrintChannelInfo(ctx context.Context, addr string) {
	ch, err := s.db.GetChannel(ctx, addr)
	if err != nil {
		log.Error().Err(err).Str("address", addr).Msg("failed to get channel")
		return
	}

	state := ch.LoadSignedState()

	// Pre-compute balances
	toThemBalances, errOut := ch.CalcBalance(ctx, false, s)
	fromThemBalances, errIn := ch.CalcBalance(ctx, true, s)

	// Helper to format amount for a given balance id and value
	formatAmt := func(balanceID string, nano *big.Int) string {
		cc, err := s.ResolveBalanceType(balanceID)
		if err != nil || cc == nil {
			return nano.String()
		}
		return cc.MustAmount(nano).String()
	}

	// Helper to resolve symbol
	symbol := func(balanceID string) string {
		cc, err := s.ResolveBalanceType(balanceID)
		if err != nil || cc == nil || cc.Symbol == "" {
			return balanceID
		}
		return cc.Symbol
	}

	// Build readable block
	var sb strings.Builder
	sep := strings.Repeat("-", 78)
	fmt.Fprintf(&sb, "\n%s\n", sep)
	fmt.Fprintf(&sb, "Payment Channel: %s\n", ch.Our.Address)
	fmt.Fprintf(&sb, "Peer: %s\n", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key))
	fmt.Fprintf(&sb, "Seqno: %d\n", state.Body.Seqno)
	fmt.Fprintf(&sb, "Flags: urgent=%v accepting_actions=%v we_master=%v web_peer=%v\n", ch.UrgentForUs, ch.AcceptingActions, ch.WeLeft, ch.WebPeer)
	fmt.Fprintf(&sb, "On-chain: our=%v their=%v\n", ch.Our.ActiveOnchain, ch.Their.ActiveOnchain)
	fmt.Fprintf(&sb, "InitAt: %s | CreatedAt: %s\n", ch.InitAt.Format(time.RFC3339), ch.CreatedAt.Format(time.RFC3339))
	if ch.SafeOnchainClosePeriod > 0 {
		fmt.Fprintf(&sb, "Safe close window: %s\n", time.Duration(ch.SafeOnchainClosePeriod)*time.Second)
	}

	// Pending commit
	if ch.PendingCommit != nil {
		fmt.Fprintf(&sb, "Pending Commit: seqno=%d msg_hash=%s\n", ch.PendingCommit.Seqno, base64.StdEncoding.EncodeToString(ch.PendingCommit.Message.Hash()))
	}

	// Per-side info
	fmt.Fprintf(&sb, "\nSides:\n")
	fmt.Fprintf(&sb, "  Our:   addr=%s active=%v wallet_seqno=%d last_lt=%d last_tx=%s finalized=%v\n",
		ch.Our.Address, ch.Our.ActiveOnchain, ch.Our.LatestWalletSeqno, ch.Our.LatestProcessedLT, ch.Our.LastProcessedTxAt.Format(time.RFC3339), ch.Our.IsSettlementFinalized)
	fmt.Fprintf(&sb, "  Their: addr=%s active=%v wallet_seqno=%d last_lt=%d last_tx=%s finalized=%v\n",
		ch.Their.Address, ch.Their.ActiveOnchain, ch.Their.LatestWalletSeqno, ch.Their.LatestProcessedLT, ch.Their.LastProcessedTxAt.Format(time.RFC3339), ch.Their.IsSettlementFinalized)

	// On-chain balances per side
	printBalances := func(title string, bals map[string]*big.Int) {
		if len(bals) == 0 {
			fmt.Fprintf(&sb, "  %s: none\n", title)
			return
		}
		keys := make([]string, 0, len(bals))
		for k := range bals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&sb, "  %s:\n", title)
		for _, id := range keys {
			fmt.Fprintf(&sb, "    • %-8s %s\n", symbol(id), formatAmt(id, bals[id]))
		}
	}
	fmt.Fprintf(&sb, "\nOn-chain balances:\n")
	printBalances("our", ch.Our.OnchainBalances)
	printBalances("their", ch.Their.OnchainBalances)

	// Locked deposits per side
	printLocked := func(title string, lds map[string]*payments.LockedDepositInfo) {
		if len(lds) == 0 {
			fmt.Fprintf(&sb, "  %s: none\n", title)
			return
		}
		keys := make([]string, 0, len(lds))
		for k := range lds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&sb, "  %s:\n", title)
		for _, id := range keys {
			ld := lds[id]
			fmt.Fprintf(&sb, "    • %-8s amount=%s used=%s avail=%s till=%s\n",
				symbol(id), formatAmt(id, ld.Amount), formatAmt(id, ld.Used), formatAmt(id, ld.Available()), ld.Till.Format(time.RFC3339))
		}
	}
	fmt.Fprintf(&sb, "\nLocked deposits (rented capacity):\n")
	printLocked("our", ch.Our.LockedDeposits)
	printLocked("their", ch.Their.LockedDeposits)

	// Pending on-chain transfers (holds)
	printPending := func(title string, pend map[string]*payments.PendingMessageInfo) {
		if len(pend) == 0 {
			fmt.Fprintf(&sb, "  %s: none\n", title)
			return
		}
		keys := make([]string, 0, len(pend))
		for k := range pend {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&sb, "  %s:\n", title)
		for _, id := range keys {
			p := pend[id]
			for bid, amt := range p.Amounts {
				fmt.Fprintf(&sb, "    • %-8s amount=%s to=%s, type=%s\n", symbol(id), formatAmt(id, amt), p.CompletionAddress, bid)
			}
		}
	}
	fmt.Fprintf(&sb, "\nPending on-chain transfers (holds):\n")
	printPending("our", ch.Our.PendingOnchainTransfers)
	printPending("their", ch.Their.PendingOnchainTransfers)

	// Aggregated balances (off-chain view)
	printBI := func(title string, bals map[string]*payments.BalanceInfo, err error) {
		fmt.Fprintf(&sb, "\nBalances %s:\n", title)
		if err != nil || len(bals) == 0 {
			if err != nil {
				fmt.Fprintf(&sb, "  error: %v\n", err)
			} else {
				fmt.Fprintf(&sb, "  none\n")
			}
			return
		}
		keys := make([]string, 0, len(bals))
		for k := range bals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, id := range keys {
			b := bals[id]
			fmt.Fprintf(&sb, "  • %-8s avail=%s | onchain=%s action=%s on_hold=%s cond_locked=%s cond_pending=%s\n",
				symbol(id), formatAmt(id, b.Available()), formatAmt(id, b.Onchain), formatAmt(id, b.Action), formatAmt(id, b.OnHold), formatAmt(id, b.ConditionalLocked), formatAmt(id, b.ConditionalPending))
		}
	}
	printBI("to THEM (our -> their)", toThemBalances, errOut)
	printBI("from THEM (their -> our)", fromThemBalances, errIn)

	// Conditionals (virtual channels, etc.)
	fmt.Fprintf(&sb, "\nConditionals:\n")
	printConds := func(title string, d *cell.Dictionary) {
		all, err := d.LoadAll()
		if err != nil || len(all) == 0 {
			if err != nil {
				fmt.Fprintf(&sb, "  %s: error: %v\n", title, err)
			} else {
				fmt.Fprintf(&sb, "  %s: none\n", title)
			}
			return
		}
		fmt.Fprintf(&sb, "  %s:\n", title)
		for _, kv := range all {
			cond, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
			if err != nil {
				fmt.Fprintf(&sb, "    • unknown conditional: %s\n", base64.StdEncoding.EncodeToString(kv.Value.MustToCell().Hash()))
				continue
			}
			info := cond.GetLogInfo()
			var parts []string
			if info != nil {
				keys := make([]string, 0, len(info))
				for k := range info {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					parts = append(parts, fmt.Sprintf("%s=%v", k, info[k]))
				}
			}
			dl := cond.GetDeadline()
			safe := dl.Add(-time.Duration(ch.SafeOnchainClosePeriod) * time.Second)
			if !dl.IsZero() {
				parts = append(parts, fmt.Sprintf("deadline=%s", dl.Format(time.RFC3339)))
				parts = append(parts, fmt.Sprintf("till_deadline=%s", time.Until(dl).Truncate(time.Second)))
				parts = append(parts, fmt.Sprintf("till_safe_deadline=%s", time.Until(safe).Truncate(time.Second)))
			}
			fmt.Fprintf(&sb, "    • %s\n", strings.Join(parts, " "))
		}
	}
	printConds("from US (outgoing)", ch.Our.Data.Conditionals)
	printConds("to US (incoming)", ch.Their.Data.Conditionals)

	// Action states
	fmt.Fprintf(&sb, "\nAction states:\n")
	printActions := func(title string, d *cell.Dictionary) {
		all, err := d.LoadAll()
		if err != nil || len(all) == 0 {
			if err != nil {
				fmt.Fprintf(&sb, "  %s: error: %v\n", title, err)
			} else {
				fmt.Fprintf(&sb, "  %s: none\n", title)
			}
			return
		}
		fmt.Fprintf(&sb, "  %s:\n", title)
		for _, kv := range all {
			id := kv.Key.MustLoadSlice(256)
			act, err := s.ResolveAction(ctx, id)
			if err != nil {
				fmt.Fprintf(&sb, "    • unknown action: %s\n", base64.StdEncoding.EncodeToString(id))
				continue
			}
			// try to parse common send state
			var st actions.StateActionSend
			if err := payments.LoadState(&st, kv.Value.MustToCell()); err == nil {
				ccs := act.GetAffectedCoins()
				bid := ccs[0].BalanceID
				fmt.Fprintf(&sb, "    • send %-8s amount=%s commited=%s last_seqno=%d\n",
					symbol(bid), ccs[0].MustAmount(st.Amount.Nano()).String(), ccs[0].MustAmount(st.Commited.Nano()).String(), st.CommitedSeqno)
			} else {
				fmt.Fprintf(&sb, "    • action=%T state_hash=%s\n", act, base64.StdEncoding.EncodeToString(kv.Value.MustToCell().Hash()))
			}
		}
	}
	printActions("our", ch.Our.Data.ActionStates)
	printActions("their", ch.Their.Data.ActionStates)

	fmt.Fprintf(&sb, "%s\n", sep)
	log.Info().Msg(sb.String())
}

func (s *Service) DebugPrintChannels(ctx context.Context, status db.ChannelStatus) {
	chs, err := s.db.GetChannels(ctx, nil, status)
	if err != nil {
		log.Error().Err(err).Msg("failed to get active channels")
		return
	}

	if len(chs) == 0 {
		log.Info().Msg("no active channels")
		return
	}

	for _, ch := range chs {
		log.Info().Str("address", ch.Our.Address).Str("their_address", ch.Their.Address).
			Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
			Uint64("seqno", ch.LoadSignedState().Body.Seqno).
			Bool("urgent", ch.UrgentForUs).
			Time("inited_at", ch.InitAt).
			Bool("accepting_actions", ch.AcceptingActions).
			Bool("we_master", ch.WeLeft).
			Bool("our_onchain", ch.Our.ActiveOnchain).
			Bool("their_onchain", ch.Their.ActiveOnchain).Msg("")
	}
	/*
		for _, ch := range chs {
			inBalance, outBalance := "?", "?"
			val, err := ch.CalcBalance(ctx, false, s)
			if err == nil {
				outBalance = tlb.MustFromNano(val, int(cc.Decimals)).String()
			}

			val, _, err = ch.CalcBalance(ctx, true, s)
			if err == nil {
				inBalance = cc.MustAmount(val).String()
			}

			lg := log.Info().Str("address", ch.Our.Address).
				Str("with", base64.StdEncoding.EncodeToString(ch.Their.OnchainInfo.Key)).
				Str("out_deposit", cc.MustAmount(ch.OurOnchain.Deposited).String()).
				Str("out_withdrawn", cc.MustAmount(ch.OurOnchain.Withdrawn).String()).
				Str("balance_out", outBalance).
				Str("in_deposit", cc.MustAmount(ch.TheirOnchain.Deposited).String()).
				Str("in_withdrawn", cc.MustAmount(ch.TheirOnchain.Withdrawn).String()).
				Str("balance_in", inBalance).
				Uint64("seqno_their", ch.Their.State.Data.Seqno).
				Uint64("seqno_our", ch.Our.State.Data.Seqno).
				Bool("accepting_actions", ch.AcceptingActions).
				Bool("we_master", ch.WeLeft).
				Str("our_locked_dep", cc.MustAmount(ch.OurLockedDeposit.Available()).String()).
				Str("their_locked_dep", cc.MustAmount(ch.TheirLockedDeposit.Available()).String()).
				Bool("onchain", ch.ActiveOnchain)

			if ch.OurLockedDeposit != nil {
				lg.Str("our_locked_dep_used", cc.MustAmount(ch.OurLockedDeposit.Used).String())
				lg.Str("our_locked_dep_amount", cc.MustAmount(ch.OurLockedDeposit.Amount).String())
			}

			if ch.TheirLockedDeposit != nil {
				lg.Str("their_locked_dep_used", cc.MustAmount(ch.TheirLockedDeposit.Used).String())
				lg.Str("their_locked_dep_amount", cc.MustAmount(ch.TheirLockedDeposit.Amount).String())
			}

			lg.Msg("active onchain channel")

			for _, kv := range ch.Our.Conditionals.All() {
				vch, _ := payments.ParseVirtualChannelCond(kv.Value.MustBeginParse())
				till := time.Unix(vch.Deadline, 0).Sub(time.Now())
				log.Info().
					Str("capacity", cc.MustAmount(vch.Capacity).String()).
					Str("till_deadline", till.String()).
					Str("till_safe_deadline", (till-time.Duration(ch.SafeOnchainClosePeriod)*time.Second).String()).
					Str("fee", cc.MustAmount(vch.Fee).String()).
					Str("prepaid", cc.MustAmount(vch.Prepay).String()).
					Str("key", base64.StdEncoding.EncodeToString(vch.Key)).
					Msg("virtual from us")
			}
			for _, kv := range ch.Their.Conditionals.All() {
				vch, _ := payments.ParseVirtualChannelCond(kv.Value.MustBeginParse())

				log.Info().
					Str("capacity", tlb.MustFromNano(vch.Capacity, int(cc.Decimals)).String()).
					Str("till_deadline", time.Unix(vch.Deadline, 0).Sub(time.Now()).String()).
					Str("fee", tlb.MustFromNano(vch.Fee, int(cc.Decimals)).String()).
					Str("prepaid", tlb.MustFromNano(vch.Prepay, int(cc.Decimals)).String()).
					Str("key", base64.StdEncoding.EncodeToString(vch.Key)).
					Msg("virtual to us")
			}
		}*/
}

func (s *Service) GetActiveChannel(ctx context.Context, channelAddr string) (*db.Channel, error) {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return nil, err
	}

	if channel.Status != db.ChannelStateActive {
		return nil, ErrNotActive
	}

	if channel.LoadSignedState().IsEmpty() {
		return nil, fmt.Errorf("states not exchanged yet")
	}

	return channel, nil
}

func (s *Service) OpenChannelOffchain(ctx context.Context, cfg *payments.OpenConfigContainer, codeHash, authorizedKey []byte, urgent, withWebPeer bool) (*address.Address, []byte, error) {
	isLeft := bytes.Equal(cfg.KeyA, s.key.Public().(ed25519.PublicKey))
	if !isLeft && !bytes.Equal(cfg.KeyB, s.key.Public().(ed25519.PublicKey)) {
		return nil, nil, fmt.Errorf("unknown keys")
	}

	theirKey := cfg.KeyB
	if !isLeft {
		theirKey = cfg.KeyA
	}

	if !bytes.Equal(theirKey, authorizedKey) {
		return nil, nil, fmt.Errorf("authorized key mismatch")
	}

	var code *cell.Cell
	for _, cd := range payments.PaymentChannelCodes {
		if bytes.Equal(cd.Hash(), codeHash) {
			code = cd
			break
		}
	}

	if code == nil {
		return nil, nil, fmt.Errorf("unknown payment channel code")
	}

	// TODO: hook to ask user if he wants it

	if cfg.Seqno != 0 {
		return nil, nil, fmt.Errorf("reopen is not supported, assign a new channel")
	}

	initBody, data, initSig, err := s.channelClient.GetDeployAsyncChannelParams(cfg.ChannelID, isLeft, cfg.Seqno, s.key, theirKey, cfg.InitSignature.Value, cfg.ClosingConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get deploy params: %w", err)
	}

	si, err := tlb.ToCell(tlb.StateInit{
		Code: code,
		Data: data,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize state init: %w", err)
	}

	proposed, err := s.channelClient.ParseChannel(address.NewAddress(0, 0, si.Hash()), code, data, false)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse channel: %w", err)
	}

	// verify that config suits us
	if ok, _, _ := s.verifyChannel(proposed); !ok {
		return nil, nil, fmt.Errorf("verification not passed")
	}

	channel, err := s.db.GetChannel(ctx, proposed.Address.String())
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, nil, fmt.Errorf("failed to get channel: %w", err)
	}

	if channel != nil && channel.Status != db.ChannelStateInactive {
		// already initialized, idempotency
		return address.MustParseAddr(channel.Our.Address), initSig, nil
	}

	our := db.OnchainState{
		Key: cfg.KeyA,
	}
	their := db.OnchainState{
		Key: cfg.KeyB,
	}

	createAt := time.Now()
	var version int64
	var latestSeqno uint64
	if channel != nil {
		version = channel.DBVersion
		createAt = channel.CreatedAt

		latestSeqno = channel.Our.LatestCommitedSeqno
		if channel.Their.LatestCommitedSeqno > latestSeqno {
			latestSeqno = channel.Their.LatestCommitedSeqno
		}
	}

	if !isLeft {
		our, their = their, our
	}

	var exists = channel != nil
	safeDur := int64(s.cfg.BufferTimeToCommit) + int64(cfg.ClosingConfig.QuarantineDuration) +
		int64(cfg.ClosingConfig.ConditionalCloseDuration) + int64(cfg.ClosingConfig.ActionsDuration)

	channel = &db.Channel{
		ID:                     cfg.ChannelID,
		Status:                 db.ChannelStateActive,
		WeLeft:                 isLeft,
		SafeOnchainClosePeriod: safeDur,
		AcceptingActions:       true,
		WebPeer:                withWebPeer,
		UrgentForUs:            urgent,
		Our: db.Side{
			Address:                 proposed.Address.String(),
			OnchainBalances:         map[string]*big.Int{},
			OnchainInfo:             our,
			Data:                    db.NewAgreedData(),
			LockedDeposits:          make(map[string]*payments.LockedDepositInfo),
			PendingOnchainTransfers: make(map[string]*payments.PendingMessageInfo),
			LatestCommitedSeqno:     latestSeqno,
		},
		Their: db.Side{
			Address:                 proposed.GetPartyAddr().String(),
			OnchainBalances:         map[string]*big.Int{},
			OnchainInfo:             their,
			Data:                    db.NewAgreedData(),
			LockedDeposits:          make(map[string]*payments.LockedDepositInfo),
			PendingOnchainTransfers: make(map[string]*payments.PendingMessageInfo),
			LatestCommitedSeqno:     latestSeqno,
		},
		InitAt:          time.Now(),
		CreatedAt:       createAt,
		CodeHash:        codeHash,
		InitMessageBody: initBody,
		InitialData:     data,
		DBVersion:       version,
	}

	fc := s.db.UpdateChannel
	if !exists {
		fc = s.db.CreateChannel
	}

	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		if err = fc(ctx, channel); err != nil {
			return fmt.Errorf("failed to set channel in db: %w", err)
		}

		if urgent {
			err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Our.Address,
				"exchange-states-"+channel.Our.Address+"-"+fmt.Sprint(channel.InitAt.Unix()),
				db.IncrementStatesTask{ChannelAddress: channel.Our.Address, WantResponse: true}, nil, nil,
			)
			if err != nil {
				return fmt.Errorf("failed to create task for incrementing states: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to exec channel db tx: %w", err)
	}

	return address.MustParseAddr(channel.Our.Address), initSig, nil
}

func (s *Service) GetVirtualChannelMeta(ctx context.Context, key ed25519.PublicKey) (*db.ConditionalMeta, error) {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return nil, err
	}

	return meta, nil
}

func (s *Service) getTransport(ch *db.Channel) Transport {
	if ch.WebPeer && s.webTransport != nil {
		return s.webTransport
	}

	return s.regularTransport
}

func (s *Service) requestAction(ctx context.Context, channelAddress string, action any) ([]byte, error) {
	channel, err := s.db.GetChannel(ctx, channelAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	decision, err := s.getTransport(channel).RequestAction(ctx, address.MustParseAddr(channel.Their.Address), channel.Their.OnchainInfo.Key, action)
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
// Must be called from worker only to ensure rollbacks are always happening in case of error.
func (s *Service) proposeAction(ctx context.Context, lockId int64, channelAddress string, action transport.Action, details any) error {
	channel, err := s.db.GetChannel(ctx, channelAddress)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	onSuccess, state, err := s.updateOurStateWithAction(ctx, channel, action, details)
	if err != nil {
		return fmt.Errorf("failed to prepare actions for the next node - %w: %v", ErrNotPossible, err)
	}

	// we should keep the pending state in case of party will decide to use it onchain uncooperatively without confirmation to our request
	// if it happens, we will be able to apply it and commit actions
	if err = s.db.SaveChannelPendingState(ctx, channel, state.Body); err != nil {
		return fmt.Errorf("failed to save pending state: %w", err)
	}

	toSend, err := tlb.ToCell(state)
	if err != nil {
		return fmt.Errorf("failed to serialize signed state: %w", err)
	}

	res, err := s.getTransport(channel).ProposeAction(ctx, lockId, address.MustParseAddr(channel.Their.Address), channel.Their.OnchainInfo.Key, toSend, action)
	if err != nil {
		return fmt.Errorf("failed to propose actions: %w", err)
	}

	if !res.Agreed {
		if res.Reason == db.ErrChannelBusy.Error() {
			// we can retry later, no need to revert
			return ErrChannelIsBusy
		}
		log.Warn().Str("reason", res.Reason).Msg("actions proposal denied")
		return ErrDenied
	}

	var theirState payments.StateBodySigned
	if err = tlb.Parse(&theirState, res.SignedState); err != nil {
		return fmt.Errorf("failed to parse their updated channel state: %w", err)
	}

	if err = theirState.Verify(channel.SideA().OnchainInfo.Key, channel.SideB().OnchainInfo.Key); err != nil {
		return fmt.Errorf("failed to verify their state signature: %w", err)
	}

	ourStateBodyCell, err := tlb.ToCell(state.Body)
	if err != nil {
		return fmt.Errorf("failed to serialize our body state: %w", err)
	}

	theirStateBodyCell, err := tlb.ToCell(theirState.Body)
	if err != nil {
		return fmt.Errorf("failed to serialize their body state: %w", err)
	}

	if !bytes.Equal(ourStateBodyCell.Hash(), theirStateBodyCell.Hash()) {
		return fmt.Errorf("their state body hash doesn't match ours")
	}

	if channel.WeLeft {
		state.SignatureB = theirState.SignatureB
	} else {
		state.SignatureA = theirState.SignatureA
	}

	toSave, err := tlb.ToCell(state)
	if err != nil {
		return fmt.Errorf("failed to serialize signed state to save: %w", err)
	}

	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		// renew their state to update their reference to our state
		channel.SignedState = toSave
		if err = s.db.UpdateChannel(ctx, channel); err != nil {
			return fmt.Errorf("failed to update channel in db: %w", err)
		}

		// we can delete pending now because it is confirmed and saved
		if err = s.db.CleanupChannelPendingStates(ctx, channel, state.Body); err != nil {
			return fmt.Errorf("failed to delete pending state: %w", err)
		}

		if onSuccess != nil {
			if err = onSuccess(ctx); err != nil {
				return fmt.Errorf("failed to execute on success in tx: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (s *Service) verifyChannel(p *payments.ChannelContract) (ok bool, isLeft bool, isOur bool) {
	isLeft = bytes.Equal(p.Storage.KeyA, s.key.Public().(ed25519.PublicKey))

	if bytes.Equal(p.Storage.KeyB, p.Storage.KeyA) {
		// wat?
		return false, false, false
	}

	if !isLeft && !bytes.Equal(p.Storage.KeyB, s.key.Public().(ed25519.PublicKey)) {
		return false, false, false
	}

	if p.Storage.ClosingConfig.ConditionalCloseDuration != s.cfg.ConditionalCloseDurationSec ||
		p.Storage.ClosingConfig.QuarantineDuration != s.cfg.QuarantineDurationSec ||
		p.Storage.ClosingConfig.ActionsDuration != s.cfg.ActionsDuration ||
		p.Storage.ClosingConfig.ReplicationMessageAttachAmount.Nano().Cmp(tlb.MustFromTON(s.cfg.ReplicationMessageAttachAmount).Nano()) != 0 {
		log.Debug().Msg("channel config mismatch, rejecting")
		return false, false, false
	}
	return true, isLeft, isLeft == p.Storage.IsA
}

func (s *Service) IncrementStates(ctx context.Context, channelAddr string, wantResponse bool) error {
	channel, err := s.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "increment-state", channel.Our.Address,
		"increment-state-"+channel.Our.Address+"-force-"+fmt.Sprint(time.Now().UnixNano()),
		db.IncrementStatesTask{
			ChannelAddress: channel.Our.Address,
			WantResponse:   wantResponse,
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create increment-state task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) RemoveConditional(ctx context.Context, key ed25519.PublicKey) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return err
	}

	if meta.Incoming == nil {
		return fmt.Errorf("conditional has no incoming channel")
	}

	if meta.Outgoing != nil && !meta.Outgoing.UncooperativeDeadline.Before(time.Now()) {
		return fmt.Errorf("outgoing direction is not timed out yet, not safe")
	}

	if meta.Status != db.ConditionalStateActive && meta.Status != db.ConditionalStatePending {
		return fmt.Errorf("conditional is not active or pending")
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "remove-cond", meta.Incoming.ChannelAddress,
		"remove-cond-"+base64.StdEncoding.EncodeToString(meta.Key),
		db.RemoveConditionalTask{
			Key: meta.Key,
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create remove-cond task: %w", err)
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

	if !bytes.Equal(channel.Their.OnchainInfo.Key, key) {
		return fmt.Errorf("unauthorized channel")
	}

	s.lockerMx.Lock()
	defer s.lockerMx.Unlock()

	l, ok := s.channelLocks[channel.Our.Address]
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

	if !bytes.Equal(channel.Their.OnchainInfo.Key, key) {
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
			log.Debug().Str("channel", addr.String()).Int64("id", id).Msg("external lock unlocked")
		}
		// already unlocked (idempotency)
		return nil
	}

	if unlockFunc != nil {
		// already locked by other party (idempotency)
		log.Debug().Str("channel", addr.String()).Int64("id", id).Msg("external lock already locked")

		return ErrChannelIsBusy
	}

	// this call is fast, because we are master
	_, _, unlock, err := s.AcquireChannel(ctx, channel.Our.Address, id)
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

			theirAddr := address.MustParseAddr(channel.Their.Address)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			res, err := s.getTransport(channel).IsChannelUnlocked(ctx, channel.Their.OnchainInfo.Key, theirAddr, -id)
			cancel()
			if err != nil {
				continue
			}

			if !res.Agreed {
				log.Warn().Str("channel", addr.String()).Int64("id", id).Str("reason", res.Reason).Msg("external lock seems still locked")

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

	log.Debug().Str("channel", addr.String()).Int64("id", id).Msg("external lock accepted")

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

	l, ok := s.channelLocks[channel.Our.Address]
	if !ok {
		l = &channelLock{
			queue: make(chan bool, 1),
		}
		s.channelLocks[channel.Our.Address] = l
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
	lockID := l.id

	log.Debug().Str("channel", addr).Bool("master", channel.WeLeft).Int64("id", lockID).Msg("acquiring lock")

	s.lockerMx.Unlock()

	// left side is lock master, negative means lock from other side (master locks us)
	if channel.WeLeft || lockID < 0 {
		return channel, lockID, func() {
			l.mx.Unlock()

			log.Debug().Str("channel", addr).Int64("id", lockID).Msg("local lock released")
		}, nil
	}

	// l.pending = true
	theirChAddr := address.MustParseAddr(channel.Their.Address)
	res, err := s.getTransport(channel).RequestChannelLock(ctx, channel.Their.OnchainInfo.Key, theirChAddr, lockID, true)
	// l.pending = false
	if err != nil {
		l.mx.Unlock()

		return nil, 0, nil, err
	}

	if !res.Agreed {
		l.mx.Unlock()

		log.Debug().Str("channel", addr).Int64("id", lockID).Str("reason", res.Reason).Msg("external lock not obtained")

		return nil, 0, nil, db.ErrChannelBusy
	}

	return channel, lockID, func() {
		l.mx.Unlock()

		res, err := s.getTransport(channel).RequestChannelLock(ctx, channel.Their.OnchainInfo.Key, theirChAddr, lockID, false)
		if err != nil {
			log.Warn().Str("channel", addr).Int64("id", lockID).Err(err).Msg("external lock release failed, but state can be fetched by another party, no worries")
		} else if !res.Agreed {
			log.Warn().Str("channel", addr).Int64("id", lockID).Str("reason", res.Reason).Msg("external lock release failed, not accepted by party")
		} else {
			log.Debug().Str("channel", addr).Int64("id", lockID).Msg("external lock released")
		}
	}, nil
}

func (s *Service) GetKnownBalanceTypes() []*payments.CoinConfig {
	configs := make([]*payments.CoinConfig, 0, len(s.knownBalanceTypes))
	for _, cfg := range s.knownBalanceTypes {
		configs = append(configs, cfg)
	}
	return configs
}
