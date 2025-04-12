package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	db "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

var ErrNoResolveExists = errors.New("cannot close channel without known state")

func (s *Service) GetChannel(ctx context.Context, addr string) (*db.Channel, error) {
	channel, err := s.db.GetChannel(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	return channel, nil
}

func (s *Service) ListChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error) {
	channels, err := s.db.GetChannels(ctx, key, status)
	if err != nil {
		return nil, fmt.Errorf("failed to get channels: %w", err)
	}
	return channels, nil
}

var ErrNotWhitelisted = errors.New("not whitelisted")

func (s *Service) ResolveCoinConfig(jetton string, ecID uint32, onlyEnabled bool) (*config.CoinConfig, error) {
	if jetton != "" && ecID != 0 {
		return nil, fmt.Errorf("jetton and ec cannot be used together")
	}

	var ok bool
	var cc config.CoinConfig
	if jetton != "" {
		cc, ok = s.supportedJettons[jetton]
		if !ok || (!cc.Enabled && onlyEnabled) {
			return nil, ErrNotWhitelisted
		}
	} else if ecID > 0 {
		cc, ok = s.supportedEC[ecID]
		if !ok || (!cc.Enabled && onlyEnabled) {
			return nil, ErrNotWhitelisted
		}
	} else {
		if !s.supportedTon || (!s.cfg.SupportedCoins.Ton.Enabled && onlyEnabled) {
			return nil, ErrNotWhitelisted
		}
		cc = s.cfg.SupportedCoins.Ton
	}

	return &cc, nil
}

func (s *Service) GetTunnelingFees(ctx context.Context, jetton string, ecID uint32) (enabled bool, minFee, maxCap tlb.Coins, percentFee float64, err error) {
	cc, err := s.ResolveCoinConfig(jetton, ecID, true)
	if err != nil {
		if errors.Is(err, ErrNotWhitelisted) {
			return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, nil
		}
		return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, fmt.Errorf("failed to resolve coin config: %w", err)
	}

	if !cc.VirtualTunnelConfig.AllowTunneling {
		return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, nil
	}

	return true, tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMinFee, int(cc.Decimals)), tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMaxCapacity, int(cc.Decimals)), cc.VirtualTunnelConfig.ProxyFeePercent, nil
}

func (s *Service) DeployChannelWithNode(ctx context.Context, nodeKey ed25519.PublicKey, jettonMaster *address.Address, ecID uint32) (*address.Address, error) {
	log.Info().Msg("locating node and proposing channel config...")

	channelId := make([]byte, 16)
	copy(channelId, nodeKey[:15])

	channels, err := s.db.GetChannels(ctx, nodeKey, db.ChannelStateAny)
	if err != nil {
		return nil, fmt.Errorf("failed to get channels: %w", err)
	}

	used := make([][]byte, 0, len(channels))
	for _, ch := range channels {
		if ch.Status != db.ChannelStateInactive {
			used = append(used, ch.ID)
		}
	}

	// find free channel id
	found := false
	for i := 0; i < 256; i++ {
		exists := false
		for _, chId := range used {
			if bytes.Equal(chId, channelId) {
				exists = true
				break
			}
		}

		if !exists {
			found = true
			break
		}
		channelId[15]++
	}

	if !found {
		return nil, fmt.Errorf("too many channels are already open")
	}

	var jettonAddr string
	if jettonMaster != nil {
		jettonAddr = jettonMaster.Bounce(true).String()
	}

	cc, err := s.ResolveCoinConfig(jettonAddr, ecID, true)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve coin config: %w", err)
	}

	var jettonData []byte
	if jettonMaster != nil {
		jettonData = jettonMaster.Data()
	}

	excessFee := tlb.MustFromTON(cc.ExcessFeeTon)
	destWallet, err := s.transport.ProposeChannelConfig(ctx, nodeKey, transport.ProposeChannelConfig{
		JettonAddr:               jettonData,
		ExtraCurrencyID:          ecID,
		ExcessFee:                excessFee.Nano().Bytes(),
		QuarantineDuration:       s.cfg.QuarantineDurationSec,
		MisbehaviorFine:          tlb.MustFromTON(cc.MisbehaviorFine).Nano().Bytes(),
		ConditionalCloseDuration: s.cfg.ConditionalCloseDurationSec,
	})
	if err != nil {
		return nil, fmt.Errorf("channel proposal failed: %w", err)
	}

	pc := payments.PaymentConfig{
		StorageFee:     tlb.MustFromTON(cc.ExcessFeeTon),
		DestA:          s.wallet.WalletAddress(),
		DestB:          destWallet,
		CurrencyConfig: payments.CurrencyConfigTon{},
	}

	if jettonMaster != nil {
		pc.CurrencyConfig = payments.CurrencyConfigJetton{
			Info: payments.CurrencyConfigJettonInfo{
				Master: jettonMaster,
				Wallet: nil,
			},
		}
	} else if ecID > 0 {
		pc.CurrencyConfig = payments.CurrencyConfigEC{
			ID: ecID,
		}
	}

	log.Info().Msg("starting channel deploy...")

	body, code, data, err := s.contractMaker.GetDeployAsyncChannelParams(channelId, true, s.key, nodeKey, payments.ClosingConfig{
		QuarantineDuration:       s.cfg.QuarantineDurationSec,
		MisbehaviorFine:          tlb.MustFromDecimal(cc.MisbehaviorFine, int(cc.Decimals)),
		ConditionalCloseDuration: s.cfg.ConditionalCloseDurationSec,
	}, pc)
	if err != nil {
		return nil, fmt.Errorf("failed to get deploy params: %w", err)
	}

	state := &tlb.StateInit{
		Data: code,
		Code: data,
	}
	accAddr := state.CalcAddress(0)

	if err = s.AddUrgentPeer(ctx, nodeKey); err != nil {
		return nil, fmt.Errorf("failed to add urgent peer: %w", err)
	}

	if s.discoverChannel(state.CalcAddress(0)) {
		return accAddr, nil
	}

	fee := new(big.Int).Add(excessFee.Nano(), tlb.MustFromTON("0.25").Nano())
	addr, tx, _, err := s.wallet.DeployContractWaitTransaction(ctx, tlb.FromNanoTON(fee), body, code, data)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy: %w", err)
	}

	log.Info().Str("addr", addr.String()).Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("contract deployed")

	for !s.discoverChannel(addr) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			log.Info().Str("addr", addr.String()).Msg("trying to discover channel onchain...")
		}
	}

	log.Info().Str("addr", addr.String()).Msg("channel discovered")

	return addr, nil
}

func (s *Service) OpenVirtualChannel(ctx context.Context, with, instructionKey, finalDest ed25519.PublicKey, private ed25519.PrivateKey, chain []transport.OpenVirtualInstruction, vch payments.VirtualChannel, jettonMaster *address.Address, ecID uint32) error {
	if len(chain) == 0 {
		return fmt.Errorf("chain is empty")
	}

	channels, err := s.db.GetChannels(ctx, with, db.ChannelStateActive)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	needAmount := new(big.Int).Add(vch.Fee, vch.Capacity)
	var channel *db.Channel
	for _, ch := range channels {
		if ch.ExtraCurrencyID != ecID {
			continue
		}
		if jettonMaster == nil && ch.JettonAddress != "" {
			continue
		}
		if jettonMaster != nil && ch.JettonAddress != jettonMaster.Bounce(true).String() {
			continue
		}

		balance, err := ch.CalcBalance(false)
		if err != nil {
			return fmt.Errorf("failed to calc channel balance: %w", err)
		}

		if balance.Cmp(needAmount) != -1 {
			// we found channel with enough balance
			channel = ch
			break
		}
	}

	if channel == nil {
		return fmt.Errorf("failed to open virtual channel, %w: no active channel with enough balance exists", ErrNotPossible)
	}

	if safe := vch.Deadline - (time.Now().Unix() + channel.SafeOnchainClosePeriod); safe < int64(s.cfg.MinSafeVirtualChannelTimeoutSec) {
		return fmt.Errorf("safe deadline is less than acceptable: %d, %d", safe, s.cfg.MinSafeVirtualChannelTimeoutSec)
	}

	act := transport.OpenVirtualAction{
		ChannelKey:     vch.Key,
		InstructionKey: instructionKey,
	}

	if err = act.SetInstructions(chain, private); err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	tryTill := time.Unix(vch.Deadline-channel.SafeOnchainClosePeriod, 0)
	err = s.db.CreateTask(ctx, PaymentsTaskPool, "open-virtual", channel.Address,
		"open-virtual-"+base64.StdEncoding.EncodeToString(vch.Key),
		db.OpenVirtualTask{
			FinalDestinationKey: finalDest,
			ChannelAddress:      channel.Address,
			VirtualKey:          vch.Key,
			Deadline:            vch.Deadline,
			Fee:                 vch.Fee.String(),
			Capacity:            vch.Capacity.String(),
			Action:              act,
		}, nil, &tryTill,
	)
	if err != nil {
		return fmt.Errorf("failed to create open task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) AddVirtualChannelResolve(ctx context.Context, virtualKey ed25519.PublicKey, state payments.VirtualChannelState) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	if meta.Incoming != nil {
		ch, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("onchain channel with source not exists")
			}
			return fmt.Errorf("failed to load channel: %w", err)
		}

		_, vch, err := payments.FindVirtualChannel(ch.Their.Conditionals, virtualKey)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return nil
			}

			log.Error().Err(err).Str("channel", ch.Address).Msg("failed to find virtual channel")
			return fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if vch.Deadline < time.Now().Unix() {
			return fmt.Errorf("virtual channel has expired")
		}

		if state.Amount.Nano().Cmp(vch.Capacity) == 1 {
			return fmt.Errorf("amount cannot be > capacity")
		}
	} else {
		// in case we are the final point, check against our channel
		ch, err := s.db.GetChannel(ctx, meta.Outgoing.ChannelAddress)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("onchain channel with target not exists")
			}
			return fmt.Errorf("failed to load channel: %w", err)
		}

		_, vch, err := payments.FindVirtualChannel(ch.Our.Conditionals, virtualKey)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return nil
			}

			log.Error().Err(err).Str("channel", ch.Address).Msg("failed to find virtual channel")
			return fmt.Errorf("failed to find virtual channel: %w", err)
		}

		if vch.Deadline < time.Now().Unix() {
			return fmt.Errorf("virtual channel has expired")
		}

		if state.Amount.Nano().Cmp(vch.Capacity) == 1 {
			return fmt.Errorf("amount cannot be > capacity")
		}
	}

	// TODO: maybe allow in want state, but need to check concurrency cases
	if meta.Status != db.VirtualChannelStateActive {
		return fmt.Errorf("virtual channel is inactive")
	}

	if err = meta.AddKnownResolve(meta.Key, &state); err != nil {
		return fmt.Errorf("failed to add channel condition resolve: %w", err)
	}

	meta.UpdatedAt = time.Now()
	if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	return nil
}

func (s *Service) RequestUncooperativeClose(ctx context.Context, addr string) error {
	channel, err := s.GetActiveChannel(ctx, addr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", channel.Address+"-uncoop",
		"uncooperative-close-"+channel.Address+"-"+fmt.Sprint(channel.InitAt.Unix()),
		db.ChannelUncooperativeCloseTask{
			Address: channel.Address,
		}, nil, nil,
	); err != nil {
		return err
	}
	return nil
}

func (s *Service) TopupChannel(ctx context.Context, addr *address.Address, amount tlb.Coins) error {
	channel, err := s.GetActiveChannel(ctx, addr.Bounce(true).String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "topup", channel.Address+"-topup",
		"topup-"+channel.Address+"-"+fmt.Sprint(time.Now().Unix()),
		db.TopupTask{
			Address:            channel.Address,
			AmountNano:         amount.Nano().String(),
			ChannelInitiatedAt: channel.InitAt,
		}, nil, nil,
	); err != nil {
		return err
	}
	log.Info().Str("address", addr.String()).Str("amount", amount.String()).Msg("topup task registered")
	return nil
}

func (s *Service) RequestWithdraw(ctx context.Context, addr *address.Address, amount tlb.Coins) error {
	channel, err := s.GetActiveChannel(ctx, addr.Bounce(true).String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	return s.requestWithdraw(ctx, channel, amount)
}

func (s *Service) requestWithdraw(ctx context.Context, channel *db.Channel, amount tlb.Coins) error {
	if _, _, _, err := s.getCommitRequest(amount, tlb.MustFromTON("0"), channel); err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	if err := s.db.CreateTask(ctx, PaymentsTaskPool, "withdraw", channel.Address+"-withdraw",
		"withdraw-"+channel.Address+"-"+fmt.Sprint(channel.InitAt.Unix())+fmt.Sprintf("-%d-%d", channel.Their.State.Data.Seqno, channel.Our.State.Data.Seqno),
		db.WithdrawTask{
			Address:            channel.Address,
			AmountNano:         amount.Nano().String(),
			ChannelInitiatedAt: channel.InitAt,
		}, nil, nil,
	); err != nil {
		return err
	}
	log.Info().Str("address", channel.Address).Str("amount", amount.String()).Msg("withdraw task registered")
	return nil
}

var ErrCannotCloseOngoingVirtual = fmt.Errorf("cannot close outgoing channel")

func (s *Service) CloseVirtualChannel(ctx context.Context, virtualKey ed25519.PublicKey) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	if meta.Incoming == nil {
		return ErrCannotCloseOngoingVirtual
	}

	ch, err := s.GetActiveChannel(ctx, meta.Incoming.ChannelAddress)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("onchain channel with source is not active")
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}

	_, vch, err := payments.FindVirtualChannel(ch.Their.Conditionals, virtualKey)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			// idempotency
			return nil
		}

		log.Error().Err(err).Str("channel", ch.Address).Msg("failed to find virtual channel")
		return fmt.Errorf("failed to find virtual channel: %w", err)
	}

	state := meta.GetKnownResolve(vch.Key)
	if state == nil {
		return ErrNoResolveExists
	}

	meta.Status = db.VirtualChannelStateWantClose
	meta.UpdatedAt = time.Now()
	if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	// We start uncooperative close at specific moment to have time
	// to commit resolve onchain in case partner is irresponsible.
	// But in the same time we give our partner time to
	uncooperativeAfter := time.Unix(vch.Deadline-ch.SafeOnchainClosePeriod, 0)
	minDelay := time.Now().Add(1 * time.Minute)
	if !uncooperativeAfter.After(minDelay) {
		uncooperativeAfter = minDelay
	}

	// Creating aggressive onchain close task, for the future,
	// in case we will not be able to communicate with party
	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", ch.Address+"-uncoop",
		"uncooperative-close-"+ch.Address+"-vc-"+base64.StdEncoding.EncodeToString(vch.Key),
		db.ChannelUncooperativeCloseTask{
			Address:                 ch.Address,
			CheckVirtualStillExists: vch.Key,
		}, &uncooperativeAfter, nil,
	); err != nil {
		log.Warn().Err(err).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to create uncooperative close task")
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-close-virtual", ch.Address+"-coop",
		"virtual-close-"+ch.Address+"-vc-"+base64.StdEncoding.EncodeToString(vch.Key),
		db.AskCloseVirtualTask{
			Key:            vch.Key,
			ChannelAddress: ch.Address,
		}, nil, &uncooperativeAfter,
	); err != nil {
		log.Warn().Err(err).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to create cooperative close task")
	}

	log.Info().Err(err).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("virtual channel close task created and will be executed soon")

	return nil
}

func (s *Service) executeCooperativeCommit(ctx context.Context, req *payments.CooperativeCommit, ch *db.Channel) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(ch.Address), tlb.MustFromTON("0.16"), msg))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("cooperative commit transaction completed")
	return nil
}

func (s *Service) executeCooperativeClose(ctx context.Context, req *payments.CooperativeClose, channel *db.Channel) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msg))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("cooperative close transaction completed")

	return nil
}

func (s *Service) RequestCooperativeClose(ctx context.Context, channelAddr string) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	ch, err := s.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if _, _, _, err = s.getCooperativeCloseRequest(ch); err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	return s.db.Transaction(ctx, func(ctx context.Context) error {
		ch.AcceptingActions = false
		if err = s.db.UpdateChannel(ctx, ch); err != nil {
			return fmt.Errorf("failed to update channel: %w", err)
		}

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "cooperative-close", ch.Address,
			"cooperative-close-"+ch.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelCooperativeCloseTask{
				Address:            ch.Address,
				ChannelInitiatedAt: ch.InitAt,
			}, nil, nil,
		); err != nil {
			return fmt.Errorf("failed to create cooperative close task: %w", err)
		}

		after := time.Now().Add(5 * time.Minute)
		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", ch.Address+"-uncoop",
			"uncooperative-close-"+ch.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelUncooperativeCloseTask{
				Address:            ch.Address,
				ChannelInitiatedAt: &ch.InitAt,
			}, &after, nil,
		); err != nil {
			log.Error().Err(err).Str("channel", ch.Address).Msg("failed to create uncooperative close task")
		}
		return nil
	})
}

func (s *Service) getCommitRequest(ourWithdraw, theirWithdraw tlb.Coins, channel *db.Channel) (*payments.CooperativeCommit, *cell.Cell, []byte, error) {
	if channel.Our.PendingWithdraw.Sign() != 0 {
		return nil, nil, nil, fmt.Errorf("our withdraw is not zero")
	}
	if channel.Their.PendingWithdraw.Sign() != 0 {
		return nil, nil, nil, fmt.Errorf("their withdraw is not zero")
	}

	// this is not locked balance
	ourBalance, err := channel.CalcBalance(false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
	}

	theirBalance, err := channel.CalcBalance(true)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc their balance: %w", err)
	}

	if ourWithdraw.Nano().Cmp(ourBalance) > 0 {
		return nil, nil, nil, fmt.Errorf("our withdraw is greater than balance")
	}
	if theirWithdraw.Nano().Cmp(theirBalance) > 0 {
		return nil, nil, nil, fmt.Errorf("their withdraw is greater than balance")
	}

	var ourReq payments.CooperativeCommit
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.BalanceA = tlb.FromNanoTON(ourBalance)
	ourReq.Signed.BalanceB = tlb.FromNanoTON(theirBalance)
	ourReq.Signed.SeqnoA = channel.Our.State.Data.Seqno + 1
	ourReq.Signed.SeqnoB = channel.Their.State.Data.Seqno + 1
	ourReq.Signed.WithdrawA = ourWithdraw
	ourReq.Signed.WithdrawB = theirWithdraw
	if !channel.WeLeft {
		ourReq.Signed.WithdrawA, ourReq.Signed.WithdrawB = ourReq.Signed.WithdrawB, ourReq.Signed.WithdrawA
		ourReq.Signed.BalanceA, ourReq.Signed.BalanceB = ourReq.Signed.BalanceB, ourReq.Signed.BalanceA
		ourReq.Signed.SeqnoA, ourReq.Signed.SeqnoB = ourReq.Signed.SeqnoB, ourReq.Signed.SeqnoA
	}
	dataCell, err := tlb.ToCell(ourReq.Signed)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize body to cell: %w", err)
	}

	signature := dataCell.Sign(s.key)
	ourReq.SignatureA.Value = signature
	ourReq.SignatureB.Value = make([]byte, 64)

	if !channel.WeLeft {
		ourReq.SignatureA.Value, ourReq.SignatureB.Value = ourReq.SignatureB.Value, ourReq.SignatureA.Value
	}

	return &ourReq, dataCell, signature, nil
}

func (s *Service) getCooperativeCloseRequest(channel *db.Channel) (*payments.CooperativeClose, *cell.Cell, []byte, error) {
	allOur, err := channel.Our.Conditionals.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load our cond dict: %w", err)
	}

	for _, kv := range allOur {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.Deadline >= time.Now().Unix() {
			return nil, nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	allTheir, err := channel.Their.Conditionals.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load their cond dict: %w", err)
	}

	for _, kv := range allTheir {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.Deadline >= time.Now().Unix() {
			return nil, nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	ourBalance, err := channel.CalcBalance(false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
	}

	theirBalance, err := channel.CalcBalance(true)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc their balance: %w", err)
	}

	var ourReq payments.CooperativeClose
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.BalanceA = tlb.FromNanoTON(ourBalance)
	ourReq.Signed.BalanceB = tlb.FromNanoTON(theirBalance)
	ourReq.Signed.SeqnoA = channel.Our.State.Data.Seqno + 1
	ourReq.Signed.SeqnoB = channel.Their.State.Data.Seqno + 1
	if !channel.WeLeft {
		ourReq.Signed.BalanceA, ourReq.Signed.BalanceB = ourReq.Signed.BalanceB, ourReq.Signed.BalanceA
		ourReq.Signed.SeqnoA, ourReq.Signed.SeqnoB = ourReq.Signed.SeqnoB, ourReq.Signed.SeqnoA
	}
	dataCell, err := tlb.ToCell(ourReq.Signed)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize body to cell: %w", err)
	}

	signature := dataCell.Sign(s.key)
	ourReq.SignatureA.Value = signature
	ourReq.SignatureB.Value = make([]byte, 64)

	if !channel.WeLeft {
		ourReq.SignatureA.Value, ourReq.SignatureB.Value = ourReq.SignatureB.Value, ourReq.SignatureA.Value
	}

	return &ourReq, dataCell, signature, nil
}

func (s *Service) startUncooperativeClose(ctx context.Context, channelAddr string) error {
	channel, err := s.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	channel.AcceptingActions = false
	if err = s.db.UpdateChannel(ctx, channel); err != nil {
		return fmt.Errorf("failed to update channel: %w", err)
	}

	block, err := s.ton.CurrentMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get master block: %w", err)
	}

	och, err := payments.NewPaymentChannelClient(s.ton).GetAsyncChannel(ctx, block, address.MustParseAddr(channelAddr), true)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status != payments.ChannelStatusOpen {
		log.Debug().Str("address", channel.Address).
			Msg("uncooperative close already started or not required")
		return nil
	}

	log.Info().Str("address", channel.Address).
		Msg("starting uncooperative close")

	msg := payments.StartUncooperativeClose{
		IsSignedByA: channel.WeLeft,
	}
	msg.Signed.A = channel.Our.SignedSemiChannel
	msg.Signed.B = channel.Their.SignedSemiChannel
	if !channel.WeLeft {
		msg.Signed.A, msg.Signed.B = msg.Signed.B, msg.Signed.A
	}
	msg.Signed.ChannelID = channel.ID

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	msgCell, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("uncooperative close transaction completed")

	return nil
}

func (s *Service) challengeChannelState(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	block, err := s.ton.CurrentMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get master block: %w", err)
	}

	och, err := payments.NewPaymentChannelClient(s.ton).GetAsyncChannel(ctx, block, address.MustParseAddr(channelAddr), true)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status == payments.ChannelStatusAwaitingFinalization ||
		och.Status == payments.ChannelStatusUninitialized ||
		och.Status == payments.ChannelStatusSettlingConditionals {
		// no more time to challenge
		return nil
	}

	msg := payments.ChallengeQuarantinedState{
		IsChallengedByA: channel.WeLeft,
	}
	msg.Signed.A = channel.Our.SignedSemiChannel
	msg.Signed.B = channel.Their.SignedSemiChannel
	if !channel.WeLeft {
		msg.Signed.A, msg.Signed.B = msg.Signed.B, msg.Signed.A
	}
	msg.Signed.ChannelID = channel.ID

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	msgCell, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("challenge channel state transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) finishUncooperativeChannelClose(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	block, err := s.ton.CurrentMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get master block: %w", err)
	}

	och, err := payments.NewPaymentChannelClient(s.ton).GetAsyncChannel(ctx, block, address.MustParseAddr(channelAddr), true)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status == payments.ChannelStatusUninitialized {
		// already closed
		return nil
	}

	msgCell, err := tlb.ToCell(payments.FinishUncooperativeClose{})
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("finish uncooperative close transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) settleChannelConditionals(ctx context.Context, channelAddr string) error {
	const messagesPerTransaction = 20
	const conditionsPerMessage = 30

	log.Info().Str("address", channelAddr).Msg("settling conditionals")

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Their.Conditionals.IsEmpty() {
		log.Info().Str("address", channel.Address).
			Msg("nothing to settle, empty their conditionals")
		return nil
	}

	block, err := s.ton.CurrentMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get master block: %w", err)
	}

	och, err := payments.NewPaymentChannelClient(s.ton).GetAsyncChannel(ctx, block, address.MustParseAddr(channelAddr), true)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status == payments.ChannelStatusAwaitingFinalization ||
		och.Status == payments.ChannelStatusUninitialized {
		// no more time to settle
		return nil
	}

	msg := payments.SettleConditionals{
		IsFromA: channel.WeLeft,
	}
	msg.Signed.ChannelID = channel.ID
	msg.Signed.ConditionalsToSettle = cell.NewDict(32)

	// TODO: get all conditions and make inputs for known
	all, err := channel.Their.Conditionals.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load their conditions dict: %w", err)
	}

	var messages []*cell.Cell
	var resolved int

	addMessage := func(data payments.SettleConditionals, proofPath *cell.ProofSkeleton, num int) error {
		dictProof, err := channel.Their.Conditionals.AsCell().CreateProof(proofPath)
		if err != nil {
			log.Warn().Err(err).Msg("failed to find proof path for virtual channel")
			return err
		}

		data.Signed.ConditionalsProof = dictProof

		dataCell, err := tlb.ToCell(data.Signed)
		if err != nil {
			return fmt.Errorf("failed to serialize body to cell: %w", err)
		}
		data.Signature.Value = dataCell.Sign(s.key)

		msgCell, err := tlb.ToCell(data)
		if err != nil {
			return fmt.Errorf("failed to serialize message to cell: %w", err)
		}

		messages = append(messages, msgCell)
		return nil
	}

	updatedState := channel.Their.Conditionals.Copy()

	condNum := 0
	proofPath := cell.CreateProofSkeleton()
	for _, kv := range all {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			log.Warn().Err(err).Msg("failed to parse virtual channel")
			continue
		}

		meta, err := s.db.GetVirtualChannelMeta(ctx, vch.Key)
		if err != nil {
			log.Warn().Err(err).Msg("failed to get virtual channel meta")
			continue
		}

		if resolve := meta.GetKnownResolve(vch.Key); resolve != nil {
			rc, err := tlb.ToCell(resolve)
			if err != nil {
				log.Warn().Err(err).Msg("failed to serialize known virtual channel state")
				continue
			}

			if err = msg.Signed.ConditionalsToSettle.Set(kv.Key.MustToCell(), rc); err != nil {
				log.Warn().Err(err).Msg("failed to store known virtual channel state in request")
				continue
			}

			_, sk, err := channel.Their.Conditionals.LoadValueWithProof(kv.Key.MustToCell(), proofPath)
			if err != nil {
				log.Warn().Err(err).Msg("failed to find proof path for virtual channel")
				continue
			}
			sk.SetRecursive() // we need full value in proof
			condNum++

			// replace value to empty cell, we need 2 dictionaries: before and after, to save and continue
			if err = updatedState.Set(kv.Key.MustToCell(), cell.BeginCell().EndCell()); err != nil {
				log.Warn().Err(err).Msg("failed to replace virtual channel in conditionals")
				continue
			}

			if condNum == conditionsPerMessage {
				if err := addMessage(msg, proofPath, condNum); err != nil {
					log.Warn().Err(err).Msg("failed to add settle message")
					return err
				}

				condNum = 0
				proofPath = cell.CreateProofSkeleton()
				msg.Signed.ConditionalsToSettle = cell.NewDict(32)
				channel.Their.Conditionals = updatedState.Copy()
			}
			resolved++
		}
	}

	if condNum%conditionsPerMessage != 0 {
		if err := addMessage(msg, proofPath, condNum); err != nil {
			log.Warn().Err(err).Msg("failed to add settle last message")
			return err
		}
	}

	// TODO: maybe wait for some deadline if not all states resolved, before settle
	if len(messages) == 0 {
		log.Warn().Msg("no known resolves for existing conditions")
		return nil
	}

	if resolved != len(all) {
		log.Warn().
			Int("with_resolves", resolved).
			Int("all", len(all)).
			Msg("not all conditions has resolves yet, settling as is")
	}

	steps := len(messages) / messagesPerTransaction
	if len(messages)%messagesPerTransaction > 0 {
		steps++
	}

	log.Info().Str("address", channel.Address).Int("steps", steps).Msg("calculated settle steps")

	for i := 0; i < steps; i++ {
		to := (i + 1) * messagesPerTransaction
		if to > len(messages) {
			to = len(messages)
		}

		var list [][]byte
		for _, c := range messages[i*messagesPerTransaction : to] {
			list = append(list, c.ToBOC())
		}

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "settle-step", channel.Address+"-settle",
			"settle-"+channel.Address+"-"+fmt.Sprint(i),
			db.SettleStepTask{
				Step:               i,
				Address:            channel.Address,
				Messages:           list,
				ChannelInitiatedAt: &channel.InitAt,
			}, nil, nil,
		); err != nil {
			log.Error().Err(err).Str("channel", channel.Address).Msg("failed to create settle step task")
		}

		log.Info().Str("address", channel.Address).Int("step", i).Msg("settle step created")
	}

	return nil
}

func (s *Service) executeSettleStep(ctx context.Context, channelAddr string, messages []*cell.Cell, step int) error {
	log.Info().Str("address", channelAddr).Int("step", step).Msg("executing settle step...")

	var list []*wallet.Message
	for _, message := range messages {
		list = append(list, wallet.SimpleMessage(address.MustParseAddr(channelAddr), tlb.MustFromTON("0.5"), message))
	}

	tx, _, err := s.wallet.SendManyWaitTransaction(ctx, list)
	if err != nil {
		return fmt.Errorf("failed to send internal messages to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Int("step", step).Int("messages", len(list)).Msg("settle conditions step transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) executeTopup(ctx context.Context, channelAddr string, amount tlb.Coins) error {
	log.Info().Str("address", channelAddr).Msg("executing topup...")

	if amount.Nano().Sign() <= 0 {
		// zero
		return nil
	}

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status != db.ChannelStateActive {
		log.Warn().Str("address", channelAddr).Msg("skip topup, channel is not active")
		return nil
	}

	c, err := tlb.ToCell(payments.TopupBalance{
		IsA: channel.WeLeft,
	})
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	var msg *wallet.Message
	if channel.JettonAddress != "" {
		master := jetton.NewJettonMasterClient(s.ton, address.MustParseAddr(channel.JettonAddress))

		jw, err := master.GetJettonWallet(ctx, s.wallet.WalletAddress())
		if err != nil {
			return fmt.Errorf("failed to get jetton wallet: %w", err)
		}

		tp, err := jw.BuildTransferPayloadV2(address.MustParseAddr(channelAddr), s.wallet.WalletAddress(), amount, tlb.MustFromTON("0.035"), c, nil)
		if err != nil {
			return fmt.Errorf("failed to build transfer payload: %w", err)
		}
		msg = wallet.SimpleMessage(jw.Address(), tlb.MustFromTON("0.07"), tp)
	} else if channel.ExtraCurrencyID > 0 {
		msg = wallet.SimpleMessage(address.MustParseAddr(channelAddr), tlb.MustFromTON("0.07"), c)
		msg.InternalMessage.ExtraCurrencies = cell.NewDict(32)
		_ = msg.InternalMessage.ExtraCurrencies.SetIntKey(big.NewInt(int64(channel.ExtraCurrencyID)), cell.BeginCell().MustStoreBigVarUInt(amount.Nano(), 32).EndCell())
	} else {
		// add ton accept fee
		toSend, err := tlb.FromNano(new(big.Int).Add(amount.Nano(), tlb.MustFromTON("0.03").Nano()), 9)
		if err != nil {
			return fmt.Errorf("failed to convert amount to nano: %w", err)
		}

		msg = wallet.SimpleMessage(address.MustParseAddr(channelAddr), toSend, c)
	}

	tx, _, err := s.wallet.SendManyWaitTransaction(ctx, []*wallet.Message{msg})
	if err != nil {
		return fmt.Errorf("failed to send internal messages to channel: %w", err)
	}
	log.Info().Str("hash", base64.StdEncoding.EncodeToString(tx.Hash)).Msg("topup transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}
