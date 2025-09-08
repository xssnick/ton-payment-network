package payments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

type AsyncUniversalChannel struct {
	Status  ChannelStatus
	Storage AsyncUniversalChannelStorageData
	addr    *address.Address
	client  *Client
}

func (c *Client) GetDeployAsyncUniversalChannelParams(channelId ChannelID, isA bool, seqnoA, seqnoB uint64, ourKey ed25519.PrivateKey, theirKey ed25519.PublicKey, theirSig []byte, closingConfig UniversalClosingConfig) (body, data *cell.Cell, signature []byte, err error) {
	if len(channelId) != 16 {
		return nil, nil, nil, fmt.Errorf("channelId len should be 16 bytes")
	}

	storageData := AsyncUniversalChannelStorageData{
		IsA:           isA,
		KeyA:          ourKey.Public().(ed25519.PublicKey),
		KeyB:          theirKey,
		ChannelID:     channelId,
		ClosingConfig: closingConfig,
	}

	if !isA {
		storageData.KeyA, storageData.KeyB = storageData.KeyB, storageData.KeyA
	}

	data, err = tlb.ToCell(storageData)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize storage data: %w", err)
	}

	initCh := InitChannelUniversal{}
	initCh.Signed.ChannelID = channelId
	initCh.Signed.SeqnoA = seqnoA
	initCh.Signed.SeqnoB = seqnoB
	sig, err := toSignature(initCh.Signed, ourKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to sign data: %w", err)
	}

	if theirSig != nil {
		cl, err := tlb.ToCell(initCh.Signed)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize signed: %w", err)
		}

		if !cl.Verify(theirKey, theirSig) {
			return nil, nil, nil, fmt.Errorf("their signature is not match")
		}
	}

	if len(theirSig) == 0 {
		theirSig = make([]byte, ed25519.SignatureSize)
	}

	if isA {
		initCh.SignatureA, initCh.SignatureB = sig, Signature{Value: theirSig}
	} else {
		initCh.SignatureA, initCh.SignatureB = Signature{Value: theirSig}, sig
	}

	body, err = tlb.ToCell(initCh)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}
	return body, data, sig.Value, nil
}

func (c *Client) GetAsyncUniversalChannel(ctx context.Context, addr *address.Address, verify bool) (*AsyncUniversalChannel, error) {
	acc, err := c.api.GetAccount(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	if !acc.IsActive {
		return nil, fmt.Errorf("channel account is not active %s", addr.String())
	}

	return c.ParseAsyncUniversalChannel(addr, acc.Code, acc.Data, verify)
}

func (c *Client) ParseAsyncUniversalChannel(addr *address.Address, code, data *cell.Cell, verify bool) (*AsyncUniversalChannel, error) {
	if verify {
		ok := false
		for _, h := range PaymentChannelUniversalCodes {
			if bytes.Equal(code.Hash(), h.Hash()) {
				ok = true
				break
			}
		}

		if !ok {
			return nil, ErrVerificationNotPassed
		}
	}

	ch := &AsyncUniversalChannel{
		addr:   addr,
		client: c,
		Status: ChannelStatusUninitialized,
	}

	err := tlb.LoadFromCell(&ch.Storage, data.BeginParse())
	if err != nil {
		return nil, fmt.Errorf("failed to load storage: %w", err)
	}

	if verify {
		storageData := AsyncUniversalChannelStorageData{
			IsA:             ch.Storage.IsA,
			Initialized:     false,
			CommittedSeqnoA: 0,
			CommittedSeqnoB: 0,
			WalletSeqno:     0,
			KeyA:            ch.Storage.KeyA,
			KeyB:            ch.Storage.KeyB,
			ChannelID:       ch.Storage.ChannelID,
			ClosingConfig:   ch.Storage.ClosingConfig,
			PartyAddress:    nil,
			Quarantine:      nil,
		}

		data, err = tlb.ToCell(storageData)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize storage data: %w", err)
		}

		si, err := tlb.ToCell(tlb.StateInit{
			Code: code,
			Data: data,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to serialize state init: %w", err)
		}

		if !bytes.Equal(si.Hash(), ch.addr.Data()) {
			return nil, ErrVerificationNotPassed
		}
	}

	ch.Status = ch.calcState()

	return ch, nil
}

// calcState - it repeats get_channel_state method of contract,
// we do this because we cannot prove method execution for now,
// but can proof contract data and code, so this approach is safe
func (c *AsyncUniversalChannel) calcState() ChannelStatus {
	if !c.Storage.Initialized {
		return ChannelUniversalStatusUninitialized
	}
	if c.Storage.Quarantine == nil {
		return ChannelUniversalStatusOpen
	}
	now := time.Now().UTC().Unix()
	quarantineEnds := int64(c.Storage.Quarantine.QuarantineStarts) + int64(c.Storage.ClosingConfig.QuarantineDuration)
	if quarantineEnds > now {
		return ChannelUniversalStatusClosureStarted
	}
	if c.Storage.Quarantine.TheirState != nil {
		if quarantineEnds+int64(c.Storage.ClosingConfig.ConditionalCloseDuration) > now {
			return ChannelUniversalStatusSettlingConditionals
		}
		if quarantineEnds+int64(c.Storage.ClosingConfig.ConditionalCloseDuration)+int64(c.Storage.ClosingConfig.ActionsDuration) > now {
			return ChannelUniversalStatusExecutingActions
		}
	}
	return ChannelUniversalStatusAwaitingFinalization
}

func (c *AsyncUniversalChannel) PrepareDoubleExternalMessage(ourKey ed25519.PrivateKey, theirSig []byte, messages []*wallet.Message, validUntil uint32) (body *cell.Cell, signature []byte, err error) {
	msg := ExternalMsgDoubleSigned{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.SideA = c.Storage.IsA
	msg.Signed.ValidUntil = validUntil
	out, err := packOutActions(messages)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to pack out actions: %w", err)
	}
	msg.Signed.OutActions = out

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *AsyncUniversalChannel) PrepareOwnerExternalMessage(ourKey ed25519.PrivateKey, messages []*wallet.Message, validUntil uint32) (body *cell.Cell, err error) {
	msg := ExternalMsgOwnerSigned{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.SideA = c.Storage.IsA
	msg.Signed.ValidUntil = validUntil
	out, err := packOutActions(messages)
	if err != nil {
		return nil, fmt.Errorf("failed to pack out actions: %w", err)
	}
	msg.Signed.OutActions = out

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *AsyncUniversalChannel) PrepareUncoopCloseMessage(ourKey ed25519.PrivateKey, states *UncoopSignedStates) (body *cell.Cell, err error) {
	msg := UncoopCloseMsgUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.States = states

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *AsyncUniversalChannel) PrepareSettleMessage(ourKey ed25519.PrivateKey, toSettle *cell.Dictionary, condProof, actProof *cell.Cell) (body *cell.Cell, err error) {
	msg := SettleMsgUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.ToSettle = toSettle
	msg.Signed.ConditionalsProof = condProof
	msg.Signed.ActionsInputProof = actProof

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *AsyncUniversalChannel) PrepareFinalizeSettleMessage(ourKey ed25519.PrivateKey, actionsHash []byte) (body *cell.Cell, err error) {
	msg := FinalizeSettleMsgUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.WalletSeqno = c.Storage.WalletSeqno
	msg.Signed.ActionsInputHash = actionsHash

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *AsyncUniversalChannel) PrepareExecuteActionsMessage(ourKey ed25519.PrivateKey, action *cell.Cell, ourProof *cell.Cell, theirProof *cell.Cell) (body *cell.Cell, err error) {
	msg := ExecuteActionsMsgUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.Action = action
	msg.Signed.OurActionsInputProof = ourProof
	msg.Signed.TheirActionsInputProof = theirProof

	expectedKey := c.Storage.KeyB
	if c.Storage.IsA {
		expectedKey = c.Storage.KeyA
	}

	if !ourKey.Public().(ed25519.PublicKey).Equal(expectedKey) {
		return nil, fmt.Errorf("our key is not match")
	}

	msg.Signature, err = toSignature(msg.Signed, ourKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign data: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, nil
}

func (c *AsyncUniversalChannel) PrepareCoopCommitMessage(ourKey ed25519.PrivateKey, theirSig []byte, seqnoA, seqnoB uint64) (body *cell.Cell, signature []byte, err error) {
	msg := CooperativeCommitUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.SeqnoA = seqnoA
	msg.Signed.SeqnoB = seqnoB

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *AsyncUniversalChannel) PrepareCoopCloseMessage(ourKey ed25519.PrivateKey, theirSig []byte, seqnoA, seqnoB uint64) (body *cell.Cell, signature []byte, err error) {
	msg := CooperativeCloseUniversal{}
	msg.Signed.ChannelID = c.Storage.ChannelID
	msg.Signed.SeqnoA = seqnoA
	msg.Signed.SeqnoB = seqnoB

	var newSig Signature
	newSig, msg.SignatureA, msg.SignatureB, err = c.prepareDoubleSignedMessage(ourKey, theirSig, msg.Signed)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare double signed message: %w", err)
	}

	body, err = tlb.ToCell(msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize message: %w", err)
	}

	return body, newSig.Value, nil
}

func (c *AsyncUniversalChannel) prepareDoubleSignedMessage(ourKey ed25519.PrivateKey, theirSig []byte, msg any) (newSig, sigA, sigB Signature, err error) {
	sig, err := toSignature(msg, ourKey)
	if err != nil {
		return Signature{}, Signature{}, Signature{}, fmt.Errorf("failed to sign data: %w", err)
	}

	weA := ourKey.Public().(ed25519.PublicKey).Equal(c.Storage.KeyA)

	var theirKey = c.Storage.KeyB
	if !weA {
		theirKey = c.Storage.KeyA
		if !ourKey.Public().(ed25519.PublicKey).Equal(c.Storage.KeyB) {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("our key is not match %s %s", hex.EncodeToString(c.Storage.KeyB), hex.EncodeToString(ourKey.Public().(ed25519.PublicKey)))
		}
	}

	if theirSig != nil {
		cl, err := tlb.ToCell(msg)
		if err != nil {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("failed to serialize signed: %w", err)
		}

		if !cl.Verify(theirKey, theirSig) {
			return Signature{}, Signature{}, Signature{}, fmt.Errorf("their signature is not match")
		}
	}

	if len(theirSig) == 0 {
		theirSig = make([]byte, ed25519.SignatureSize)
	}

	if weA {
		sigA, sigB = sig, Signature{Value: theirSig}
	} else {
		sigA, sigB = Signature{Value: theirSig}, sig
	}

	return sig, sigA, sigB, nil
}

func (c *AsyncUniversalChannel) SignSemiChannel(data SemiChannelUniversal, key ed25519.PrivateKey) (SemiChannelUniversalSigned, error) {
	val := SemiChannelPacked{
		ChannelID: c.Storage.ChannelID,
		State:     data,
	}

	cl, err := tlb.ToCell(val)
	if err != nil {
		return SemiChannelUniversalSigned{}, fmt.Errorf("failed to serialize signed: %w", err)
	}

	return SemiChannelUniversalSigned{
		Signature: Signature{
			Value: cl.Sign(key),
		},
		Channel: val,
	}, nil
}

func validateMessageFields(messages []*wallet.Message) error {
	if len(messages) > 255 {
		return fmt.Errorf("max 255 messages allowed for v5")
	}
	for _, message := range messages {
		if message.InternalMessage == nil {
			return fmt.Errorf("internal message cannot be nil")
		}
	}
	return nil
}

func packOutActions(messages []*wallet.Message) (*cell.Cell, error) {
	if err := validateMessageFields(messages); err != nil {
		return nil, err
	}

	var list = cell.BeginCell().EndCell()
	for _, message := range messages {
		outMsg, err := tlb.ToCell(message.InternalMessage)
		if err != nil {
			return nil, err
		}

		/*
			out_list_empty$_ = OutList 0;
			out_list$_ {n:#} prev:^(OutList n) action:OutAction
			  = OutList (n + 1);
			action_send_msg#0ec3c86d mode:(## 8)
			  out_msg:^(MessageRelaxed Any) = OutAction;
		*/
		msg := cell.BeginCell().MustStoreUInt(0x0ec3c86d, 32). // action_send_msg prefix
									MustStoreUInt(uint64(message.Mode), 8). // mode
									MustStoreRef(outMsg)                    // message reference

		list = cell.BeginCell().MustStoreRef(list).MustStoreBuilder(msg).EndCell()
	}

	// Ensure the action list ends with 0, 1 as per the new specification
	return list, nil
}

const (
	ChannelUniversalStatusUninitialized ChannelStatus = iota
	ChannelUniversalStatusOpen
	ChannelUniversalStatusClosureStarted
	ChannelUniversalStatusSettlingConditionals
	ChannelUniversalStatusExecutingActions
	ChannelUniversalStatusAwaitingFinalization
)
