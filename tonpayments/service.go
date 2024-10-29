package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
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
	GetChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey) (*transport.ChannelConfig, error)
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

	GetVirtualChannelMeta(ctx context.Context, key []byte) (*db.VirtualChannelMeta, error)
	UpdateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error
	CreateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error

	SetBlockOffset(ctx context.Context, seqno uint32) error
	GetBlockOffset(ctx context.Context) (*db.BlockOffset, error)

	GetChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error)
	CreateChannel(ctx context.Context, channel *db.Channel) error
	GetChannel(ctx context.Context, addr string) (*db.Channel, error)
	UpdateChannel(ctx context.Context, channel *db.Channel) error
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

type Service struct {
	ton       ton.APIClientWrapped
	transport Transport
	updates   chan any
	db        DB
	webhook   Webhook

	key ed25519.PrivateKey

	virtualChannelProxyFee         tlb.Coins
	excessFee                      tlb.Coins
	wallet                         *wallet.Wallet
	contractMaker                  *payments.Client
	closingConfig                  payments.ClosingConfig
	virtualChannelsLimitPerChannel int
	workerSignal                   chan bool

	// TODO: channel based lock
	mx sync.Mutex

	externalLock func()

	channelLocks     map[string]*channelLock
	lockerMx         sync.Mutex
	externalLockerMx sync.Mutex

	discoveryMx sync.Mutex
}

func NewService(api ton.APIClientWrapped, db DB, transport Transport, wallet *wallet.Wallet, updates chan any, key ed25519.PrivateKey, cfg config.ChannelConfig) *Service {
	return &Service{
		ton:                    api,
		transport:              transport,
		updates:                updates,
		db:                     db,
		key:                    key,
		virtualChannelProxyFee: tlb.MustFromTON(cfg.VirtualChannelProxyFee),
		excessFee:              tlb.MustFromTON("0.01"),
		wallet:                 wallet,
		contractMaker:          payments.NewPaymentChannelClient(api),
		closingConfig: payments.ClosingConfig{
			QuarantineDuration:       cfg.QuarantineDurationSec,
			MisbehaviorFine:          tlb.MustFromTON(cfg.MisbehaviorFine),
			ConditionalCloseDuration: cfg.ConditionalCloseDurationSec,
		},
		virtualChannelsLimitPerChannel: 30000,
		workerSignal:                   make(chan bool, 1),
		channelLocks:                   map[string]*channelLock{},
	}
}

func (s *Service) SetWebhook(webhook Webhook) {
	s.webhook = webhook
}

func (s *Service) GetChannelConfig() transport.ChannelConfig {
	return transport.ChannelConfig{
		ExcessFee:                s.excessFee.Nano().Bytes(),
		VirtualTunnelFee:         s.virtualChannelProxyFee.Nano().Bytes(),
		WalletAddr:               s.wallet.WalletAddress().Data(),
		QuarantineDuration:       s.closingConfig.QuarantineDuration,
		MisbehaviorFine:          s.closingConfig.MisbehaviorFine.Nano().Bytes(),
		ConditionalCloseDuration: s.closingConfig.ConditionalCloseDuration,
	}
}

func (s *Service) Start() {
	if err := s.addPeersForChannels(); err != nil {
		log.Error().Err(err).Msg("failed to add urgent peers")
	}

	go s.taskExecutor()

	for update := range s.updates {
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

			var err error
			var channel *db.Channel
			for {
				// TODO: not block, DLQ
				channel, err = s.db.GetChannel(context.Background(), upd.Channel.Address().String())
				if err != nil && !errors.Is(err, db.ErrNotFound) {
					log.Error().Err(err).Msg("failed to get channel from db, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}
				break
			}

			our := db.OnchainState{
				Key:            upd.Channel.Storage.KeyB,
				CommittedSeqno: upd.Channel.Storage.CommittedSeqnoB,
				WalletAddress:  upd.Channel.Storage.Payments.DestB.String(),
				Deposited:      upd.Channel.Storage.BalanceB.Nano(),
			}
			their := db.OnchainState{
				Key:            upd.Channel.Storage.KeyA,
				CommittedSeqno: upd.Channel.Storage.CommittedSeqnoA,
				WalletAddress:  upd.Channel.Storage.Payments.DestA.String(),
				Deposited:      upd.Channel.Storage.BalanceA.Nano(),
			}

			if isLeft {
				our, their = their, our
			}

			isNew := channel == nil
			if isNew || channel.Status == db.ChannelStateInactive {
				if upd.Channel.Status == payments.ChannelStatusUninitialized {
					continue
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
					UpdatedAt:              time.Unix(int64(upd.Transaction.Now), 0),
					CreatedAt:              time.Now(),
					AcceptingActions:       upd.Channel.Status == payments.ChannelStatusOpen,
					SafeOnchainClosePeriod: 300 + int64(upd.Channel.Storage.ClosingConfig.QuarantineDuration) + int64(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration),
				}

				if upd.Channel.Storage.JettonConfig != nil && upd.Channel.Storage.JettonConfig.Root != nil {
					channel.JettonAddress = upd.Channel.Storage.JettonConfig.Root.String()
				}
			}

			updatedAt := time.Unix(int64(upd.Transaction.Now), 0)
			if updatedAt.Before(channel.UpdatedAt) {
				continue
			}
			channel.UpdatedAt = updatedAt
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
						Hex("with", channel.TheirOnchain.Key).
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
						Hex("with", channel.TheirOnchain.Key).
						Msg("onchain channel closure started")

					// TODO: maybe check only their?
					if qOur.Seqno < channel.Our.State.Data.Seqno ||
						qTheir.Seqno < channel.Their.State.Data.Seqno {
						// something is outdated, challenge state
						settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+1), 0)
						err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "challenge", channel.Address+"-chain",
							"challenge-"+hex.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
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
					Hex("with", channel.TheirOnchain.Key).
					Time("execute_at", settleAt).
					Msg("onchain channel uncooperative closing event, settling conditions")

				err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "settle", channel.Address+"-settle",
					"settle-"+hex.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
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
					Hex("with", channel.TheirOnchain.Key).
					Float64("till_finalize_sec", time.Until(finishAt).Seconds()).
					Msg("onchain channel awaiting finalization")

				err = s.db.CreateTask(context.Background(), PaymentsTaskPool, "finalize", channel.Address+"-finalize",
					"finalize-"+hex.EncodeToString(channel.ID)+"-"+fmt.Sprint(channel.InitAt.Unix()),
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
						Hex("with", channel.TheirOnchain.Key).
						Msg("onchain channel closed")
				}
			}

			fc := s.db.UpdateChannel
			if isNew {
				fc = s.db.CreateChannel
			}

			for {
				if err = fc(context.Background(), channel); err != nil {
					log.Error().Err(err).Msg("failed to set channel in db, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}

				s.transport.AddUrgentPeer(channel.TheirOnchain.Key)
				break
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
			Str("out_deposit", tlb.FromNanoTON(ch.OurOnchain.Deposited).String()).
			Str("sent_out", ch.Our.State.Data.Sent.String()).
			Str("balance_out", outBalance).
			Str("in_deposit", tlb.FromNanoTON(ch.TheirOnchain.Deposited).String()).
			Str("sent_in", ch.Their.State.Data.Sent.String()).
			Str("balance_in", inBalance).
			Uint64("seqno_their", ch.Their.State.Data.Seqno).
			Uint64("seqno_our", ch.Our.State.Data.Seqno).
			Bool("accepting_actions", ch.AcceptingActions).
			Bool("we_master", ch.WeLeft).
			Msg("active onchain channel")
		for _, kv := range ch.Our.Conditionals.All() {
			vch, _ := payments.ParseVirtualChannelCond(kv.Value.BeginParse())
			log.Info().
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("till_deadline", time.Unix(vch.Deadline, 0).Sub(time.Now()).String()).
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

func (s *Service) requestAction(ctx context.Context, channelAddress string, action any) error {
	channel, err := s.db.GetChannel(ctx, channelAddress)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	decision, err := s.transport.RequestAction(ctx, address.MustParseAddr(channel.Address), channel.TheirOnchain.Key, action)
	if err != nil {
		return fmt.Errorf("failed to request actions: %w", err)
	}

	if !decision.Agreed {
		log.Warn().Str("reason", decision.Reason).Msg("actions request denied")
		return ErrDenied
	}
	return nil
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

	if !isLeft && p.Storage.Payments.DestB.Bounce(false).String() != s.wallet.WalletAddress().String() {
		return false, false
	}

	if isLeft && p.Storage.Payments.DestA.Bounce(false).String() != s.wallet.WalletAddress().String() {
		return false, false
	}

	if p.Storage.Payments.ExcessFee.String() != s.excessFee.String() {
		return false, false
	}

	if p.Storage.ClosingConfig.ConditionalCloseDuration != s.closingConfig.ConditionalCloseDuration ||
		p.Storage.ClosingConfig.QuarantineDuration != s.closingConfig.QuarantineDuration ||
		p.Storage.ClosingConfig.MisbehaviorFine.String() != s.closingConfig.MisbehaviorFine.String() {
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
