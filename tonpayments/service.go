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
	db "github.com/xssnick/ton-payment-network/tonpayments/db"
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
var ErrNotPossible = errors.New("not possible")

type Transport interface {
	AddUrgentPeer(channelKey ed25519.PublicKey)
	RemoveUrgentPeer(channelKey ed25519.PublicKey)
	GetChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey) (*transport.ChannelConfig, error)
	RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action transport.Action) (*transport.Decision, error)
	ProposeAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, state *cell.Cell, action transport.Action) (*transport.ProposalDecision, error)
	RequestInboundChannel(ctx context.Context, capacity *big.Int, ourWallet *address.Address, ourKey, theirKey []byte) (*transport.Decision, error)
}

type DB interface {
	Transaction(ctx context.Context, f func(ctx context.Context) error) error
	CreateTask(ctx context.Context, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context) (*db.Task, error)
	RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, task *db.Task) error

	GetVirtualChannelMeta(ctx context.Context, key []byte) (*db.VirtualChannelMeta, error)
	UpdateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error
	CreateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error

	SetBlockOffset(ctx context.Context, seqno uint32) error
	GetBlockOffset(ctx context.Context) (*db.BlockOffset, error)

	GetActiveChannels(ctx context.Context) ([]*db.Channel, error)
	GetChannelsWithKey(ctx context.Context, key ed25519.PublicKey) ([]*db.Channel, error)
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

type Service struct {
	ton       ton.APIClientWrapped
	transport Transport
	updates   chan any
	db        DB

	key ed25519.PrivateKey

	maxOutboundCapacity  tlb.Coins
	virtualChannelFee    tlb.Coins
	excessFee            tlb.Coins
	wallet               *wallet.Wallet
	contractMaker        *payments.Client
	closingConfig        payments.ClosingConfig
	virtualChannelsLimit int

	// TODO: channel based lock
	mx sync.Mutex
}

func NewService(api ton.APIClientWrapped, db DB, transport Transport, wallet *wallet.Wallet, updates chan any, key ed25519.PrivateKey, closingConfig payments.ClosingConfig) *Service {
	return &Service{
		ton:                  api,
		transport:            transport,
		updates:              updates,
		db:                   db,
		key:                  key,
		maxOutboundCapacity:  tlb.MustFromTON("15.00"),
		virtualChannelFee:    tlb.MustFromTON("0.01"),
		excessFee:            tlb.MustFromTON("0.01"),
		wallet:               wallet,
		contractMaker:        payments.NewPaymentChannelClient(api),
		closingConfig:        closingConfig,
		virtualChannelsLimit: 3000,
	}
}

func (s *Service) GetChannelConfig() transport.ChannelConfig {
	return transport.ChannelConfig{
		ExcessFee:                s.excessFee.Nano().Bytes(),
		WalletAddr:               s.wallet.WalletAddress().Data(),
		QuarantineDuration:       s.closingConfig.QuarantineDuration,
		MisbehaviorFine:          s.closingConfig.MisbehaviorFine.Nano().Bytes(),
		ConditionalCloseDuration: s.closingConfig.ConditionalCloseDuration,
	}
}

func (s *Service) GetChannelsWithNode(ctx context.Context, key ed25519.PublicKey) ([]*db.Channel, error) {
	return s.db.GetChannelsWithKey(ctx, key)
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
					err = s.db.CreateTask(context.Background(), "exchange-states", channel.Address,
						"exchange-states-"+channel.Address+"-"+fmt.Sprint(channel.InitAt.Unix()),
						db.ChannelTask{Address: channel.Address}, &delay, nil,
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
						err = s.db.CreateTask(context.Background(), "challenge", channel.Address+"-chain",
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

				log.Info().Str("address", channel.Address).
					Hex("with", channel.TheirOnchain.Key).
					Msg("onchain channel uncooperative closing event, settling conditions")

				settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+3), 0)
				finishAt := settleAt.Add(time.Duration(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration) * time.Second)

				err = s.db.CreateTask(context.Background(), "settle", channel.Address+"-settle",
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

				err = s.db.CreateTask(context.Background(), "finalize", channel.Address+"-finalize",
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
		}
	}
}

func (s *Service) DebugPrintVirtualChannels() {
	chs, err := s.db.GetActiveChannels(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("failed to get active channels")
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
			Msg("active onchain channel")
		for _, kv := range ch.Our.State.Data.Conditionals.All() {
			vch, _ := payments.ParseVirtualChannelCond(kv.Value)
			log.Info().
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("till_deadline", time.Unix(vch.Deadline, 0).Sub(time.Now()).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Hex("key", vch.Key).
				Msg("virtual from us")
		}
		for _, kv := range ch.Their.State.Data.Conditionals.All() {
			vch, _ := payments.ParseVirtualChannelCond(kv.Value)

			log.Info().
				Str("capacity", tlb.FromNanoTON(vch.Capacity).String()).
				Str("till_deadline", time.Unix(vch.Deadline, 0).Sub(time.Now()).String()).
				Str("fee", tlb.FromNanoTON(vch.Fee).String()).
				Hex("key", vch.Key).
				Msg("virtual to us")
		}
	}
}

func (s *Service) GetActiveChannel(channelAddr string) (*db.Channel, error) {
	channel, err := s.getVerifiedChannel(channelAddr)
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

func (s *Service) getVerifiedChannel(channelAddr string) (*db.Channel, error) {
	channel, err := s.db.GetChannel(context.Background(), channelAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	if err = channel.Our.Verify(s.key.Public().(ed25519.PublicKey)); err != nil {
		log.Warn().Msg(channel.Our.State.Dump())
		return nil, fmt.Errorf("looks like our db state was tampered: %w", err)
	}
	if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
		log.Warn().Msg(channel.Their.State.Dump())
		return nil, fmt.Errorf("looks like their db state was tampered: %w", err)
	}
	return channel, nil
}

func (s *Service) requestAction(ctx context.Context, channelAddress string, action any) error {
	channel, err := s.getVerifiedChannel(channelAddress)
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
func (s *Service) proposeAction(ctx context.Context, channelAddress string, action transport.Action, details any) error {
	channel, err := s.getVerifiedChannel(channelAddress)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err := s.updateOurStateWithAction(channel, action, details); err != nil {
		return fmt.Errorf("failed to prepare actions for the next node - %w: %v", ErrNotPossible, err)
	}

	stateCell, err := tlb.ToCell(channel.Our.SignedSemiChannel)
	if err != nil {
		return fmt.Errorf("failed to serialize state: %w", err)
	}

	res, err := s.transport.ProposeAction(ctx, address.MustParseAddr(channel.Address), channel.TheirOnchain.Key, stateCell, action)
	if err != nil {
		return fmt.Errorf("failed to propose actions: %w", err)
	}

	if !res.Agreed {
		log.Warn().Str("reason", res.Reason).Msg("actions request denied")
		return ErrDenied
	}

	var theirState payments.SignedSemiChannel
	if err := tlb.LoadFromCell(&theirState, res.SignedState.BeginParse()); err != nil {
		return fmt.Errorf("failed to parse their updated channel state: %w", err)
	}

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

	return nil
}

func (s *Service) verifyChannel(p *payments.AsyncChannel) (ok bool, isLeft bool) {
	isLeft = bytes.Equal(p.Storage.KeyA, s.key.Public().(ed25519.PublicKey))

	if !isLeft && !bytes.Equal(p.Storage.KeyB, s.key.Public().(ed25519.PublicKey)) {
		return false, false
	}

	if !isLeft && p.Storage.Payments.DestB.String() != s.wallet.Address().String() {
		return false, false
	}

	if isLeft && p.Storage.Payments.DestA.String() != s.wallet.Address().String() {
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

func (s *Service) incrementStates(ctx context.Context, channelAddr string, wantResponse bool) error {
	if err := s.proposeAction(ctx, channelAddr, transport.IncrementStatesAction{WantResponse: wantResponse}, nil); err != nil {
		return fmt.Errorf("failed to propose action to the party: %w", err)
	}
	return nil
}
