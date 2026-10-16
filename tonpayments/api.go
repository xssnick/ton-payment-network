package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
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

func (s *Service) ResolveCoinConfig(balanceId string) (*payments.CoinConfig, error) {
	b := s.knownBalanceTypes[balanceId]
	if b == nil || !b.Enabled {
		return nil, ErrNotWhitelisted
	}
	return b, nil
}

func (s *Service) ResolveCoinConfigBySymbol(sym string) (*payments.CoinConfig, error) {
	sym = strings.ToUpper(sym)
	b := s.knownBalanceTypesSymbols[sym]
	if b == nil || !b.Enabled {
		return nil, ErrNotWhitelisted
	}
	return b, nil
}

func (s *Service) GetTunnelingFees(ctx context.Context, balanceId string) (enabled bool, minFee, maxCap tlb.Coins, percentFee float64, err error) {
	cc, err := s.ResolveCoinConfig(balanceId)
	if err != nil {
		if errors.Is(err, ErrNotWhitelisted) {
			return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, nil
		}
		return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, fmt.Errorf("failed to resolve coin config: %w", err)
	}

	if !cc.VirtualTunnelConfig.AllowTunneling {
		return false, tlb.ZeroCoins, tlb.ZeroCoins, 0, nil
	}

	return true, cc.VirtualTunnelConfig.ProxyMinFee, cc.VirtualTunnelConfig.ProxyMaxCapacity, cc.VirtualTunnelConfig.ProxyFeePercent, nil
}

func (s *Service) OpenChannelWithNode(ctx context.Context, nodeKey ed25519.PublicKey) (*address.Address, error) {
	log.Info().Str("with", base64.StdEncoding.EncodeToString(nodeKey)).Msg("locating node and proposing channel config...")

	codeHash := payments.PaymentChannelCodes[0].Hash()
	channelId := make([]byte, 16)
	copy(channelId, nodeKey)

	binary.LittleEndian.PutUint32(channelId[12:], uint32(time.Now().UTC().Unix()))

	attach := tlb.MustFromTON(s.cfg.ReplicationMessageAttachAmount)
	err := s.regularTransport.ProposeChannelConfig(ctx, nodeKey, transport.ProposeChannelConfig{
		ReplicateAttachAmount:    attach.Nano().Bytes(),
		QuarantineDuration:       s.cfg.QuarantineDurationSec,
		ActionsExecuteDuration:   s.cfg.ActionsDuration,
		ConditionalCloseDuration: s.cfg.ConditionalCloseDurationSec,
		NodeVersion:              payments.Version,
		CodeHash:                 codeHash,
	})
	if err != nil {
		return nil, fmt.Errorf("channel proposal failed: %w", err)
	}

	log.Info().Msg("starting channel opening...")

	ctr := payments.OpenConfigContainer{
		Seqno:     0, // reopen not supported for now
		KeyA:      s.key.Public().(ed25519.PublicKey),
		KeyB:      nodeKey,
		ChannelID: channelId,
		ClosingConfig: payments.ClosingConfig{
			QuarantineDuration:             s.cfg.QuarantineDurationSec,
			ConditionalCloseDuration:       s.cfg.ConditionalCloseDurationSec,
			ActionsDuration:                s.cfg.ActionsDuration,
			ReplicationMessageAttachAmount: attach,
		},
	}

	_, _, ourSig, err := s.channelClient.GetDeployAsyncChannelParams(ctr.ChannelID, true, ctr.Seqno, s.key, nodeKey, nil, ctr.ClosingConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get deploy params: %w", err)
	}
	ctr.InitSignature.Value = ourSig

	_, theirInitSig, err := s.regularTransport.OpenOffchainChannel(ctx, nodeKey, codeHash, ctr)
	if err != nil {
		return nil, fmt.Errorf("failed to open channel request: %w", err)
	}
	ctr.InitSignature.Value = theirInitSig

	var addr *address.Address
	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		addr, _, err = s.OpenChannelOffchain(ctx, &ctr, codeHash, nodeKey, true, false)
		if err != nil {
			return fmt.Errorf("failed to open channel: %w", err)
		}

		/*if !theirSideAddr.Equals(address.MustParseAddr(ch.Their.Address)) {
			return fmt.Errorf("their side address is different %s %s", addr.String(), theirSideAddr.String())
		}*/
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create channel failed: %w", err)
	}

	log.Info().Str("addr", addr.String()).Msg("channel opened offchain")

	return addr, nil
}

func (s *Service) InitiateSwap(ctx context.Context, channel *db.Channel, fromCC, toCC *payments.CoinConfig, fromAmt, toAmt tlb.Coins) error {
	fid, _ := hex.DecodeString(fromCC.BalanceID)
	tid, _ := hex.DecodeString(toCC.BalanceID)

	s.mx.Lock()
	defer s.mx.Unlock()

	ourBalances, err := channel.CalcBalance(ctx, false, s)
	if err != nil {
		return fmt.Errorf("failed to calc our balances: %w", err)
	}
	if b := ourBalances[fromCC.BalanceID]; b == nil || b.Available().Cmp(fromAmt.Nano()) < 0 {
		return fmt.Errorf("not enough our balance")
	}

	theirBalances, err := channel.CalcBalance(ctx, true, s)
	if err != nil {
		return fmt.Errorf("failed to calc their balances: %w", err)
	}
	if b := theirBalances[toCC.BalanceID]; b == nil || b.Available().Cmp(toAmt.Nano()) < 0 {
		return fmt.Errorf("not enough their balance")
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "swap", channel.Our.Address,
		"swap-"+channel.Our.Address+"-"+fmt.Sprint(time.Now().UnixNano()),
		db.SwapTask{
			ChannelAddress: channel.Our.Address,
			TransportAction: transport.SwapAction{
				FromBalanceID: fid,
				ToBalanceID:   tid,
				FromAmount:    fromAmt.Nano().Bytes(),
				ToAmount:      toAmt.Nano().Bytes(),
			},
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create open task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) CreateSendConditional(ctx context.Context, instructionKey ed25519.PublicKey, private ed25519.PrivateKey, firstPart, lastPart transport.TunnelChainPart, chain []transport.AddConditionalInstruction, cc *payments.CoinConfig) error {
	if len(chain) == 0 {
		return fmt.Errorf("chain is empty")
	}

	channels, err := s.db.GetChannels(ctx, firstPart.Target, db.ChannelStateActive)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	needAmount := new(big.Int).Add(firstPart.Fee, firstPart.Capacity)
	var channel *db.Channel
	for _, ch := range channels {
		balances, err := ch.CalcBalance(ctx, false, s)
		if err != nil {
			return fmt.Errorf("failed to calc channel balance: %w", err)
		}

		if balances[cc.BalanceID].Available().Cmp(needAmount) >= 0 {
			// we found channel with enough balance
			channel = ch
			break
		}
	}

	if channel == nil {
		return fmt.Errorf("failed to open virtual channel, %w: no active channel with enough balance exists", ErrNotPossible)
	}

	a, b := channel.Our.Address, channel.Their.Address
	if !channel.WeLeft {
		a, b = b, a
	}

	act, err := actions.NewSendActionFromBalanceID(ctx, cc, a, b)
	if err != nil {
		return fmt.Errorf("failed to create action: %w", err)
	}

	vch := conditionals.ConditionalVirtualChannel{
		Key:      private.Public().(ed25519.PublicKey),
		Capacity: firstPart.Capacity,
		Fee:      firstPart.Fee,
		Prepay:   big.NewInt(0),
		Deadline: firstPart.Deadline.Unix(),
		Action:   act,
	}

	if safe := vch.Deadline - (time.Now().UTC().Unix() + channel.SafeOnchainClosePeriod); safe < int64(s.cfg.MinSafeVirtualChannelTimeoutSec) {
		return fmt.Errorf("safe deadline is less than acceptable: %d, %d", safe, s.cfg.MinSafeVirtualChannelTimeoutSec)
	}

	tAct := transport.AddConditionalAction{
		Conditional:    vch.Serialize(),
		InstructionKey: instructionKey,
	}

	if _, err = channel.Our.Data.ActionStates.LoadValue(act.IDCell()); err != nil {
		if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return fmt.Errorf("failed to load action state: %w", err)
		}
		tAct.NewActionCode = act.Serialize()
	}

	if err = tAct.SetInstructions(chain, private); err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "create-send-conditional", channel.Our.Address,
		"create-send-conditional-"+base64.StdEncoding.EncodeToString(vch.Key),
		db.AddConditionalTask{
			FinalDestinationKey: lastPart.Target,
			ChannelAddress:      channel.Our.Address,
			Deadline:            vch.Deadline,
			TransportAction:     tAct,
		}, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create open task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) CreateDerivativeCond(ctx context.Context, instructionKey ed25519.PublicKey, private ed25519.PrivateKey, firstPart transport.TunnelChainPart, instruction transport.AddConditionalInstruction, cc *payments.CoinConfig,
	amount, fee *big.Int,
	details conditionals.ConditionalResolvableDetails, resolverAddr *address.Address) error {
	if amount == nil || amount.Sign() <= 0 {
		return fmt.Errorf("invalid derivative amount")
	}
	if fee == nil || fee.Sign() < 0 {
		return fmt.Errorf("invalid derivative fee")
	}

	channels, err := s.db.GetChannels(ctx, firstPart.Target, db.ChannelStateActive)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	needAmount := new(big.Int).Add(amount, fee)
	var channel *db.Channel
	for _, ch := range channels {
		balances, err := ch.CalcBalance(ctx, false, s)
		if err != nil {
			return fmt.Errorf("failed to calc channel balance: %w", err)
		}

		if balances[cc.BalanceID].Available().Cmp(needAmount) >= 0 {
			// we found channel with enough balance
			channel = ch
			break
		}
	}

	if channel == nil {
		return fmt.Errorf("failed to open derivative, %w: no active channel with enough balance exists", ErrNotPossible)
	}

	a, b := channel.Our.Address, channel.Their.Address
	if !channel.WeLeft {
		a, b = b, a
	}

	act, err := actions.NewSendActionFromBalanceID(ctx, cc, a, b)
	if err != nil {
		return fmt.Errorf("failed to create action: %w", err)
	}

	condRes := conditionals.ConditionalResolvable{
		Key:          private.Public().(ed25519.PublicKey),
		Amount:       amount,
		Fee:          fee,
		IsInitiator:  true,
		ResolverAddr: resolverAddr,
		Details:      details,
		Action:       act,
	}

	tAct := transport.AddConditionalAction{
		Conditional:    condRes.Serialize(),
		InstructionKey: instructionKey,
	}

	if _, err = channel.Our.Data.ActionStates.LoadValue(act.IDCell()); err != nil {
		if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return fmt.Errorf("failed to load action state: %w", err)
		}
		tAct.NewActionCode = act.Serialize()
	}

	if err = tAct.SetInstructions([]transport.AddConditionalInstruction{instruction}, private); err != nil {
		return fmt.Errorf("failed to set instructions: %w", err)
	}

	err = s.db.CreateTask(ctx, PaymentsTaskPool, "create-derivative-cond", channel.Our.Address,
		"create-derivative-cond-"+base64.StdEncoding.EncodeToString(condRes.Key),
		db.AddDerivativeTask{
			ChannelAddress:  channel.Our.Address,
			Deadline:        firstPart.Deadline.Unix(),
			TransportAction: tAct,
		}, nil, nil,
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
		dictKV, err := channel.Our.Data.Conditionals.LoadAll()
		if err != nil {
			log.Error().Err(err).Str("address", channel.Our.Address).Msg("failed to load our conditionals")
			continue
		}

		for _, kv := range dictKV {
			vch, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
			if err != nil {
				log.Error().Err(err).Str("address", channel.Our.Address).
					Str("hash", base64.StdEncoding.EncodeToString(kv.Key.MustLoadSlice(256))).
					Msg("failed to parse conditional")
				continue
			}

			if err = s.CommitVirtualChannel(ctx, vch.GetKey()); err != nil {
				log.Error().Err(err).Str("address", channel.Our.Address).
					Str("hash", base64.StdEncoding.EncodeToString(kv.Key.MustLoadSlice(256))).
					Msg("failed to commit virtual channel")
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

	resolve := meta.LastKnownResolve
	if resolve == nil {
		// nothing to commit
		return nil
	}

	ch, err := s.db.GetChannel(ctx, meta.Outgoing.ChannelAddress)
	if err != nil {
		return fmt.Errorf("failed to get outgoing channel: %w", err)
	}

	_, vch, err := payments.FindConditional(ctx, ch.Our.Data.Conditionals, meta.Outgoing.Conditional.Hash(), s)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			// no need
			return nil
		}
		return fmt.Errorf("failed to find virtual channel: %w", err)
	}

	upd, err := vch.PrepareCommit(resolve)
	if err != nil {
		return fmt.Errorf("failed to prepare commit: %w", err)
	}

	if bytes.Equal(upd.Serialize().Hash(), vch.Serialize().Hash()) {
		// already commited
		return nil
	}

	tryTill := vch.GetDeadline().Add(time.Duration(-ch.SafeOnchainClosePeriod) * time.Second)
	err = s.db.CreateTask(ctx, PaymentsTaskPool, "commit-virtual", ch.Our.Address,
		"commit-virtual-"+base64.StdEncoding.EncodeToString(vch.GetKey())+"-"+base64.StdEncoding.EncodeToString(resolve.Hash()),
		db.CommitVirtualTask{
			ChannelAddress: ch.Our.Address,
			VirtualKey:     vch.GetKey(),
		}, nil, &tryTill,
	)
	if err != nil {
		return fmt.Errorf("failed to create virtual commit task: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) AddConditionalResolve(ctx context.Context, virtualKey ed25519.PublicKey, state *cell.Cell) error {
	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	// TODO: maybe allow in want state, but need to check concurrency cases
	if meta.Status != db.ConditionalStateActive {
		return fmt.Errorf("virtual channel is inactive, state %d", meta.Status)
	}

	var cond payments.Conditional
	if meta.Incoming != nil {
		ch, err := s.db.GetChannel(ctx, meta.Incoming.ChannelAddress)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("onchain channel with source not exists")
			}
			return fmt.Errorf("failed to load channel: %w", err)
		}

		_, cond, err = payments.FindConditional(ctx, ch.Their.Data.Conditionals, meta.Incoming.Conditional.Hash(), s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return nil
			}

			log.Error().Err(err).Str("channel", ch.Our.Address).Msg("failed to find conditional")
			return fmt.Errorf("failed to find conditional: %w", err)
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

		_, cond, err = payments.FindConditional(ctx, ch.Our.Data.Conditionals, meta.Outgoing.Conditional.Hash(), s)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				// idempotency
				return nil
			}

			log.Error().Err(err).Str("channel", ch.Our.Address).Msg("failed to find conditional")
			return fmt.Errorf("failed to find conditional: %w", err)
		}
	}

	if err = meta.AddKnownResolve(ctx, cond, state, true); err != nil {
		return fmt.Errorf("failed to add channel condition resolve: %w", err)
	}

	meta.UpdatedAt = time.Now()
	if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
		return fmt.Errorf("failed to update meta in db: %w", err)
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

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", channel.Our.Address+"-uncoop",
		"uncooperative-close-"+channel.Our.Address+"-"+fmt.Sprint(channel.InitAt.Unix()),
		db.ChannelUncooperativeCloseTask{
			Address: channel.Our.Address,
		}, nil, nil,
	); err != nil {
		return err
	}
	return nil
}

var minTonAmountForTx = tlb.MustFromTON("0.15")
var ErrNotEnoughTonBalance = fmt.Errorf("not enough ton balance")
var ErrNotEnoughBalance = fmt.Errorf("not enough balance")

func (s *Service) CheckWalletBalance(ctx context.Context, balanceId string, amount tlb.Coins) error {
	return s.CheckAddrBalance(ctx, s.wallet.WalletAddress(), balanceId, amount)
}

func (s *Service) CheckAddrBalance(ctx context.Context, addr *address.Address, balanceId string, amount tlb.Coins) error {
	acc, err := s.ton.GetAccount(ctx, addr, time.Time{})
	if err != nil {
		return fmt.Errorf("failed to get ton balance: %w", err)
	}
	if !acc.HasState {
		return fmt.Errorf("%s address is not exists, topup it", addr.String())
	}

	balance := acc.Balance.Nano()
	balance = balance.Sub(balance, minTonAmountForTx.Nano())
	if balance.Sign() < 0 {
		return ErrNotEnoughTonBalance
	}

	if amount.Nano().Sign() <= 0 {
		return nil
	}

	// if it is empty we're just checking ton balance (in case we need to do uncoop tx or something)
	if balanceId != "" {
		cc, err := s.ResolveCoinConfig(balanceId)
		if err != nil {
			return fmt.Errorf("failed to resolve coin config: %w", err)
		}

		if cc.JettonClient != nil {
			balance, err = cc.JettonClient.GetBalance(ctx, addr, time.Time{})
			if err != nil {
				return fmt.Errorf("failed to get jetton balance: %w", err)
			}
		} else if cc.BalanceID != payments.GetTONBalanceID() {
			if isWeb {
				panic("extra currency is not supported on web")
			}

			if acc.ExtraCurrencies.IsEmpty() {
				return fmt.Errorf("no extra currencies in wallet")
			}
			ec := payments.GetECFromBalanceID(cc.BalanceID)

			val, err := acc.ExtraCurrencies.LoadValueByIntKey(big.NewInt(int64(ec)))
			if err != nil {
				return fmt.Errorf("failed to get extra currency value: %w", err)
			}

			balance, err = val.LoadVarUInt(32)
			if err != nil {
				return fmt.Errorf("failed to parse extra currency value: %w", err)
			}
		}
	}

	if balance.Cmp(amount.Nano()) < 0 {
		return ErrNotEnoughBalance
	}

	return nil
}

func (s *Service) TopupChannel(ctx context.Context, channel *db.Channel, balanceId string, amount tlb.Coins, unlockBalanceControl bool) error {
	_, err := s.ResolveCoinConfig(balanceId)
	if err != nil {
		return fmt.Errorf("failed to resolve coin config: %w", err)
	}

	if err := s.CheckWalletBalance(ctx, balanceId, amount); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	bls := channel.Our.OnchainBalances[balanceId]
	if bls == nil {
		bls = big.NewInt(0)
	}

	if err := s.db.CreateTask(ctx, PaymentsTaskPool, "topup", channel.Our.Address+"-topup",
		"topup-"+channel.Our.Address+"-"+bls.String()+"-"+balanceId+"-"+fmt.Sprint(channel.InitAt.Unix())+"-"+fmt.Sprint(time.Now().Unix()),
		db.TopupTask{
			Address:            channel.Our.Address,
			Amount:             amount.String(),
			BalanceID:          balanceId,
			ChannelInitiatedAt: channel.InitAt,
			FromBalanceControl: unlockBalanceControl,
		}, nil, nil,
	); err != nil {
		return err
	}
	log.Info().Str("address", channel.Our.Address).Str("amount", amount.String()).Msg("topup task registered")
	return nil
}

func (s *Service) RequestCommitAction(ctx context.Context, addr *address.Address, actionId []byte) error {
	channel, err := s.GetActiveChannel(ctx, addr.Bounce(true).String())
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	return s.requestCommitAction(ctx, channel, actionId)
}

func (s *Service) requestCommitAction(ctx context.Context, channel *db.Channel, actionId []byte) error {
	doTxOurself := true
	if err := s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		if !errors.Is(err, ErrNotEnoughTonBalance) {
			return fmt.Errorf("failed to check balance: %w", err)
		}
		doTxOurself = false
	}

	var feeFromUs *bool
	if doTxOurself {
		feeFromUs = &doTxOurself
	}

	if _, _, _, _, err := s.getCommitRequest(ctx, channel, actionId, doTxOurself, feeFromUs); err != nil {
		return fmt.Errorf("failed to prepare channel commit request: %w", err)
	}

	if err := s.db.CreateTask(ctx, PaymentsTaskPool, "commit-action", channel.Our.Address+"-commit-action",
		"commit-action-"+channel.Our.Address+"-"+fmt.Sprint(channel.InitAt.Unix())+fmt.Sprintf("-%d", channel.LoadSignedState().Body.Seqno),
		db.ActionCommitTask{
			Address:            channel.Our.Address,
			ActionId:           actionId,
			ChannelInitiatedAt: channel.InitAt,
			ForFee:             !doTxOurself,
		}, nil, nil,
	); err != nil {
		return err
	}
	log.Info().Str("address", channel.Our.Address).Bool("ourself", doTxOurself).Str("action", base64.StdEncoding.EncodeToString(actionId)).Msg("commit action task registered")
	return nil
}

var ErrCannotCloseOutgoingVirtual = fmt.Errorf("cannot close outgoing channel")

func (s *Service) CloseDerivative(ctx context.Context, virtualKey ed25519.PublicKey) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	if meta.Outgoing == nil {
		return fmt.Errorf("derivative is not outgoing")
	}
	if meta.Outgoing.Conditional == nil {
		return fmt.Errorf("outgoing derivative conditional is missing")
	}

	linkedMeta, err := s.loadLinkedIncomingDerivativeMeta(ctx, meta)
	if err != nil {
		return err
	}

	ch, err := s.GetActiveChannel(ctx, meta.Outgoing.ChannelAddress)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("onchain channel with target is not active")
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}

	condId := meta.Outgoing.Conditional.Hash()
	_, cond, err := payments.FindConditional(ctx, ch.Our.Data.Conditionals, condId, s)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			log.Debug().Err(err).Str("channel", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(condId)).Msg("derivative not found, nothing to close")
			// idempotency
			return nil
		}
		log.Error().Err(err).Str("channel", ch.Our.Address).Msg("failed to find derivative virtual channel")
		return fmt.Errorf("failed to find derivative virtual channel: %w", err)
	}

	resolve := meta.LastKnownResolve
	if resolve == nil {
		return ErrNoResolveExists
	}

	actStateSlice, err := ch.Our.Data.ActionStates.LoadValue(cond.GetAction().IDCell())
	if err != nil {
		return fmt.Errorf("failed to load active channel state: %w", err)
	}
	actState := actStateSlice.MustToCell()

	_, err = cond.Execute(actState, resolve, nil)
	if err != nil {
		return fmt.Errorf("failed to execute conditional: %w", err)
	}

	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		meta.Status = db.ConditionalStateWantClose
		meta.UpdatedAt = time.Now()
		if err = s.db.UpdateVirtualChannelMeta(ctx, meta); err != nil {
			return fmt.Errorf("failed to update channel in db: %w", err)
		}

		till := cond.GetDeadline()

		// Propagate resolve to the incoming meta.
		// If outgoing settle is already > 0 (our loss), incoming settle is
		// deterministically 0 and does not require oracle access.
		if meta.LastKnownResolve != nil {
			var outResolve conditionals.ResolvableState
			if rErr := payments.LoadState(&outResolve, meta.LastKnownResolve); rErr != nil {
				return fmt.Errorf("failed to parse outgoing derivative resolve: %w", rErr)
			}

			if outResolve.Amount.Sign() > 0 {
				zeroIncoming, zErr := tlb.ToCell(conditionals.ResolvableState{
					Key:    linkedMeta.Key,
					Amount: big.NewInt(0),
					At:     outResolve.At,
				})
				if zErr != nil {
					return fmt.Errorf("failed to build incoming zero resolve: %w", zErr)
				}
				linkedMeta.LastKnownResolve = zeroIncoming
			}
		}

		if linkedMeta.LastKnownResolve == nil && meta.LastKnownResolve != nil {
			if res, ok := cond.(*conditionals.ConditionalResolvable); ok {
				incomingResolve, rErr := s.buildIncomingDerivativeResolve(res, linkedMeta.Key)
				if rErr != nil {
					return fmt.Errorf("failed to build incoming derivative resolve: %w", rErr)
				} else {
					linkedMeta.LastKnownResolve = incomingResolve
				}
			} else {
				return fmt.Errorf("conditional is not derivative resolvable")
			}
		}

		if linkedMeta.LastKnownResolve == nil {
			return fmt.Errorf("linked incoming resolve is not set")
		}

		linkedMeta.Status = db.ConditionalStateWantClose
		linkedMeta.UpdatedAt = time.Now()
		if lErr := s.db.UpdateVirtualChannelMeta(ctx, linkedMeta); lErr != nil {
			return fmt.Errorf("failed to update linked incoming meta: %w", lErr)
		}

		log.Debug().Str("key", base64.StdEncoding.EncodeToString(linkedMeta.Key)).
			Msg("creating task to close conditional")

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-conditional", ch.Our.Address+"-deriv-close",
			"close-conditional-"+ch.Our.Address+"-vc-"+base64.StdEncoding.EncodeToString(condId),
			db.CloseConditionalTask{
				VirtualKey: linkedMeta.Key,
			}, nil, &till,
		); err != nil {
			return fmt.Errorf("failed to create close conditional task: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to execute db tx for close derivative: %w", err)
	}
	s.touchWorker()

	return nil
}

func (s *Service) loadLinkedIncomingDerivativeMeta(ctx context.Context, meta *db.ConditionalMeta) (*db.ConditionalMeta, error) {
	if meta == nil || meta.Outgoing == nil {
		return nil, fmt.Errorf("derivative is not outgoing")
	}
	if len(meta.Outgoing.LinkedKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("outgoing derivative has no linked incoming side")
	}

	linkedMeta, err := s.db.GetVirtualChannelMeta(ctx, meta.Outgoing.LinkedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load linked incoming meta: %w", err)
	}
	if linkedMeta.Incoming == nil {
		return nil, fmt.Errorf("linked derivative meta is not incoming")
	}
	if linkedMeta.Incoming.Conditional == nil {
		return nil, fmt.Errorf("linked incoming derivative conditional is missing")
	}
	if len(linkedMeta.Incoming.LinkedKey) != ed25519.PublicKeySize || !bytes.Equal(linkedMeta.Incoming.LinkedKey, meta.Key) {
		return nil, fmt.Errorf("linked incoming derivative does not point back to outgoing meta")
	}
	if linkedMeta.Incoming.ChannelAddress != meta.Outgoing.ChannelAddress {
		return nil, fmt.Errorf("linked incoming derivative channel mismatch")
	}
	return linkedMeta, nil
}

func (s *Service) CloseConditional(ctx context.Context, virtualKey ed25519.PublicKey) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	return s.closeConditional(ctx, meta)
}

func (s *Service) closeConditional(ctx context.Context, meta *db.ConditionalMeta) error {
	if meta == nil {
		return fmt.Errorf("conditional meta is nil")
	}

	if meta.Incoming == nil {
		if meta.Outgoing != nil {
			return ErrCannotCloseOutgoingVirtual
		}
		return fmt.Errorf("conditional has no incoming channel")
	}

	resolve := meta.LastKnownResolve
	if resolve == nil {
		return ErrNoResolveExists
	}

	ch, err := s.GetActiveChannel(ctx, meta.Incoming.ChannelAddress)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("onchain channel with source is not active")
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}

	condId := meta.Incoming.Conditional.Hash()
	_, cond, err := payments.FindConditional(ctx, ch.Their.Data.Conditionals, condId, s)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			log.Debug().Err(err).Str("channel", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(condId)).Msg("conditional not found, nothing to close")
			// idempotency
			return nil
		}

		log.Error().Err(err).Str("channel", ch.Our.Address).Msg("failed to find virtual channel")
		return fmt.Errorf("failed to find virtual channel: %w", err)
	}

	actStateSlice, err := ch.Their.Data.ActionStates.LoadValue(cond.GetAction().IDCell())
	if err != nil {
		return fmt.Errorf("failed to load active channel state: %w", err)
	}
	actState := actStateSlice.MustToCell()

	till := cond.GetDeadline()

	newActState, err := cond.Execute(actState, resolve, nil)
	if err != nil {
		return fmt.Errorf("failed to execute conditional: %w", err)
	}

	isStateSame := bytes.Equal(newActState.Hash(), actState.Hash())

	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		if err = s.db.ClosePairMeta(ctx, meta.Key, db.ConditionalStateWantClose); err != nil {
			return fmt.Errorf("failed to update virtual channel pair in db: %w", err)
		}
		meta.Status = db.ConditionalStateWantClose
		meta.UpdatedAt = time.Now()

		// if state is equal after exec, no need to uncoop actions
		if !isStateSame {
			// We start uncooperative close at specific moment to have time
			// to commit resolve onchain in case partner is irresponsible.
			// But in the same time we give our partner time to
			till = till.Add(time.Duration(-ch.SafeOnchainClosePeriod) * time.Second)
			minDelay := time.Now().Add(1 * time.Minute)
			if !till.After(minDelay) {
				till = minDelay
			}

			// Creating aggressive onchain close task, for the future,
			// in case we will not be able to communicate with party
			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", ch.Our.Address+"-uncoop",
				"uncooperative-close-"+ch.Our.Address+"-vc-"+base64.StdEncoding.EncodeToString(condId),
				db.ChannelUncooperativeCloseTask{
					Address:              ch.Our.Address,
					CheckCondStillExists: condId,
				}, &till, nil,
			); err != nil {
				log.Warn().Err(err).Str("channel", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(condId)).Msg("failed to create uncooperative close task")
			}
		}

		log.Debug().Str("key", base64.StdEncoding.EncodeToString(meta.Key)).
			Msg("creating task to close conditional to us")

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "close-conditional", ch.Our.Address+"-coop",
			"close-conditional-"+ch.Our.Address+"-vc-"+base64.StdEncoding.EncodeToString(condId),
			db.CloseConditionalTask{
				VirtualKey: meta.Key,
			}, nil, &till,
		); err != nil {
			log.Warn().Err(err).Str("channel", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(condId)).Msg("failed to create close conditional task")
			return fmt.Errorf("failed to create close conditional task: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to execute db tx for close virtual channel: %w", err)
	}
	s.touchWorker()

	log.Info().Err(err).Bool("isStateSame", isStateSame).Str("channel", ch.Our.Address).Str("id", base64.StdEncoding.EncodeToString(condId)).Msg("conditional close task created and will be executed soon")

	return nil
}

func (s *Service) executeCooperativeCommit(ctx context.Context, req *payments.CooperativeCommit, addr *address.Address) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	log.Info().Str("addr", addr.String()).Msg("executing cooperative commit transaction...")

	if err = s.CheckAddrBalance(ctx, addr, "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, addr, msg)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", addr.String()).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("cooperative commit transaction completed")
	return nil
}

func (s *Service) executeCooperativeClose(ctx context.Context, req *payments.CooperativeClose, channel *db.Channel) error {
	msg, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), msg)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("cooperative close transaction completed")

	return nil
}

func (s *Service) executeSignedExternal(ctx context.Context, msg *cell.Cell, addr *address.Address) error {
	msgHash, err := s.ton.SendWaitExternalMessage(ctx, addr, msg)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", addr.String()).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("signed external executed")

	return nil
}

func (s *Service) RequestCooperativeClose(ctx context.Context, channelAddr string) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	ch, err := s.GetActiveChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if _, _, _, err = s.getCooperativeCloseRequest(ctx, ch, ch.WeLeft); err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	return s.db.Transaction(ctx, func(ctx context.Context) error {
		ch.AcceptingActions = false
		if err = s.db.UpdateChannel(ctx, ch); err != nil {
			return fmt.Errorf("failed to update channel: %w", err)
		}

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "cooperative-close", ch.Our.Address,
			"cooperative-close-"+ch.Our.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelCooperativeCloseTask{
				Address:            ch.Our.Address,
				ChannelInitiatedAt: ch.InitAt,
			}, nil, nil,
		); err != nil {
			return fmt.Errorf("failed to create cooperative close task: %w", err)
		}

		after := time.Now().Add(5 * time.Minute)
		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "uncooperative-close", ch.Our.Address+"-uncoop",
			"uncooperative-close-"+ch.Our.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelUncooperativeCloseTask{
				Address:            ch.Our.Address,
				ChannelInitiatedAt: &ch.InitAt,
			}, &after, nil,
		); err != nil {
			log.Error().Err(err).Str("channel", ch.Our.Address).Msg("failed to create uncooperative close task")
		}
		return nil
	})
}

func (s *Service) RequestWithdrawToAddr(ctx context.Context, channelAddr string, addr *address.Address, cc *payments.CoinConfig, amount *big.Int) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	ch, err := s.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	return s.requestWithdrawToAddr(ctx, ch, addr, cc, amount)
}

func (s *Service) requestWithdrawToAddr(ctx context.Context, ch *db.Channel, addr *address.Address, cc *payments.CoinConfig, amount *big.Int) error {
	var msg payments.WalletMessage
	if cc.JettonClient != nil {
		payload, err := buildJettonTransferPayload(addr, addr, cc.MustAmount(amount), tlb.ZeroCoins, nil, nil)
		if err != nil {
			return fmt.Errorf("failed to build jetton transfer payload: %w", err)
		}

		jw, err := cc.JettonClient.GetWalletAddress(ctx, address.MustParseAddr(ch.Our.Address))
		if err != nil {
			return fmt.Errorf("failed to get jetton wallet address: %w", err)
		}

		msg = payments.WalletMessage{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled: true,
				Bounce:      true,
				DstAddr:     jw,
				Amount:      tlb.MustFromTON("0.05"),
				Body:        payload,
			},
		}
	} else if cc.BalanceID != payments.GetTONBalanceID() { // ec
		ec := payments.GetECFromBalanceID(cc.BalanceID)
		ecs := cell.NewDict(32)
		if err := ecs.SetIntKey(big.NewInt(int64(ec)), cell.BeginCell().MustStoreBigVarUInt(amount, 32).EndCell()); err != nil {
			return fmt.Errorf("failed to set ec amount: %w", err)
		}

		msg = payments.WalletMessage{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled:     true,
				Bounce:          addr.IsBounceable(),
				DstAddr:         addr,
				ExtraCurrencies: ecs,
				Amount:          tlb.MustFromTON("0.02"),
				Body:            cell.BeginCell().EndCell(),
			},
		}
	} else {
		msg = payments.WalletMessage{
			Mode: 1 + 2,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled: true,
				Bounce:      addr.IsBounceable(),
				DstAddr:     addr,
				Amount:      tlb.MustFromNano(amount, 9),
				Body:        cell.BeginCell().EndCell(),
			},
		}
	}

	return s.requestSignedMessage(ctx, ch, []payments.WalletMessage{msg})
}

func (s *Service) requestSignedMessage(ctx context.Context, ch *db.Channel, messages []payments.WalletMessage) error {
	if len(messages) == 0 {
		return fmt.Errorf("no messages provided")
	}

	packed, err := payments.PackOutActions(messages)
	if err != nil {
		return fmt.Errorf("failed to pack actions: %w", err)
	}

	if len(ch.Our.PendingOnchainTransfers) > 0 {
		return fmt.Errorf("channel already has pending onchain transfer, wait for completion")
	}

	if err = s.db.CreateTask(ctx, PaymentsTaskPool, "request-tx-external", ch.Our.Address+"-tx-external",
		"request-tx-external-"+ch.Our.Address+"-"+fmt.Sprint(ch.Our.LatestWalletSeqno),
		db.RequestExternalTxTask{
			ChannelAddress: ch.Our.Address,
			PackedMessages: packed,
			WalletSeqno:    ch.Our.LatestWalletSeqno,
		}, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create request-tx-external task: %w", err)
	}
	return nil
}

var ErrNothingToCommit = fmt.Errorf("nothing to commit")

func (s *Service) getCommitRequest(ctx context.Context, channel *db.Channel, actionId []byte, executeByUs bool, feeFromUs *bool) (*payments.CooperativeCommit, *payments.PendingMessageInfo, *payments.PendingMessageInfo, []byte, error) {
	var actStateOur, actStateTheir *cell.Cell = nil, nil

	if !channel.Our.ActiveOnchain || !channel.Their.ActiveOnchain {
		return nil, nil, nil, nil, fmt.Errorf("both sides contracts must be active onchain to commit")
	}

	var ourTx, theirTx *payments.PendingMessageInfo
	var ourReq payments.CooperativeCommit
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.Seqno = channel.LoadSignedState().Body.Seqno + 1

	if executeByUs {
		// we execute onchain
		ourReq.Signed.FromA = channel.WeLeft
	} else {
		ourReq.Signed.FromA = !channel.WeLeft
	}

	if actionId != nil {
		act, err := s.ResolveAction(ctx, actionId)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to resolve action: %w", err)
		}

		actStateTheirLd, err := channel.Their.Data.ActionStates.LoadValue(act.IDCell())
		if err != nil && !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, nil, nil, fmt.Errorf("failed to load their action state for action %s: %w", hex.EncodeToString(actionId), err)
		} else if err == nil {
			actStateTheir = actStateTheirLd.MustToCell()
		}

		actStateOurLd, err := channel.Our.Data.ActionStates.LoadValue(act.IDCell())
		if err != nil && !errors.Is(err, cell.ErrNoSuchKeyInDict) {
			return nil, nil, nil, nil, fmt.Errorf("failed to load our action state for action %s: %w", hex.EncodeToString(actionId), err)
		} else if err == nil {
			actStateOur = actStateOurLd.MustToCell()
		}

		if actStateOur == nil && actStateTheir == nil {
			return nil, nil, nil, nil, ErrNothingToCommit
		}

		if actStateOur != nil {
			balance, err := channel.CalcBalance(ctx, false, s)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
			}

			actStateOur, ourTx, err = act.PrepareExecuteState(actStateOur, address.MustParseAddr(channel.Their.Address), ourReq.Signed.Seqno, feeFromUs != nil && *feeFromUs, balance)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to prepare our execute state: %w", err)
			}
		}

		if actStateTheir != nil {
			balance, err := channel.CalcBalance(ctx, true, s)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
			}

			actStateTheir, theirTx, err = act.PrepareExecuteState(actStateTheir, address.MustParseAddr(channel.Our.Address), ourReq.Signed.Seqno, feeFromUs != nil && !*feeFromUs, balance)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("failed to prepare their execute state: %w", err)
			}
		}

		ourReq.Signed.Action = &payments.CooperativeCommitAction{
			StateA: actStateOur,
			StateB: actStateTheir,
			Code:   act.Serialize(),
		}
	}

	if !channel.WeLeft && ourReq.Signed.Action != nil {
		ourReq.Signed.Action.StateA, ourReq.Signed.Action.StateB = ourReq.Signed.Action.StateB, ourReq.Signed.Action.StateA
	}
	dataCell, err := tlb.ToCell(ourReq.Signed)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to serialize body to cell: %w", err)
	}

	signature := dataCell.Sign(s.key)
	ourReq.SignatureA.Value = signature
	ourReq.SignatureB.Value = make([]byte, 64)

	if !channel.WeLeft {
		ourReq.SignatureA.Value, ourReq.SignatureB.Value = ourReq.SignatureB.Value, ourReq.SignatureA.Value
	}

	return &ourReq, ourTx, theirTx, signature, nil
}

func (s *Service) getCooperativeCloseRequest(ctx context.Context, channel *db.Channel, fromA bool) (*payments.CooperativeClose, *cell.Cell, []byte, error) {
	if len(channel.Our.PendingOnchainTransfers) > 0 {
		return nil, nil, nil, fmt.Errorf("some our transfers are pending")
	}
	if len(channel.Their.PendingOnchainTransfers) > 0 {
		return nil, nil, nil, fmt.Errorf("some their transfers are pending")
	}

	allOur, err := channel.Our.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load our cond dict: %w", err)
	}

	for _, kv := range allOur {
		vch, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.GetDeadline().After(time.Now()) {
			return nil, nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	allTheir, err := channel.Their.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load their cond dict: %w", err)
	}

	for _, kv := range allTheir {
		vch, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.GetDeadline().After(time.Now()) {
			return nil, nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	allOurAct, err := channel.Our.Data.ActionStates.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load our actions dict: %w", err)
	}

	for _, v := range allOurAct {
		id := v.Key.MustLoadSlice(256)
		act, err := s.ResolveAction(ctx, id)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to resolve our action %s: %w", hex.EncodeToString(id), err)
		}

		can, err := act.CheckCanRemove(channel.Our.LatestCommitedSeqno, v.Value.MustToCell())
		if err != nil {
			return nil, nil, nil, err
		}

		if !can {
			return nil, nil, nil, fmt.Errorf("our action %s cannot be removed, state requires commit", hex.EncodeToString(id))
		}
	}

	allTheirAct, err := channel.Their.Data.ActionStates.LoadAll()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load our actions dict: %w", err)
	}

	for _, v := range allTheirAct {
		id := v.Key.MustLoadSlice(256)
		act, err := s.ResolveAction(ctx, id)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to resolve their action %s: %w", hex.EncodeToString(id), err)
		}

		can, err := act.CheckCanRemove(channel.Their.LatestCommitedSeqno, v.Value.MustToCell())
		if err != nil {
			return nil, nil, nil, err
		}

		if !can {
			return nil, nil, nil, fmt.Errorf("their action %s cannot be removed, state requires commit", hex.EncodeToString(id))
		}
	}

	var ourReq payments.CooperativeClose
	ourReq.Signed.ChannelID = channel.ID
	ourReq.Signed.FromA = fromA
	ourReq.Signed.Seqno = channel.LoadSignedState().Body.Seqno + 1

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

	och, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status != payments.ChannelStatusOpen {
		log.Debug().Str("address", channel.Our.Address).
			Msg("uncooperative close already started or not required")
		return nil
	}

	log.Info().Str("address", channel.Our.Address).
		Msg("starting uncooperative close")

	msg := payments.UncoopCloseMsg{}
	msg.Signed.ChannelID = channel.ID
	msg.Signed.State = channel.LoadSignedState()

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	msgCell, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("uncooperative close transaction sent")

	return nil
}

func (s *Service) challengeChannelState(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		return nil
	}

	och, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status == payments.ChannelStatusAwaitingFinalization ||
		och.Status == payments.ChannelStatusUninitialized ||
		och.Status == payments.ChannelStatusSettlingConditionals {
		// no more time to challenge
		return nil
	}

	msg := payments.UncoopCloseMsg{}
	msg.Signed.ChannelID = channel.ID
	msg.Signed.State = channel.LoadSignedState()

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	msgCell, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message to cell: %w", err)
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("challenge channel state transaction completed")

	return nil
}

func (s *Service) finishUncooperativeChannelClose(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	och, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
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

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), msgCell)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}

	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("finish uncooperative close transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) settleChannelConditionals(ctx context.Context, channelAddr string) error {
	const conditionsPerMessage = 30
	type settleConditionalMessage struct {
		Message        *cell.Cell
		ExpectedSender *address.Address
	}

	log.Info().Str("address", channelAddr).Msg("settling conditionals")

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		return nil
	}

	if channel.Their.Data.ActionStates.IsEmpty() {
		log.Info().Str("address", channel.Our.Address).Str("their_address", channel.Their.Address).
			Msg("cannot settle, their action states are empty")
		return nil
	}

	och, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status >= payments.ChannelStatusExecutingActions ||
		och.Status == payments.ChannelStatusUninitialized {
		// no more time to settle
		return nil
	}

	msg := payments.SettleMsg{}
	msg.Signed.ChannelID = channel.ID
	msg.Signed.ToSettle = cell.NewDict(256)

	// TODO: get all conditions and make inputs for known
	all, err := channel.Their.Data.Conditionals.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load their conditions dict: %w", err)
	}

	var condMessages []settleConditionalMessage
	var resolved int
	var pending int

	addMessage := func(data *payments.SettleMsg, condProofBuilder, actProofBuilder *cell.MerkleProofBuilder) error {
		if data.Signed.ToSettle == nil || data.Signed.ToSettle.IsEmpty() {
			return nil
		}

		condDictProof, err := condProofBuilder.CreateProof()
		if err != nil {
			log.Warn().Err(err).Msg("failed to find proof path for conditionals dict")
			return err
		}

		actDictProof, err := actProofBuilder.CreateProof()
		if err != nil {
			log.Warn().Err(err).Msg("failed to find proof path for action states dict")
			return err
		}

		data.Signed.ConditionalsProof = condDictProof
		data.Signed.ActionsInputProof = actDictProof

		dataCell, err := tlb.ToCell(data.Signed)
		if err != nil {
			return fmt.Errorf("failed to serialize body to cell: %w", err)
		}
		data.Signature.Value = dataCell.Sign(s.key)

		msgCell, err := tlb.ToCell(data)
		if err != nil {
			return fmt.Errorf("failed to serialize message to cell: %w", err)
		}

		condMessages = append(condMessages, settleConditionalMessage{
			Message:        msgCell,
			ExpectedSender: data.Signed.ExpectedSender,
		})
		return nil
	}

	updatedState := channel.Their.Data.Conditionals.Copy()
	updatedActions := channel.Their.Data.ActionStates.Copy()

	condNum := 0
	condProofBuilder := cell.NewMerkleProofBuilder(channel.Their.Data.Conditionals.AsCell())
	actProofBuilder := cell.NewMerkleProofBuilder(channel.Their.Data.ActionStates.AsCell())
	flushMessage := func() error {
		if condNum == 0 {
			return nil
		}

		if err := addMessage(&msg, condProofBuilder, actProofBuilder); err != nil {
			log.Warn().Err(err).Msg("failed to add settle message")
			return err
		}

		condNum = 0
		channel.Their.Data.Conditionals = updatedState.Copy()
		channel.Their.Data.ActionStates = updatedActions.Copy()
		condProofBuilder = cell.NewMerkleProofBuilder(channel.Their.Data.Conditionals.AsCell())
		actProofBuilder = cell.NewMerkleProofBuilder(channel.Their.Data.ActionStates.AsCell())
		msg.Signed.ExpectedSender = nil
		msg.Signed.ToSettle = cell.NewDict(256)

		return nil
	}
	for _, kv := range all {
		if kv.Value.RefsNum() == 0 && kv.Value.BitsLeft() == 0 {
			// executed
			continue
		}
		pending++
		key := kv.Key.MustToCell()

		vch, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
		if err != nil {
			log.Warn().Err(err).Msg("failed to parse virtual channel")
			continue
		}

		meta, err := s.db.GetVirtualChannelMeta(ctx, vch.GetKey())
		if err != nil {
			log.Warn().Err(err).Msg("failed to get virtual channel meta")
			continue
		}

		resolve := meta.LastKnownResolve
		var expectedSender *address.Address
		if drv, ok := vch.(*conditionals.ConditionalResolvable); ok && drv.ResolverAddr != nil {
			expectedSender = drv.ResolverAddr
			if resolve == nil {
				resolve, err = s.prepareDerivativeConditionalResolve(ctx, channel, meta, drv)
				if err != nil {
					log.Warn().Err(err).Str("key", base64.StdEncoding.EncodeToString(drv.GetKey())).Msg("failed to prepare derivative settle resolve")
					continue
				}
			}
		}

		if resolve != nil {
			if expectedSender != nil && condNum > 0 {
				if err := flushMessage(); err != nil {
					return err
				}
			}
			if expectedSender != nil {
				msg.Signed.ExpectedSender = expectedSender
			}

			actionState, err := actProofBuilder.Root().AsDict(channel.Their.Data.ActionStates.GetKeySize()).LoadValue(vch.GetAction().IDCell())
			if err != nil {
				log.Warn().Err(err).Msg("failed to find proof path for action state")
				continue
			}
			actionStateCell := actionState.MustToCell()
			if err = payments.MarkCellUsedRecursive(actionStateCell); err != nil {
				log.Warn().Err(err).Msg("failed to mark full action state proof")
				continue
			}

			updatedActionState, err := vch.Execute(actionStateCell, resolve, map[string]*payments.LockedDepositInfo{})
			if err != nil {
				log.Warn().Err(err).Msg("failed to execute conditional")
				continue
			}

			if err = msg.Signed.ToSettle.Set(key, resolve); err != nil {
				log.Warn().Err(err).Msg("failed to store known virtual channel state in request")
				continue
			}

			condValue, err := condProofBuilder.Root().AsDict(channel.Their.Data.Conditionals.GetKeySize()).LoadValue(key)
			if err != nil {
				log.Warn().Err(err).Msg("failed to find proof path for conditionals dict")
				continue
			}
			if err = payments.MarkCellUsedRecursive(condValue.MustToCell()); err != nil {
				log.Warn().Err(err).Msg("failed to mark full conditional proof")
				continue
			}
			condNum++

			// replace value to empty cell, we need 2 dictionaries: before and after, to save and continue
			if err = updatedState.Set(key, cell.BeginCell().EndCell()); err != nil {
				log.Warn().Err(err).Msg("failed to replace conditional to empty")
				continue
			}

			if err = updatedActions.Set(vch.GetAction().IDCell(), updatedActionState); err != nil {
				log.Warn().Err(err).Msg("failed to replace action state to executed")
				continue
			}

			if expectedSender != nil || condNum == conditionsPerMessage {
				if err := flushMessage(); err != nil {
					return err
				}
			}
			resolved++
		}
	}

	if err := flushMessage(); err != nil {
		log.Warn().Err(err).Msg("failed to add settle last message")
		return err
	}

	if resolved != pending {
		log.Warn().Int("resolved", resolved).Int("pending", pending).Msg("not all conditions resolved")
		// return fmt.Errorf("cannot settle conditionals partially: resolved=%d pending=%d", resolved, pending)
	}

	expectedActionsHash := updatedActions.AsCell().Hash()
	log.Info().Str("address", channel.Our.Address).Int("steps", len(condMessages)).Msg("calculated settle steps")

	err = s.db.Transaction(ctx, func(ctx context.Context) error {
		for i, message := range condMessages {
			var executeAfter *time.Time
			if message.ExpectedSender != nil {
				at := time.Now().Add(time.Duration(derivativeResolverQuarantineDuration+1) * time.Second)
				executeAfter = &at
			}

			if err = s.db.CreateTask(ctx, PaymentsTaskPool, "settle-step", channel.Our.Address+"-settle",
				"settle-"+channel.Our.Address+"-"+fmt.Sprint(i),
				db.SettleConditionalStepTask{
					Step:               i,
					Address:            channel.Our.Address,
					Message:            message.Message,
					ChannelInitiatedAt: &channel.InitAt,
				}, executeAfter, nil,
			); err != nil {
				log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to create settle step task")
			}

			log.Info().Str("address", channel.Our.Address).Int("step", i).Msg("settle step created")
		}

		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "settle-fin", channel.Our.Address+"-settle",
			"settle-"+channel.Our.Address+"-finish",
			db.FinalizeSettleTask{
				ChannelAddress:      channel.Our.Address,
				ExpectedActionsHash: expectedActionsHash,
			}, nil, nil,
		); err != nil {
			log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to create finalize settle task")
		}

		log.Info().Str("address", channel.Our.Address).Msg("settle conditionals tasks created")

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to add tasks: %w", err)
	}

	return nil
}

func (s *Service) settleChannelActions(ctx context.Context, channelAddr string) error {
	log.Info().Str("address", channelAddr).Msg("settling actions")

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Their.Data.ActionStates.IsEmpty() {
		log.Info().Str("address", channel.Our.Address).Str("their_address", channel.Their.Address).
			Msg("nothing to settle, their actions are empty")
		return nil
	}

	och, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if och.Status == payments.ChannelStatusAwaitingFinalization ||
		och.Status == payments.ChannelStatusUninitialized {
		log.Warn().Str("address", channel.Our.Address).Str("their_address", channel.Their.Address).Msg("cannot settle, channel is not active anymore")
		// no more time to settle
		return nil
	}

	if och.Status != payments.ChannelStatusExecutingActions {
		return fmt.Errorf("channel is not in executing actions state yet")
	}

	if !channel.Our.IsSettlementFinalized {
		return fmt.Errorf("conditionals settle is not yet finalized")
	}

	msg := payments.ExecuteActionsMsg{}
	msg.Signed.ChannelID = channel.ID

	allTheir, err := channel.Their.Data.ActionStates.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load their actions dict: %w", err)
	}

	var messages []*cell.Cell

	for _, kv := range allTheir {
		if kv.Value.RefsNum() == 0 && kv.Value.BitsLeft() == 0 {
			// executed
			continue
		}

		key := kv.Key.MustToCell()
		actId := kv.Key.MustLoadSlice(256)

		act, err := s.ResolveAction(ctx, actId)
		if err != nil {
			log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to resolve action")
			continue
		}

		theirProofBuilder := cell.NewMerkleProofBuilder(channel.Their.Data.ActionStates.AsCell())
		theirActionState, err := theirProofBuilder.Root().AsDict(channel.Their.Data.ActionStates.GetKeySize()).LoadValue(key)
		if err != nil {
			log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to find proof path for their action")
			continue
		}
		if err = payments.MarkCellUsedRecursive(theirActionState.MustToCell()); err != nil {
			log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to mark full proof for their action")
			continue
		}

		ourProofBuilder := cell.NewMerkleProofBuilder(channel.Our.Data.ActionStates.AsCell())
		ourActionState, err := ourProofBuilder.Root().AsDict(channel.Our.Data.ActionStates.GetKeySize()).LoadValue(key)
		if err != nil {
			if !errors.Is(err, cell.ErrNoSuchKeyInDict) {
				log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to find proof path for our action")
				continue
			}
		} else {
			if err = payments.MarkCellUsedRecursive(ourActionState.MustToCell()); err != nil {
				log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to mark full proof for our action")
				continue
			}
		}

		theirDictProof, err := theirProofBuilder.CreateProof()
		if err != nil {
			log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to create proof for their actions")
			return err
		}

		if !channel.Our.Data.ActionStates.IsEmpty() {
			ourDictProof, err := ourProofBuilder.CreateProof()
			if err != nil {
				log.Warn().Err(err).Str("id", base64.StdEncoding.EncodeToString(actId)).Msg("failed to create proof for our actions")
				return err
			}
			msg.Signed.TheirActionsInputProof = ourDictProof
		}

		msg.Signed.Action = act.Serialize()
		msg.Signed.OurActionsInputProof = theirDictProof // it is swapped because executed on party contract

		// replace value to empty cell, we need 2 dictionaries: before and after, to save and continue
		if err = channel.Their.Data.ActionStates.Set(key, cell.BeginCell().EndCell()); err != nil {
			log.Warn().Err(err).Msg("failed to replace their action")
			continue
		}

		dataCell, err := tlb.ToCell(msg.Signed)
		if err != nil {
			return fmt.Errorf("failed to serialize body to cell: %w", err)
		}
		msg.Signature.Value = dataCell.Sign(s.key)

		msgCell, err := tlb.ToCell(msg)
		if err != nil {
			return fmt.Errorf("failed to serialize message to cell: %w", err)
		}

		messages = append(messages, msgCell)
	}

	pendingActions := 0
	for _, kv := range allTheir {
		if kv.Value.RefsNum() == 0 && kv.Value.BitsLeft() == 0 {
			continue
		}
		pendingActions++
	}
	if pendingActions == 0 {
		log.Warn().Msg("nothing to commit")
		return nil
	}

	if len(messages) != pendingActions {
		return fmt.Errorf("cannot settle actions partially: resolved=%d pending=%d", len(messages), pendingActions)
	}

	log.Info().Str("address", channel.Our.Address).Int("steps", len(messages)).Msg("calculated settle action steps")

	for i := 0; i < len(messages); i++ {
		if err = s.db.CreateTask(ctx, PaymentsTaskPool, "settle-act-step", channel.Our.Address+"-act-settle",
			"act-settle-"+channel.Our.Address+"-"+fmt.Sprint(i),
			db.SettleActionStepTask{
				Step:               i,
				Address:            channel.Our.Address,
				Message:            messages[i],
				ChannelInitiatedAt: &channel.InitAt,
			}, nil, nil,
		); err != nil {
			log.Error().Err(err).Str("channel", channel.Our.Address).Msg("failed to create settle action step task")
		}

		log.Info().Str("address", channel.Our.Address).Int("step", i).Msg("settle action step created")
	}

	return nil
}

func (s *Service) executeSettleActionStep(ctx context.Context, channelAddr string, executeMsg *cell.Cell, step int) error {
	log.Info().Str("address", channelAddr).Int("step", step).Msg("executing settle action step...")

	var mm payments.ExecuteActionsMsg
	if err := tlb.Parse(&mm, executeMsg); err != nil {
		return fmt.Errorf("failed to load execute message: %w", err)
	}

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		log.Warn().Str("address", channelAddr).Msg("channel is inactive, skipping action step")
		return nil
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	ourC, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channel.Our.Address), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	theirC, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channel.Their.Address), true, channel.Their.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	if theirC.Storage.Quarantine == nil {
		return fmt.Errorf("their channel is not quarantined")
	}

	ourProofHash := mm.Signed.OurActionsInputProof.MustBeginParse().MustLoadSlice(8 + 256)[1:]
	if !bytes.Equal(theirC.Storage.Quarantine.ActionsToExecuteHash, ourProofHash) {
		return fmt.Errorf("their proof hash mismatch, expected %x, got %x", theirC.Storage.Quarantine.ActionsToExecuteHash, ourProofHash)
	}

	if mm.Signed.TheirActionsInputProof != nil {
		theirProofHash := mm.Signed.TheirActionsInputProof.MustBeginParse().MustLoadSlice(8 + 256)[1:]
		if !bytes.Equal(theirC.Storage.Quarantine.TheirState.ActionStatesHash, theirProofHash) {
			return fmt.Errorf("our proof hash mismatch, expected %x, got %x", theirC.Storage.Quarantine.TheirState.ActionStatesHash, theirProofHash)
		}
	}

	msg := payments.ProxyExecuteActionsMsg{}
	msg.Signed.ChannelID = ourC.Storage.ChannelID
	msg.Signed.WalletSeqno = ourC.Storage.WalletSeqno
	msg.Signed.Msg = mm

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize to sign body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	message, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize wrap body to cell: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), message)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}
	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Int("step", step).Msg("settle conditions step transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) executeSettleStep(ctx context.Context, channelAddr string, rawMessage *cell.Cell, step int) error {
	log.Info().Str("address", channelAddr).Int("step", step).Msg("executing settle step...")

	var msg payments.SettleMsg
	if err := tlb.Parse(&msg, rawMessage); err != nil {
		return fmt.Errorf("failed to load execute message: %w", err)
	}

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		return nil
	}

	c, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channel.Our.Address), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}

	msg.Signed.WalletSeqno = c.Storage.WalletSeqno

	dataCell, err := tlb.ToCell(msg.Signed)
	if err != nil {
		return fmt.Errorf("failed to serialize to sign body to cell: %w", err)
	}
	msg.Signature.Value = dataCell.Sign(s.key)

	message, err := tlb.ToCell(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize wrap body to cell: %w", err)
	}

	if msg.Signed.ExpectedSender != nil {
		acc, err := s.ton.GetAccount(ctx, msg.Signed.ExpectedSender, channel.Our.LastProcessedTxAt)
		if err != nil {
			return fmt.Errorf("failed to get derivative resolver state: %w", err)
		}
		if acc == nil || !acc.HasState || acc.Data == nil {
			return ErrStillPending
		}

		storage, err := condcontracts.LoadDerivativeStorage(acc.Data)
		if err != nil {
			return fmt.Errorf("failed to parse derivative resolver storage: %w", err)
		}
		if storage.ExitAt == 0 {
			return ErrStillPending
		}

		if err = s.CheckWalletBalance(ctx, "", tlb.MustFromTON(derivativeResolverProxyAmount)); err != nil {
			return fmt.Errorf("failed to check wallet balance: %w", err)
		}

		target, proxiedBody, err := prepareDerivativeSettleProxyMessage(c, message, msg.Signed.ExpectedSender)
		if err != nil {
			return fmt.Errorf("failed to build derivative settle proxy message: %w", err)
		}

		msgHash, err := s.wallet.DoTransactionMany(ctx, "Proxy derivative settle", []WalletMessage{{
			To:     target,
			Amount: tlb.MustFromTON(derivativeResolverProxyAmount),
			Body:   proxiedBody,
		}})
		if err != nil {
			return fmt.Errorf("failed to send proxy settle transaction: %w", err)
		}
		log.Info().Str("addr", channel.Our.Address).Str("resolver", target.String()).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Int("step", step).Msg("settle conditions step transaction proxied via resolver")
		return nil
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), message)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}
	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Int("step", step).Msg("settle conditions step transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) executeSettleFinalize(ctx context.Context, channelAddr string, actionsHash []byte) error {
	log.Info().Str("address", channelAddr).Msg("executing finalize settle conditionals...")

	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Status == db.ChannelStateInactive {
		return nil
	}

	if err = s.CheckAddrBalance(ctx, address.MustParseAddr(channel.Our.Address), "", tlb.ZeroCoins); err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	c, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channel.Our.Address), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}
	if c.Storage.Quarantine == nil || c.Storage.Quarantine.TheirState == nil {
		return ErrStillPending
	}
	if c.Storage.Quarantine.OurSettlementFinalized {
		return nil
	}
	if !bytes.Equal(c.Storage.Quarantine.TheirState.ActionStatesHash, actionsHash) {
		return ErrStillPending
	}

	message, err := c.PrepareFinalizeSettleMessage(s.key, actionsHash)
	if err != nil {
		return fmt.Errorf("failed to prepare finalize settle message: %w", err)
	}

	msgHash, err := s.ton.SendWaitExternalMessage(ctx, address.MustParseAddr(channel.Our.Address), message)
	if err != nil {
		return fmt.Errorf("failed to send external message: %w", err)
	}
	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Msg("finalize settle conditionals transaction completed")

	return nil
}

func (s *Service) ExecuteTopup(ctx context.Context, channelAddr, balanceId string, amount tlb.Coins, unlockBalanceControlOnDone bool) error {
	log.Info().Str("balance_id", balanceId).Str("address", channelAddr).Msg("executing topup...")

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

	cc, err := s.ResolveCoinConfig(balanceId)
	if err != nil {
		return fmt.Errorf("failed to resolve coin config: %w", err)
	}

	activationAmount := big.NewInt(0)
	if !channel.Our.ActiveOnchain {
		activationAmount.Mul(tlb.MustFromTON(s.cfg.ReplicationMessageAttachAmount).Nano(), big.NewInt(4))
		activationAmount.Add(activationAmount, tlb.MustFromTON("0.05").Nano())
	}
	amtTonFeeToCheck := tlb.FromNanoTON(new(big.Int).Add(tlb.MustFromTON("0.09").Nano(), activationAmount))

	if cc.BalanceID == payments.GetTONBalanceID() {
		toCheck, err := tlb.FromNano(new(big.Int).Add(amount.Nano(), amtTonFeeToCheck.Nano()), 9)
		if err != nil {
			return fmt.Errorf("failed to convert amount to nano: %w", err)
		}

		if err = s.CheckWalletBalance(ctx, "", toCheck); err != nil {
			return fmt.Errorf("failed to check ton balance: %w", err)
		}
	} else {
		if err = s.CheckWalletBalance(ctx, "", amtTonFeeToCheck); err != nil {
			return fmt.Errorf("failed to check ton balance: %w", err)
		}

		if err = s.CheckWalletBalance(ctx, cc.BalanceID, amount); err != nil {
			return fmt.Errorf("failed to check coin balance: %w", err)
		}
	}

	var messages []WalletMessage

	if !channel.Our.ActiveOnchain {
		var code *cell.Cell
		for _, cd := range payments.PaymentChannelCodes {
			if bytes.Equal(cd.Hash(), channel.CodeHash) {
				code = cd
				break
			}
		}

		if code == nil {
			return fmt.Errorf("failed to find channel code")
		}

		state := &tlb.StateInit{
			Data: channel.InitialData,
			Code: code,
		}

		// deploy a contract or activate it
		messages = append(messages, WalletMessage{
			Amount:    tlb.FromNanoTON(activationAmount),
			Body:      channel.InitMessageBody,
			StateInit: state,
		})
	}

	if cc.JettonClient != nil {
		jw, err := cc.JettonClient.GetWalletAddress(ctx, s.wallet.WalletAddress())
		if err != nil {
			return fmt.Errorf("failed to get jetton wallet: %w", err)
		}

		tp, err := buildJettonTransferPayload(address.MustParseAddr(channelAddr), s.wallet.WalletAddress(), amount, tlb.MustFromTON("0.001"), cell.BeginCell().EndCell(), nil)
		if err != nil {
			return fmt.Errorf("failed to build transfer payload: %w", err)
		}

		messages = append(messages, WalletMessage{
			To:     jw,
			Amount: tlb.MustFromTON("0.05"),
			Body:   tp,
		})
	} else if cc.BalanceID != payments.GetTONBalanceID() {
		messages = append(messages, WalletMessage{
			To:     address.MustParseAddr(channelAddr),
			Amount: tlb.MustFromTON("0.001"),
			EC: map[uint32]tlb.Coins{
				payments.GetECFromBalanceID(cc.BalanceID): amount,
			},
		})
	} else {
		// add ton accept fee
		toSend, err := tlb.FromNano(new(big.Int).Add(amount.Nano(), tlb.MustFromTON("0.001").Nano()), 9)
		if err != nil {
			return fmt.Errorf("failed to convert amount to nano: %w", err)
		}

		messages = append(messages, WalletMessage{
			To:     address.MustParseAddr(channelAddr),
			Amount: toSend,
		})
	}

	reason := "Channel balance top up"
	if !channel.Our.ActiveOnchain {
		reason += " with channel activation"
	}

	startedAt := time.Now()
	msgHash, err := s.wallet.DoTransactionMany(ctx, reason, messages)
	if err != nil {
		return fmt.Errorf("failed to send internal messages to channel: %w", err)
	}

	log.Info().Str("addr", channel.Our.Address).Str("hash", base64.StdEncoding.EncodeToString(msgHash)).Bool("with_deploy", !channel.Our.ActiveOnchain).Msg("topup transaction completed")

	if unlockBalanceControlOnDone {
		// TODO: atomic with sending?

		tryTill := startedAt.Add(time.Minute * 15)
		err = s.db.CreateTask(ctx, PaymentsTaskPool, "wait-deposit-completion", channel.Our.Address+"-wait-deposit",
			"wait-deposit-"+base64.StdEncoding.EncodeToString(msgHash),
			db.WaitDepositCompletionTask{
				ChannelAddress:       channel.Our.Address,
				BalanceID:            balanceId,
				UnlockBalanceControl: unlockBalanceControlOnDone,
				MsgHash:              msgHash,
				FromAddress:          s.wallet.WalletAddress().String(),
				StartedAt:            startedAt,
			}, nil, &tryTill,
		)
		if err != nil {
			return fmt.Errorf("failed to create wait task: %w", err)
		}
		s.touchWorker()
	}

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) validateOutMessages(ctx context.Context, channel *db.Channel, out *cell.Cell, isTheir bool) (*payments.PendingMessageInfo, map[string]bool, error) {
	msgs, err := payments.UnpackOutActions(out)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unpack out-actions: %w", err)
	}

	if len(msgs) == 0 {
		return nil, nil, fmt.Errorf("no messages")
	}

	balances, err := channel.CalcBalance(ctx, isTheir, s)
	if err != nil {
		return nil, nil, err
	}

	side, otherSide := &channel.Our, &channel.Their
	if isTheir {
		side, otherSide = otherSide, side
	}

	jettons := make(map[string]*payments.BalanceInfo)
	for _, info := range balances {
		if info.CoinConfig.JettonClient == nil {
			continue
		}

		// TODO: cache jetton addresses
		wa, err := info.CoinConfig.JettonClient.GetWalletAddress(ctx, address.MustParseAddr(side.Address))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get jetton wallet address: %w", err)
		}

		// map jetton wallets to balances to detect jetton transactions
		jettons[hex.EncodeToString(wa.Data())] = info
	}

	pending := payments.PendingMessageInfo{
		Amounts:              map[string]*big.Int{},
		CompletionBodyPrefix: nil,
		CompletionAddress:    "",
		LimitDepth:           4, // enough for jetton deduct
	}

	var lowOnchain = map[string]bool{}
	otherAddr := address.MustParseAddr(otherSide.Address)
	for _, m := range msgs {
		if m.InternalMessage.DstAddr.Equals(otherAddr) {
			return nil, nil, fmt.Errorf("direct transactions to party contract are not allowed, use commits")
		}
		if m.Mode > 3 {
			return nil, nil, fmt.Errorf("message mode is not allowed")
		}

		tonBalance := balances[payments.GetTONBalanceID()]
		if tonBalance != nil {
			tonBalance.Onchain.Sub(tonBalance.Onchain, m.InternalMessage.Amount.Nano())
			// reserve for fee
			tonBalance.Onchain.Sub(tonBalance.Onchain, tlb.MustFromTON("0.1").Nano())

			// check available to be sure we have no debt
			if tonBalance.Available().Sign() < 0 {
				return nil, nil, fmt.Errorf("too few available ton balance")
			}
			// check onchain to be sure we are able to do this tx
			if tonBalance.Onchain.Sign() < 0 {
				lowOnchain[payments.GetTONBalanceID()] = true
			}

			if p := pending.Amounts[payments.GetTONBalanceID()]; p != nil {
				p.Add(p, m.InternalMessage.Amount.Nano())
			} else {
				pending.Amounts[payments.GetTONBalanceID()] = m.InternalMessage.Amount.Nano()
			}
		}

		if !m.InternalMessage.ExtraCurrencies.IsEmpty() {
			ecs, err := m.InternalMessage.ExtraCurrencies.LoadAll()
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load all ec: %w", err)
			}

			for _, dictKV := range ecs {
				currencyId := uint32(dictKV.Key.MustLoadUInt(32))
				id := payments.GetECBalanceID(currencyId)
				if b := balances[id]; b != nil {
					amt := dictKV.Value.MustLoadVarUInt(32)
					b.Onchain.Sub(b.Onchain, amt)

					// check available to be sure we have no debt
					if b.Available().Sign() < 0 {
						return nil, nil, fmt.Errorf("too few available ec balance")
					}
					// check onchain to be sure we are able to do this tx
					if b.Onchain.Sign() < 0 {
						lowOnchain[id] = true
					}

					if p := pending.Amounts[id]; p != nil {
						p.Add(p, amt)
					} else {
						pending.Amounts[id] = amt
					}
				}
			}
		}

		if b := jettons[hex.EncodeToString(m.InternalMessage.DstAddr.Data())]; b != nil {
			// checking that it is not masking like ec or ton
			// checking for supported jetton amounts
			var pfx uint64
			body := m.InternalMessage.Body.MustBeginParse()
			if body.BitsLeft() >= 32 {
				pfx, err = body.PreloadUInt(32)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to load preload op bits: %w", err)
				}
			}

			var amount *big.Int
			switch pfx {
			case 0xf8a7ea5: // jetton transfer
				var tr TransferPayload
				if err = tlb.LoadFromCell(&tr, body); err != nil {
					return nil, nil, fmt.Errorf("failed to load jetton transfer payload: %w", err)
				}
				amount = tr.Amount.Nano()
			case 0x595f07bc: // burn
				var burn BurnPayload
				if err = tlb.LoadFromCell(&burn, body); err != nil {
					return nil, nil, fmt.Errorf("failed to load jetton burn payload: %w", err)
				}
				amount = burn.Amount.Nano()
			default:
				// preventive protection in case of custom jetton methods execution
				return nil, nil, fmt.Errorf("unsupported jetton transaction for double signing")
			}

			b.Onchain.Sub(b.Onchain, amount)

			// check available to be sure we have no debt
			if b.Available().Sign() < 0 {
				return nil, nil, fmt.Errorf("too few available jetton balance")
			}

			// check onchain to be sure we are able to do this tx
			if b.Onchain.Sign() < 0 {
				lowOnchain[b.CoinConfig.BalanceID] = true
			}

			if p := pending.Amounts[b.CoinConfig.BalanceID]; p != nil {
				p.Add(p, amount)
			} else {
				pending.Amounts[b.CoinConfig.BalanceID] = amount
			}
		}
	}

	if len(lowOnchain) > 0 {
		return nil, lowOnchain, nil
	}

	return &pending, nil, nil
}

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

type BurnPayload struct {
	_                   tlb.Magic        `tlb:"#595f07bc"`
	QueryID             uint64           `tlb:"## 64"`
	Amount              tlb.Coins        `tlb:"."`
	ResponseDestination *address.Address `tlb:"addr"`
	CustomPayload       *cell.Cell       `tlb:"maybe ^"`
}

// we copied it here to lower binary size for wasm build (because of imports chain)
func buildJettonTransferPayload(to, responseTo *address.Address, amountCoins, amountForwardTON tlb.Coins, payloadForward, customPayload *cell.Cell) (*cell.Cell, error) {
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
