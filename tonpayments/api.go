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
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

func (s *Service) DeployChannelWithNode(ctx context.Context, capacity tlb.Coins, nodeKey ed25519.PublicKey) (*address.Address, error) {
	cfg, err := s.transport.GetChannelConfig(ctx, nodeKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel config: %w", err)
	}

	log.Info().Msg("starting channel deploy")
	return s.deployChannelWithNode(ctx, nodeKey, address.NewAddress(0, 0, cfg.WalletAddr), capacity)
}

func (s *Service) deployChannelWithNode(ctx context.Context, nodeKey ed25519.PublicKey, nodeAddr *address.Address, capacity tlb.Coins) (*address.Address, error) {
	channelId := make([]byte, 16)
	copy(channelId, nodeKey[:15])

	channels, err := s.db.GetChannelsWithKey(ctx, nodeKey)
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

	body, code, data, err := s.contractMaker.GetDeployAsyncChannelParams(channelId, true, capacity, s.key, nodeKey, s.closingConfig, payments.PaymentConfig{
		ExcessFee: s.excessFee,
		DestA:     s.wallet.WalletAddress(),
		DestB:     nodeAddr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get deploy params: %w", err)
	}

	amt := new(big.Int).Add(capacity.Nano(), tlb.MustFromTON("0.05").Nano())

	addr, tx, _, err := s.wallet.DeployContractWaitTransaction(ctx, tlb.FromNanoTON(amt), body, code, data)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy: %w", err)
	}

	log.Info().Str("addr", addr.String()).Hex("tx", tx.Hash).Msg("contract deployed")
	return addr, nil
}

func (s *Service) OpenVirtualChannel(ctx context.Context, with, instructionKey ed25519.PublicKey, private ed25519.PrivateKey, chain []transport.OpenVirtualInstruction, vch payments.VirtualChannel) error {
	if len(chain) == 0 {
		return fmt.Errorf("chain is empty")
	}

	channels, err := s.db.GetChannelsWithKey(ctx, with)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	needAmount := new(big.Int).Add(vch.Fee, vch.Capacity)
	var channel *db.Channel
	for _, ch := range channels {
		if ch.Status != db.ChannelStateActive {
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

	act := transport.OpenVirtualAction{
		ChannelKey:     vch.Key,
		InstructionKey: instructionKey,
	}

	if err = act.SetInstructions(chain, private); err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	tryTill := time.Unix(vch.Deadline, 0)
	err = s.db.CreateTask(ctx, "open-virtual", channel.Address,
		"open-virtual-"+hex.EncodeToString(vch.Key),
		db.OpenVirtualTask{
			ChannelAddress: channel.Address,
			VirtualKey:     vch.Key,
			Deadline:       vch.Deadline,
			Fee:            vch.Fee.String(),
			Capacity:       vch.Capacity.String(),
			Action:         act,
		}, nil, &tryTill,
	)
	if err != nil {
		return fmt.Errorf("failed to create open task: %w", err)
	}

	return nil
}

func (s *Service) CloseVirtualChannel(ctx context.Context, virtualKey ed25519.PublicKey, state payments.VirtualChannelState) error {
	if !state.Verify(virtualKey) {
		return fmt.Errorf("incorrect state signature")
	}

	meta, err := s.db.GetVirtualChannelMeta(ctx, virtualKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("virtual channel is not exists")
		}
		return fmt.Errorf("failed to load virtual channel meta: %w", err)
	}

	ch, err := s.db.GetChannel(ctx, meta.FromChannelAddress)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("onchain channel with source not exists")
		}
		return fmt.Errorf("failed to load channel: %w", err)
	}

	_, vch, err := ch.Their.State.FindVirtualChannel(virtualKey)
	if err != nil {
		if errors.Is(err, payments.ErrNotFound) {
			// idempotency
			return nil
		}

		log.Error().Err(err).Str("channel", ch.Address).Msg("failed to find virtual channel")
		return fmt.Errorf("failed to find virtual channel: %w", err)
	}

	if state.Amount.Nano().Cmp(vch.Capacity) == 1 {
		return fmt.Errorf("amount cannot be > capacity")
	}

	if vch.Deadline < time.Now().Unix() {
		return fmt.Errorf("virtual channel has expired")
	}

	stateCell, err := state.ToCell()
	if err != nil {
		return fmt.Errorf("failed to serialize state to cell: %w", err)
	}

	if err = meta.AddKnownResolve(vch.Key, &state); err != nil {
		return fmt.Errorf("failed to add channel condition resolve: %w", err)
	}
	meta.ReadyToReleaseCoins = true
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
	if err = s.db.CreateTask(ctx, "uncooperative-close", ch.Address+"-uncoop",
		"uncooperative-close-"+ch.Address+"-vc-"+hex.EncodeToString(vch.Key),
		db.ChannelUncooperativeCloseTask{
			Address:                 ch.Address,
			CheckVirtualStillExists: vch.Key,
		}, &uncooperativeAfter, nil,
	); err != nil {
		log.Warn().Err(err).Str("channel", ch.Address).Msg("failed to create uncooperative close task")
	}

	if err = s.requestAction(ctx, ch.Address, transport.CloseVirtualAction{
		Key:   vch.Key,
		State: stateCell,
	}); err != nil {
		return fmt.Errorf("failed to request action from the node: %w", err)
	}
	return nil
}

func (s *Service) RequestInboundChannel(ctx context.Context, capacity tlb.Coins, theirKey ed25519.PublicKey) error {
	res, err := s.transport.RequestInboundChannel(ctx, capacity.Nano(), s.wallet.Address(), s.key.Public().(ed25519.PublicKey), theirKey)
	if err != nil {
		return fmt.Errorf("failed to request inbound channel: %w", err)
	}

	if !res.Agreed {
		return fmt.Errorf("inbound channel request declined with reason: %s", res.Reason)
	}
	return nil
}

func (s *Service) executeCooperativeClose(ctx context.Context, partyReq *payments.CooperativeClose, channelAddr string) error {
	ourReq, channel, err := s.getCooperativeCloseRequest(channelAddr, partyReq)
	if err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	channel.AcceptingActions = false
	if err = s.db.UpdateChannel(ctx, channel); err != nil {
		return fmt.Errorf("failed to update channel: %w", err)
	}

	// TODO: send external in worker and wait result

	msg, err := tlb.ToCell(ourReq)
	if err != nil {
		return fmt.Errorf("failed to serialize close channel request: %w", err)
	}

	err = s.ton.SendExternalMessage(ctx, &tlb.ExternalMessage{
		DstAddr: address.MustParseAddr(channel.Address),
		Body:    msg,
	})
	if err != nil {
		return fmt.Errorf("failed to send external message to channel: %w", err)
	}
	return nil
}

func (s *Service) RequestCooperativeClose(ctx context.Context, channelAddr string) error {
	s.mx.Lock()
	defer s.mx.Unlock()

	_, ch, err := s.getCooperativeCloseRequest(channelAddr, nil)
	if err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	return s.db.Transaction(ctx, func(ctx context.Context) error {
		ch.AcceptingActions = false
		if err = s.db.UpdateChannel(ctx, ch); err != nil {
			return fmt.Errorf("failed to update channel: %w", err)
		}

		if err = s.db.CreateTask(ctx, "cooperative-close", ch.Address,
			"cooperative-close-"+ch.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelUncooperativeCloseTask{
				Address:            ch.Address,
				ChannelInitiatedAt: &ch.InitAt,
			}, nil, nil,
		); err != nil {
			return fmt.Errorf("failed to create cooperative close task: %w", err)
		}
		return nil
	})
}

func (s *Service) getCooperativeCloseRequest(channelAddr string, partyReq *payments.CooperativeClose) (*payments.CooperativeClose, *db.Channel, error) {
	channel, err := s.GetActiveChannel(channelAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get channel: %w", err)
	}

	for _, kv := range channel.Our.State.Data.Conditionals.All() {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.Deadline >= time.Now().Unix() {
			return nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	for _, kv := range channel.Their.State.Data.Conditionals.All() {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		// if condition is not expired we cannot close onchain channel
		if vch.Deadline >= time.Now().Unix() {
			return nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
		}
	}

	ourBalance, err := channel.CalcBalance(false)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to calc our balance: %w", err)
	}

	theirBalance, err := channel.CalcBalance(true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to calc their balance: %w", err)
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
		return nil, nil, fmt.Errorf("failed to serialize body to cell: %w", err)
	}

	ourReq.SignatureA.Value = dataCell.Sign(s.key)
	if partyReq == nil {
		ourReq.SignatureB.Value = make([]byte, 64)
	} else {
		if !channel.WeLeft {
			ourReq.SignatureB = partyReq.SignatureA
		} else {
			ourReq.SignatureB = partyReq.SignatureB
		}

		if !dataCell.Verify(channel.TheirOnchain.Key, ourReq.SignatureB.Value) {
			return nil, nil, fmt.Errorf("incorrect party signature")
		}
	}

	if !channel.WeLeft {
		ourReq.SignatureA.Value, ourReq.SignatureB.Value = ourReq.SignatureB.Value, ourReq.SignatureA.Value
	}

	return &ourReq, channel, nil
}

func (s *Service) StartUncooperativeClose(ctx context.Context, channelAddr string) error {
	channel, err := s.GetActiveChannel(channelAddr)
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
		log.Info().Str("address", channel.Address).
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
	log.Info().Hex("hash", tx.Hash).Msg("uncooperative close transaction completed")

	return nil
}

func (s *Service) ChallengeChannelState(ctx context.Context, channelAddr string) error {
	channel, err := s.getVerifiedChannel(channelAddr)
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

	err = s.ton.SendExternalMessage(ctx, &tlb.ExternalMessage{
		DstAddr: address.MustParseAddr(channel.Address),
		Body:    msgCell,
	})
	if err != nil {
		return fmt.Errorf("failed to send external message to channel: %w", err)
	}

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) FinishUncooperativeChannelClose(ctx context.Context, channelAddr string) error {
	channel, err := s.getVerifiedChannel(channelAddr)
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

	err = s.ton.SendExternalMessage(ctx, &tlb.ExternalMessage{
		DstAddr: address.MustParseAddr(channel.Address),
		Body:    msgCell,
	})
	if err != nil {
		return fmt.Errorf("failed to send external message to channel: %w", err)
	}

	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) SettleChannelConditionals(ctx context.Context, channelAddr string) error {
	channel, err := s.getVerifiedChannel(channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Their.State.Data.Conditionals.Size() == 0 {
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
	msg.Signed.B = channel.Their.SignedSemiChannel
	msg.Signed.ConditionalsToSettle = cell.NewDict(32)
	// TODO: get all conditions and make inputs for known
	for _, kv := range channel.Their.State.Data.Conditionals.All() {
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

			if err = msg.Signed.ConditionalsToSettle.Set(kv.Key, rc); err != nil {
				log.Warn().Err(err).Msg("failed to store known virtual channel state in request")
				continue
			}
		}
	}

	// TODO: maybe wait for some deadline if not all states resolved, before settle
	if msg.Signed.ConditionalsToSettle.Size() == 0 {
		log.Warn().Msg("no known resolves for existing conditions")
		return nil
	}

	if msg.Signed.ConditionalsToSettle.Size() != channel.Their.State.Data.Conditionals.Size() {
		log.Warn().
			Int("with_resolves", msg.Signed.ConditionalsToSettle.Size()).
			Int("all", channel.Their.State.Data.Conditionals.Size()).
			Msg("not all conditions has resolves yet, settling as is")
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

	tx, _, err := s.wallet.SendWaitTransaction(ctx, wallet.SimpleMessage(address.MustParseAddr(channel.Address), tlb.MustFromTON("0.05"), msgCell))
	if err != nil {
		return fmt.Errorf("failed to send internal message to channel: %w", err)
	}
	log.Info().Hex("hash", tx.Hash).Msg("settle conditions transaction completed")

	// TODO: wait event from invalidator here to confirm
	return nil
}
