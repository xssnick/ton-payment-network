package node

import (
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
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"time"
)

func (s *Service) GetActiveChannelsWithNode(_ context.Context, nodeKey ed25519.PublicKey) ([]string, error) {
	list, err := s.db.GetActiveChannelsWithKey(nodeKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get channels: %w", err)
	}

	var addresses []string
	for _, channel := range list {
		addresses = append(addresses, channel.Address)
	}
	return addresses, nil
}

func (s *Service) DeployChannelWithNode(ctx context.Context, channelId []byte, nodeKey ed25519.PublicKey, capacity tlb.Coins) (*address.Address, error) {
	cfg, err := s.transport.GetChannelConfig(ctx, nodeKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel config: %w", err)
	}

	log.Info().Msg("starting channel deploy")
	return s.deployChannelWithNode(ctx, channelId, nodeKey, address.NewAddress(0, 0, cfg.WalletAddr), capacity)
}

func (s *Service) deployChannelWithNode(ctx context.Context, channelId []byte, nodeKey ed25519.PublicKey, nodeAddr *address.Address, capacity tlb.Coins) (*address.Address, error) {
	body, code, data, err := s.contractMaker.GetDeployAsyncChannelParams(channelId, true, capacity, s.key, nodeKey, s.closingConfig, payments.PaymentConfig{
		ExcessFee: s.excessFee,
		DestA:     s.wallet.Address(),
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

func (s *Service) IncrementStates(ctx context.Context, channelAddr string) error {
	channel, err := s.getVerifiedChannel(channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.proposeAction(ctx, channel, transport.IncrementStatesAction{}, nil); err != nil {
		return fmt.Errorf("failed to propose action to the party: %w", err)
	}
	return nil
}

func (s *Service) OpenVirtualChannel(ctx context.Context, with ed25519.PublicKey, private ed25519.PrivateKey, chain []transport.OpenVirtualInstruction, vch payments.VirtualChannel) error {
	if len(chain) == 0 {
		return fmt.Errorf("chain is empty")
	}

	channels, err := s.GetActiveChannelsWithNode(ctx, with)
	if err != nil {
		return fmt.Errorf("failed to get active channels: %w", err)
	}

	if len(channels) == 0 {
		return fmt.Errorf("no active channels with first node")
	}
	// TODO: select channel with enough balance

	channel, err := s.GetActiveChannel(channels[0])
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	act := transport.OpenVirtualAction{
		Key: vch.Key,
	}

	if err = act.SetInstructions(chain, private); err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if err = s.proposeAction(ctx, channel, act, vch); err != nil {
		return fmt.Errorf("failed to propose actions to the node: %w", err)
	}
	return nil
}

func (s *Service) CloseVirtualChannel(ctx context.Context, virtualKey ed25519.PublicKey, state payments.VirtualChannelState) error {
	channels, err := s.db.GetActiveChannels()
	if err != nil {
		log.Error().Err(err).Msg("failed to get active channels")
		return fmt.Errorf("failed to get active channels: %w", err)
	}
	// TODO: tampering checks

	if !state.Verify(virtualKey) {
		return fmt.Errorf("incorrect state signature")
	}

	for _, ch := range channels {
		_, vch, err := ch.Their.State.FindVirtualChannel(virtualKey)
		if err != nil {
			if errors.Is(err, payments.ErrNotFound) {
				continue
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

		if err = ch.AddKnownResolve(vch.Key, &state); err != nil {
			return fmt.Errorf("failed to add channel condition resolve: %w", err)
		}
		if err = s.db.UpdateChannel(ch); err != nil {
			return fmt.Errorf("failed to update channel in db: %w", err)
		}

		if err = s.requestAction(ctx, ch, transport.CloseVirtualAction{
			Key:   vch.Key,
			State: stateCell,
		}); err != nil {
			// TODO: task for coop close retry
			// TODO: accurate calc timings
			after := time.Now() // time.Unix(vch.Deadline-s.deadlineDecreaseNextHop, 0)

			// creating aggressive onchain close task, for the future,
			// in case we will not be able to communicate with party
			errCreate := s.db.CreateTask("uncooperative-close",
				"uncooperative-close-"+ch.Address+"-vc-"+hex.EncodeToString(vch.Key),
				db.ChannelUncooperativeCloseTask{
					Address:                 ch.Address,
					CheckVirtualStillExists: vch.Key,
				}, &after, nil,
			)
			if errCreate != nil {
				log.Error().Err(errCreate).Str("channel", ch.Address).Msg("failed to create uncooperative close task")
			}

			return fmt.Errorf("failed to request action from the node: %w", err)
		}
		return nil
	}
	return fmt.Errorf("virtual channel is not exists")
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

func (s *Service) ExecuteCooperativeClose(ctx context.Context, partyReq *payments.CooperativeClose, channelAddr string) error {
	ourReq, channel, err := s.getCooperativeCloseRequest(channelAddr, partyReq)
	if err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

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
	req, ch, err := s.getCooperativeCloseRequest(channelAddr, nil)
	if err != nil {
		return fmt.Errorf("failed to prepare close channel request: %w", err)
	}

	cl, err := tlb.ToCell(req)
	if err != nil {
		return fmt.Errorf("failed to serialize request to cell: %w", err)
	}

	log.Info().Msg("trying cooperative close")

	if err = s.requestAction(ctx, ch, transport.CooperativeCloseAction{
		SignedCloseRequest: cl,
	}); err != nil {
		// TODO: be not so aggressive
		// creating aggressive onchain close task
		errCreate := s.db.CreateTask("uncooperative-close",
			"uncooperative-close-"+ch.Address+"-"+fmt.Sprint(ch.InitAt.Unix()),
			db.ChannelUncooperativeCloseTask{
				Address: ch.Address,
			}, nil, nil,
		)
		if errCreate != nil {
			log.Error().Err(errCreate).Str("channel", ch.Address).Msg("failed to create uncooperative close task")
		}

		return fmt.Errorf("failed to request action from the node: %w", err)
	}

	// TODO: wait close here to confirm
	return nil
}

func (s *Service) getCooperativeCloseRequest(channelAddr string, partyReq *payments.CooperativeClose) (*payments.CooperativeClose, *db.Channel, error) {
	channel, err := s.GetActiveChannel(channelAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get channel: %w", err)
	}

	if channel.Our.State.Data.Conditionals.Size() > 0 ||
		channel.Their.State.Data.Conditionals.Size() > 0 {
		return nil, nil, fmt.Errorf("conditionals should be resolved before cooperative close")
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
	// TODO: wait event from invalidator here to confirm
	return nil
}

func (s *Service) ChallengeChannelState(ctx context.Context, channelAddr string) error {
	channel, err := s.getVerifiedChannel(channelAddr)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
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

		if resolve := channel.GetKnownResolve(vch.Key); resolve != nil {
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
