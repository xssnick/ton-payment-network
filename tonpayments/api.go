package tonpayments

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
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
	copy(channelId, nodeKey)

	binary.LittleEndian.PutUint32(channelId[12:], uint32(time.Now().UTC().Unix()))

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

	if err = s.CheckWalletBalance(ctx, jettonAddr, ecID, tlb.ZeroCoins); err != nil {
		return nil, fmt.Errorf("failed to check balance: %w", err)
	}

	excessFee := tlb.MustFromTON(cc.ExcessFeeTon)
	destWallet, configVirtualResp, err := s.regularTransport.ProposeChannelConfig(ctx, nodeKey, transport.ProposeChannelConfig{
		JettonAddr:               jettonData,
		ExtraCurrencyID:          ecID,
		ExcessFee:                excessFee.Nano().Bytes(),
		QuarantineDuration:       s.cfg.QuarantineDurationSec,
		MisbehaviorFine:          tlb.MustFromDecimal(cc.MisbehaviorFine, int(cc.Decimals)).Nano().Bytes(),
		ConditionalCloseDuration: s.cfg.ConditionalCloseDurationSec,
		NodeVersion:              payments.Version,
		CodeHash:                 payments.PaymentChannelCodes[0].Hash(),
	})
	if err != nil {
		return nil, fmt.Errorf("channel proposal failed: %w", err)
	}

	_ = configVirtualResp // TODO: check if it fits us
	// TODO: if code hash is unknown to peer try older if possible

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

	body, code, data, err := s.channelClient.GetDeployAsyncChannelParams(channelId, true, s.key, nodeKey, payments.ClosingConfig{
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
	addr, msgHash, err := s.wallet.DeployContractWaitTransaction(ctx, tlb.FromNanoTON(fee), body, code, data)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy: %w", err)
	}

	log.Info().Str("addr", addr.String()).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("contract deployed")

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

	if safe := vch.Deadline - (time.Now().UTC().Unix() + channel.SafeOnchainClosePeriod); safe < int64(s.cfg.MinSafeVirtualChannelTimeoutSec) {
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

func (s *Service) CommitAllOurVirtualChannelsAndWait(ctx context.Context) error {
	list, err := s.ListChannels(ctx, nil, db.ChannelStateActive)
	if err != nil {
		return fmt.Errorf("failed to list channels: %w", err)
	}

	for _, channel := range list {
		dictKV, err := channel.Our.Conditionals.LoadAll()
		if err != nil {
			log.Error().Err(err).Str("address", channel.Address).Msg("failed to load our conditionals")
			continue
		}

		for _, kv := range dictKV {
			vch, err := payments.ParseVirtualChannelCond(kv.Value)
			if err != nil {
				log.Error().Err(err).Str("address", channel.Address).Uint64("index", kv.Key.MustLoadUInt(32)).Msg("failed to parse conditional")
				continue
			}

			if err = s.CommitVirtualChannel(ctx, vch.Key); err != nil {
				log.Error().Err(err).Str("address", channel.Address).Uint64("index", kv.Key.MustLoadUInt(32)).Msg("failed to commit virtual channel")
				continue
			}
		}
	}

	for {
		// TODO: optimize
		tasks, err := s.db.ListActiveTasks(ctx, PaymentsTaskPool)
		if err != nil {
			return fmt.Errorf("failed to list tasks: %w", err)
		}

		has := false
		for _, task := range tasks {
			if task.Type == "commit-virtual" {
				has = true
				break
			}
		}

		if !has {
			// all commits completed
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Service) CommitVirtualChannel(ctx context.Context, key []byte) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	if meta.Outgoing == nil {
		return fmt.Errorf("virtual channel is not outgoing")
	}

	resolve := meta.GetKnownResolve()
	if resolve == nil {
		// nothing to commit
		return nil
	}

	ch, err := s.db.GetChannel(ctx, meta.Outgoing.ChannelAddress)
	if err != nil {
		return fmt.Errorf("failed to get outgoing channel: %w", err)
	}

	_, vch, err := payments.FindVirtualChannel(ch.Our.Conditionals, key)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			// no need
			return nil
		}
		return fmt.Errorf("failed to find virtual channel: %w", err)
	}

	if vch.Prepay.Cmp(new(big.Int).Add(resolve.Amount, vch.Fee)) >= 0 {
		// already commited
		return nil
	}

	tryTill := time.Unix(vch.Deadline-ch.SafeOnchainClosePeriod, 0)
	err = s.db.CreateTask(ctx, PaymentsTaskPool, "commit-virtual", ch.Address,
		"commit-virtual-"+base64.StdEncoding.EncodeToString(vch.Key)+"-"+resolve.Amount.String(),
		db.CommitVirtualTask{
			ChannelAddress: ch.Address,
			VirtualKey:     vch.Key,
		}, nil, &tryTill,
	)
	if err != nil {
		return fmt.Errorf("failed to create virtual commit task: %w", err)
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

		if vch.Deadline < time.Now().UTC().Unix() {
			return fmt.Errorf("virtual channel has expired")
		}

		if state.Amount.Cmp(vch.Capacity) > 0 {
			return fmt.Errorf("amount cannot be > capacity")
		}
	} else {
		// in case we are the first point, check against our channel
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

		if vch.Deadline < time.Now().UTC().Unix() {
			return fmt.Errorf("virtual channel has expired")
		}

		if state.Amount.Cmp(vch.Capacity) > 0 {
			return fmt.Errorf("amount cannot be > capacity")
		}
	}

	// TODO: maybe allow in want state, but need to check concurrency cases
	if meta.Status != db.VirtualChannelStateActive {
		return fmt.Errorf("virtual channel is inactive")
	}

	if err = meta.AddKnownResolve(&state); err != nil {
		return fmt.Errorf("failed to add channel condition resolve: %w", err)
	}

	meta.UpdatedAt = time.Now()
	if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	return nil
}

func (s *Service) RequestUncooperativeClose(ctx context.Context, addr string) error {
	channel, err := s.GetChannel(ctx, addr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		return fmt.Errorf("channel is already inactive")
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

var minTonAmountForTx = tlb.MustFromTON("0.25")
var ErrNotEnoughTonBalance = fmt.Errorf("not enough ton balance")
var ErrNotEnoughBalance = fmt.Errorf("not enough balance")

func (s *Service) CheckWalletBalance(ctx context.Context, jettonAddr string, ec uint32, amount tlb.Coins) error {
	acc, err := s.ton.GetAccount(ctx, s.wallet.WalletAddress())
	if err != nil {
		return fmt.Errorf("failed to get ton balance: %w", err)
	}
	if !acc.HasState {
		return fmt.Errorf("wallet is not exists, topup it")
	}

	balance := acc.Balance.Nano()
	balance = balance.Sub(balance, minTonAmountForTx.Nano())
	if balance.Sign() < 0 {
		return ErrNotEnoughTonBalance
	}

	if amount.Nano().Sign() <= 0 {
		return nil
	}

	// just checking it is enabled
	_, err = s.ResolveCoinConfig(jettonAddr, ec, true)
	if err != nil {
		return fmt.Errorf("failed to resolve coin config: %w", err)
	}

	if jettonAddr != "" {
		balance, err = s.ton.GetJettonBalance(ctx, address.MustParseAddr(jettonAddr), s.wallet.WalletAddress())
		if err != nil {
			return fmt.Errorf("failed to get jetton balance: %w", err)
		}
	} else if ec > 0 {
		if isWeb {
			panic("extra currency is not supported on web")
		}

		if acc.ExtraCurrencies.IsEmpty() {
			return fmt.Errorf("no extra currencies in wallet")
		}

		val, err := acc.ExtraCurrencies.LoadValueByIntKey(big.NewInt(int64(ec)))
		if err != nil {
			return fmt.Errorf("failed to get extra currency value: %w", err)
		}

		balance, err = val.LoadVarUInt(32)
		if err != nil {
			return fmt.Errorf("failed to parse extra currency value: %w", err)
		}
	}

	if balance.Cmp(amount.Nano()) < 0 {
		return ErrNotEnoughBalance
	}

	return nil
}

func (s *Service) TopupChannel(ctx context.Context, addr *address.Address, amount tlb.Coins) error {
	channel, err := s.GetActiveChannel(ctx, addr.Bounce(true).String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, amount); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "topup", channel.Address+"-topup",
		"topup-"+channel.Address+"-"+fmt.Sprint(time.Now().UTC().Unix()),
		db.TopupTask{
			Address:            channel.Address,
			Amount:             amount.String(),
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
	var err error
	fullAmount := new(big.Int).Add(amount.Nano(), channel.OurOnchain.Withdrawn)

	amount, err = tlb.FromNano(fullAmount, amount.Decimals())
	if err != nil {
		return fmt.Errorf("failed to convert amount to nano: %w", err)
	}

	maxTheirWithdraw := new(big.Int).Set(channel.Their.PendingWithdraw)
	if channel.TheirOnchain.Withdrawn.Cmp(maxTheirWithdraw) > 0 {
		maxTheirWithdraw.Set(channel.TheirOnchain.Withdrawn)
	}
	amountTheir, err := tlb.FromNano(maxTheirWithdraw, amount.Decimals())
	if err != nil {
		return fmt.Errorf("failed to convert amount to nano: %w", err)
	}
	// 4.2 - 1.3

	if _, _, _, err := s.getCommitRequest(amount, amountTheir, channel); err != nil {
		return fmt.Errorf("failed to prepare channel commit request: %w", err)
	}

	if err := s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	if err := s.db.CreateTask(ctx, PaymentsTaskPool, "withdraw", channel.Address+"-withdraw",
		"withdraw-"+channel.Address+"-"+fmt.Sprint(channel.InitAt.Unix())+fmt.Sprintf("-%d-%d", channel.Their.State.Data.Seqno, channel.Our.State.Data.Seqno),
		db.WithdrawTask{
			Address:            channel.Address,
			Amount:             amount.String(),
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

	resolve := meta.GetKnownResolve()
	if resolve == nil {
		return ErrNoResolveExists
	}

	meta.Status = db.VirtualChannelStateWantClose
	meta.UpdatedAt = time.Now()
	if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
		return fmt.Errorf("failed to update channel in db: %w", err)
	}

	till := time.Unix(vch.Deadline, 0)

	// if it was prepaid for resolve amount, no need do onchain close when not succeed
	prepaid := vch.Prepay.Cmp(new(big.Int).Add(resolve.Amount, vch.Fee)) >= 0
	if !prepaid {
		// We start uncooperative close at specific moment to have time
		// to commit resolve onchain in case partner is irresponsible.
		// But in the same time we give our partner time to
		till = time.Unix(vch.Deadline-ch.SafeOnchainClosePeriod, 0)
		minDelay := time.Now().Add(1 * time.Minute)
		if !till.After(minDelay) {
			till = minDelay
		}

		// Creating aggressive onchain close task, for the future,
		// in case we will not be able to communicate with party
		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", ch.Address+"-uncoop",
			"uncooperative-close-"+ch.Address+"-vc-"+base64.StdEncoding.EncodeToString(vch.Key),
			db.ChannelUncooperativeCloseTask{
				Address:                 ch.Address,
				CheckVirtualStillExists: vch.Key,
			}, &till, nil,
		); err != nil {
			log.Warn().Err(err).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to create uncooperative close task")
		}
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "ask-close-virtual", ch.Address+"-coop",
		"virtual-close-"+ch.Address+"-vc-"+base64.StdEncoding.EncodeToString(vch.Key),
		db.AskCloseVirtualTask{
			Key:            vch.Key,
			ChannelAddress: ch.Address,
		}, nil, &till,
	); err != nil {
		log.Warn().Err(err).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("failed to create cooperative close task")
	}
	s.touchWorker()

	log.Info().Err(err).Bool("prepaid", prepaid).Str("channel", ch.Address).Str("key", base64.StdEncoding.EncodeToString(vch.Key)).Msg("virtual channel close task created and will be executed soon")

	return nil
}

func (s *Service) executeCooperativeCommit(ctx context.Context, req *payments.CooperativeCommit, ch *db.Channel) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	if err = s.CheckWalletBalance(ctx, ch.JettonAddress, ch.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.wallet.DoTransaction(ctx, "Cooperative commit (withdraw)", address.MustParseAddr(ch.Address), tlb.MustFromTON("0.16"), msg)
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("addr", ch.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("cooperative commit transaction completed")
	return nil
}

func (s *Service) executeCooperativeClose(ctx context.Context, req *payments.CooperativeClose, channel *db.Channel) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	if err = s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.wallet.DoTransaction(ctx, "Cooperative channel close", address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msg)
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("cooperative close transaction completed")

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
	if channel.Our.PendingWithdraw.Cmp(ourWithdraw.Nano()) > 0 || channel.OurOnchain.Withdrawn.Cmp(ourWithdraw.Nano()) > 0 {
		return nil, nil, nil, fmt.Errorf("our withdraw %s cannot decrease %s %s", ourWithdraw.String(), channel.Our.PendingWithdraw.String(), channel.OurOnchain.Withdrawn.String())
	}
	if channel.Their.PendingWithdraw.Cmp(theirWithdraw.Nano()) > 0 || channel.TheirOnchain.Withdrawn.Cmp(theirWithdraw.Nano()) > 0 {
		return nil, nil, nil, fmt.Errorf("their withdraw %s cannot decrease %s", theirWithdraw.String(), channel.TheirOnchain.Withdrawn.String())
	}

	maxOurWithdraw := new(big.Int).Set(channel.Our.PendingWithdraw)
	if channel.OurOnchain.Withdrawn.Cmp(maxOurWithdraw) > 0 {
		maxOurWithdraw.Set(channel.OurOnchain.Withdrawn)
	}

	maxTheirWithdraw := new(big.Int).Set(channel.Their.PendingWithdraw)
	if channel.TheirOnchain.Withdrawn.Cmp(maxTheirWithdraw) > 0 {
		maxTheirWithdraw.Set(channel.TheirOnchain.Withdrawn)
	}

	ourToWithdraw := new(big.Int).Sub(ourWithdraw.Nano(), maxOurWithdraw)
	theirToWithdraw := new(big.Int).Sub(theirWithdraw.Nano(), maxTheirWithdraw)

	// this is not locked balance
	ourBalance, err := channel.CalcBalance(false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
	}

	theirBalance, err := channel.CalcBalance(true)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to calc their balance: %w", err)
	}

	if ourToWithdraw.Cmp(ourBalance) > 0 {
		return nil, nil, nil, fmt.Errorf("our withdraw %s is greater than balance %s ", ourToWithdraw.String(), ourBalance.String())
	}
	if theirToWithdraw.Cmp(theirBalance) > 0 {
		return nil, nil, nil, fmt.Errorf("their withdraw %s is greater than balance %s", theirToWithdraw.String(), theirBalance.String())
	}

	var ourReq payments.CooperativeCommit
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.SentA = channel.Our.State.Data.Sent
	ourReq.Signed.SentB = channel.Their.State.Data.Sent
	ourReq.Signed.SeqnoA = channel.Our.State.Data.Seqno + 1
	ourReq.Signed.SeqnoB = channel.Their.State.Data.Seqno + 1
	ourReq.Signed.WithdrawA = ourWithdraw
	ourReq.Signed.WithdrawB = theirWithdraw
	if !channel.WeLeft {
		ourReq.Signed.WithdrawA, ourReq.Signed.WithdrawB = ourReq.Signed.WithdrawB, ourReq.Signed.WithdrawA
		ourReq.Signed.SentA, ourReq.Signed.SentB = ourReq.Signed.SentB, ourReq.Signed.SentA
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

	if channel.Our.PendingWithdraw.Cmp(new(big.Int).SetUint64(0)) > 0 || channel.Their.PendingWithdraw.Cmp(new(big.Int).SetUint64(0)) > 0 {
		return nil, nil, nil, fmt.Errorf("pending withdraw is not zero")
	}

	for _, kv := range allOur {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.Deadline >= time.Now().UTC().Unix() {
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
		if vch.Deadline >= time.Now().UTC().Unix() {
			return nil, nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	var ourReq payments.CooperativeClose
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.SentA = channel.Our.State.Data.Sent
	ourReq.Signed.SentB = channel.Their.State.Data.Sent
	ourReq.Signed.SeqnoA = channel.Our.State.Data.Seqno + 1
	ourReq.Signed.SeqnoB = channel.Their.State.Data.Seqno + 1
	if !channel.WeLeft {
		ourReq.Signed.SentA, ourReq.Signed.SentB = ourReq.Signed.SentB, ourReq.Signed.SentA
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
	channel, err := s.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status != db.ChannelStateActive {
		return fmt.Errorf("channel is not active")
	}

	channel.AcceptingActions = false
	if err = s.db.UpdateChannel(ctx, channel); err != nil {
		return fmt.Errorf("failed to update channel: %w", err)
	}

	och, err := s.channelClient.GetAsyncChannel(ctx, address.MustParseAddr(channelAddr), true)
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

	if err = s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.wallet.DoTransaction(ctx, "Channel closure initiation, because peer not responding for too long", address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("uncooperative close transaction completed")

	return nil
}

func (s *Service) challengeChannelState(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	och, err := s.channelClient.GetAsyncChannel(ctx, address.MustParseAddr(channelAddr), true)
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

	if err := s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.wallet.DoTransaction(ctx, "Channel state challenge, because peer committed older state", address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("challenge channel state transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) finishUncooperativeChannelClose(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	och, err := s.channelClient.GetAsyncChannel(ctx, address.MustParseAddr(channelAddr), true)
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

	if err := s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.wallet.DoTransaction(ctx, "Complete channel closure procedure", address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("finish uncooperative close transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) settleChannelConditionals(ctx context.Context, channelAddr string) error {
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

	och, err := s.channelClient.GetAsyncChannel(ctx, address.MustParseAddr(channelAddr), true)
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

		if resolve := meta.GetKnownResolve(); resolve != nil {
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

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	var list []WalletMessage
	for _, message := range messages {
		list = append(list, WalletMessage{
			To:     address.MustParseAddr(channelAddr),
			Amount: tlb.MustFromTON("0.5"),
			Body:   message,
		})
	}

	msgHash, err := s.wallet.DoTransactionMany(ctx, fmt.Sprintf("Channel actions settle step %d (required to resolve pending virtual channels)", step), list)
	if err != nil {
		return fmt.Errorf("failed to send internal messages to channel: %w", err)
	}
	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Int("step", step).Int("messages", len(list)).Msg("settle conditions step transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) ExecuteTopup(ctx context.Context, channelAddr string, amount tlb.Coins) error {
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

	if err = s.CheckWalletBalance(ctx, channel.JettonAddress, channel.ExtraCurrencyID, amount); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	c, err := tlb.ToCell(payments.TopupBalance{
		IsA: channel.WeLeft,
	})
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	var msgHash []byte
	if channel.JettonAddress != "" {
		jw, err := s.ton.GetJettonWalletAddress(ctx, address.MustParseAddr(channel.JettonAddress), s.wallet.WalletAddress())
		if err != nil {
			return fmt.Errorf("failed to get jetton wallet: %w", err)
		}

		tp, err := buildJettonTransferPayload(address.MustParseAddr(channelAddr), s.wallet.WalletAddress(), amount, tlb.MustFromTON("0.035"), c, nil)
		if err != nil {
			return fmt.Errorf("failed to build transfer payload: %w", err)
		}

		msgHash, err = s.wallet.DoTransaction(ctx, "Channel balance top up (jetton)", jw, tlb.MustFromTON("0.07"), tp)
		if err != nil {
			return fmt.Errorf("failed to send internal messages to channel (via jetton): %w", err)
		}
	} else if channel.ExtraCurrencyID > 0 {
		msgHash, err = s.wallet.DoTransactionEC(ctx, "Channel balance top up (extra currency)", address.MustParseAddr(channelAddr), tlb.MustFromTON("0.07"), c, channel.ExtraCurrencyID, amount)
		if err != nil {
			return fmt.Errorf("failed to send internal messages to channel: %w", err)
		}
	} else {
		// add ton accept fee
		toSend, err := tlb.FromNano(new(big.Int).Add(amount.Nano(), tlb.MustFromTON("0.03").Nano()), 9)
		if err != nil {
			return fmt.Errorf("failed to convert amount to nano: %w", err)
		}

		msgHash, err = s.wallet.DoTransaction(ctx, "Channel balance top up", address.MustParseAddr(channelAddr), toSend, c)
		if err != nil {
			return fmt.Errorf("failed to send internal messages to channel: %w", err)
		}
	}

	log.Info().Str("addr", channel.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("topup transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

// we copied it here to lower binary size for wasm build (because of imports chain)
func buildJettonTransferPayload(to, responseTo *address.Address, amountCoins, amountForwardTON tlb.Coins, payloadForward, customPayload *cell.Cell) (*cell.Cell, error) {
	type TransferPayload struct {
		_                   tlb.Magic        `tlb:"#0f8a7ea5"`
		QueryID             uint64           `tlb:"## 64"`
		Amount              tlb.Coins        `tlb:"."`
		Destination         *address.Address `tlb:"addr"`
		ResponseDestination *address.Address `tlb:"addr"`
		CustomPayload       *cell.Cell       `tlb:"maybe ^"`
		ForwardTONAmount    tlb.Coins        `tlb:"."`
		ForwardPayload      *cell.Cell       `tlb:"either . ^"`
	}

	if payloadForward == nil {
		payloadForward = cell.BeginCell().EndCell()
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	rnd := binary.LittleEndian.Uint64(buf)

	body, err := tlb.ToCell(TransferPayload{
		QueryID:             rnd,
		Amount:              amountCoins,
		Destination:         to,
		ResponseDestination: responseTo,
		CustomPayload:       customPayload,
		ForwardTONAmount:    amountForwardTON,
		ForwardPayload:      payloadForward,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to convert TransferPayload to cell: %w", err)
	}

	return body, nil
}
