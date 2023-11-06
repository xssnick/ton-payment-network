package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/internal/node/transport"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"sync"
	"time"
)

var ErrDenied = errors.New("actions denied")

type Transport interface {
	GetChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey) (*transport.ChannelConfig, error)
	RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action transport.Action) (*transport.Decision, error)
	ProposeAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, state *cell.Cell, action transport.Action) (*transport.Decision, error)
	RequestInboundChannel(ctx context.Context, capacity *big.Int, ourWallet *address.Address, ourKey, theirKey []byte) (*transport.Decision, error)
}

type DB interface {
	CreateTask(id, typ string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask() (*db.Task, error)
	RetryTask(id string, reason string, retryAt time.Time) error
	CompleteTask(id string) error

	SetBlockOffset(seqno uint32) error
	GetBlockOffset() (*db.BlockOffset, error)

	GetActiveChannels() ([]*db.Channel, error)
	GetActiveChannelsWithKey(key ed25519.PublicKey) ([]*db.Channel, error)
	CreateChannel(channel *db.Channel) error
	GetChannel(addr string) (*db.Channel, error)
	UpdateChannel(channel *db.Channel) error
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

	maxOutboundCapacity     tlb.Coins
	virtualChannelFee       tlb.Coins
	excessFee               tlb.Coins
	wallet                  *wallet.Wallet
	contractMaker           *payments.Client
	closingConfig           payments.ClosingConfig
	deadlineDecreaseNextHop int64
	channelsLimit           int

	// TODO: channel based lock
	mx sync.Mutex
}

func NewService(api ton.APIClientWrapped, db DB, transport Transport, wallet *wallet.Wallet, updates chan any, key ed25519.PrivateKey, deadlineDecreaseNextHop time.Duration, closingConfig payments.ClosingConfig) *Service {
	return &Service{
		ton:                     api,
		transport:               transport,
		updates:                 updates,
		db:                      db,
		key:                     key,
		maxOutboundCapacity:     tlb.MustFromTON("5.0"),
		virtualChannelFee:       tlb.MustFromTON("0.01"),
		excessFee:               tlb.MustFromTON("0.01"),
		wallet:                  wallet,
		contractMaker:           payments.NewPaymentChannelClient(api),
		closingConfig:           closingConfig,
		deadlineDecreaseNextHop: int64(deadlineDecreaseNextHop / time.Second),
		channelsLimit:           3000,
	}
}

func (s *Service) GetChannelConfig() transport.ChannelConfig {
	return transport.ChannelConfig{
		ExcessFee:                s.excessFee.Nano().Bytes(),
		WalletAddr:               s.wallet.Address().Data(),
		QuarantineDuration:       s.closingConfig.QuarantineDuration,
		MisbehaviorFine:          s.closingConfig.MisbehaviorFine.Nano().Bytes(),
		ConditionalCloseDuration: s.closingConfig.ConditionalCloseDuration,
	}
}

func (s *Service) Start() {
	go s.peerDiscovery()
	go s.taskExecutor()

	for update := range s.updates {
		switch upd := update.(type) {
		case BlockCheckedEvent:
			if err := s.db.SetBlockOffset(upd.Seqno); err != nil {
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
				channel, err = s.db.GetChannel(upd.Channel.Address().String())
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
				if upd.Channel.Status != payments.ChannelStatusOpen {
					// TODO: think about possible cases
					continue
				}

				channel = &db.Channel{
					ID:           upd.Channel.Storage.ChannelID,
					Address:      upd.Channel.Address().String(),
					WeLeft:       isLeft,
					OurOnchain:   our,
					TheirOnchain: their,
					Our:          db.NewSide(upd.Channel.Storage.ChannelID, uint64(our.CommittedSeqno)),
					Their:        db.NewSide(upd.Channel.Storage.ChannelID, uint64(their.CommittedSeqno)),
					InitAt:       time.Unix(int64(upd.Transaction.Now), 0),
					UpdatedAt:    time.Unix(int64(upd.Transaction.Now), 0),
					CreatedAt:    time.Now(),
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
					err = s.db.CreateTask("exchange-states",
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
				if (isLeft && !upd.Channel.Storage.Quarantine.StateCommittedByA) ||
					(!isLeft && upd.Channel.Storage.Quarantine.StateCommittedByA) {
					// if committed not by us, check state
					qOur, qTheir := upd.Channel.Storage.Quarantine.StateB, upd.Channel.Storage.Quarantine.StateA
					if isLeft {
						qOur, qTheir = qTheir, qOur
					}

					// TODO: maybe check only their?
					if qOur.Seqno < channel.Our.State.Data.Seqno ||
						qTheir.Seqno < channel.Their.State.Data.Seqno {
						// something is outdated, challenge state
						settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+1), 0)
						err = s.db.CreateTask("challenge",
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
				log.Info().Str("address", channel.Address).
					Hex("with", channel.TheirOnchain.Key).
					Msg("onchain channel uncooperative closing event")

				channel.Status = db.ChannelStateClosing
				settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+3), 0)
				finishAt := settleAt.Add(time.Duration(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration) * time.Second)

				err = s.db.CreateTask("settle",
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
				settleAt := time.Unix(int64(upd.Channel.Storage.Quarantine.QuarantineStarts+upd.Channel.Storage.ClosingConfig.QuarantineDuration+5), 0)
				finishAt := settleAt.Add(time.Duration(upd.Channel.Storage.ClosingConfig.ConditionalCloseDuration+5) * time.Second)
				err = s.db.CreateTask("finalize",
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
				if err = fc(channel); err != nil {
					log.Error().Err(err).Msg("failed to set channel in db, retrying...")
					time.Sleep(1 * time.Second)
					continue
				}
				break
			}
		}
	}
}

func (s *Service) DebugPrintVirtualChannels() {
	chs, err := s.db.GetActiveChannels()
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
		return nil, fmt.Errorf("channel is not active")
	}

	if !channel.Our.IsReady() || !channel.Their.IsReady() {
		return nil, fmt.Errorf("states not exchanged yet")
	}

	return channel, nil
}

func (s *Service) getVerifiedChannel(channelAddr string) (*db.Channel, error) {
	channel, err := s.db.GetChannel(channelAddr)
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

func (s *Service) requestAction(ctx context.Context, channel *db.Channel, action any) error {
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

func (s *Service) proposeAction(ctx context.Context, channel *db.Channel, action transport.Action, details any) error {
	ourNextState, rollback, err := s.updateOurStateWithAction(channel, action, details)
	if err != nil {
		if errors.Is(err, ErrAlreadyApplied) {
			return nil
		}
		return fmt.Errorf("failed to prepare actions for the next node: %w", err)
	}

	success := false
	defer func() {
		if !success {
			// we rollback changes in case of they are not accepted by party
			if err = rollback(); err != nil {
				log.Error().Err(err).Msg("failed to rollback unaccepted proposal")
			}
		}
	}()

	stateCell, err := tlb.ToCell(ourNextState)
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
	// TODO: better rollback with signed confirmation from party
	success = true
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
