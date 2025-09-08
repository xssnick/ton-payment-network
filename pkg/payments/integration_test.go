package payments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	client2 "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var api = func() ton.APIClientWrapped {
	client := liteclient.NewConnectionPool()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.AddConnectionsFromConfigUrl(ctx, "https://ton-blockchain.github.io/testnet-global.config.json")
	if err != nil {
		panic(err)
	}

	return ton.NewAPIClient(client).WithRetry()
}()

var code = PaymentChannelCodes[0]
var codeUniversal = PaymentChannelUniversalCodes[0]

var _seed = strings.Split(os.Getenv("WALLET_SEED"), " ")

func TestClient_AsyncChannelFullFlowUniversal(t *testing.T) {
	client := NewPaymentChannelClient(client2.NewTON(api))
	ctx := api.Client().StickyContext(context.Background())

	chID, err := RandomChannelID()
	if err != nil {
		t.Fatal(err)
	}

	aPubKey, aKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bPubKey, bKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := wallet.FromSeed(api, _seed, wallet.HighloadV2R2)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.Address().String())

	closeConfig := UniversalClosingConfig{
		QuarantineDuration:             25,
		ReplicationMessageAttachAmount: tlb.MustFromTON("0.05"),
		ConditionalCloseDuration:       50,
		ActionsDuration:                40,
	}

	_, _, bSig, err := client.GetDeployAsyncUniversalChannelParams(chID, false, 0, 0, bKey, aPubKey, nil, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	body, data, _, err := client.GetDeployAsyncUniversalChannelParams(chID, true, 0, 0, aKey, bPubKey, bSig, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(ctx, tlb.MustFromTON("0.5"), body, codeUniversal, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

reCh:
	ch, err := client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reCh
	}

	log.Println("party channel addr:", ch.Storage.PartyAddress.Value.String())

reCh2:
	ch2, err := client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reCh2
	}

	if ch.Status != ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}
	if ch2.Status != ChannelStatusOpen {
		t.Fatal("party channel status incorrect")
	}

	log.Println("channel is active", ch.Storage.Initialized)

	until := uint32(time.Now().Add(90 * time.Second).Unix())
	msg := wallet.SimpleMessage(ch.Storage.PartyAddress.Value, tlb.MustFromTON("0.1"), cell.BeginCell().EndCell())
	_, bSig, err = ch.PrepareDoubleExternalMessage(bKey, nil, []*wallet.Message{msg}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	body, _, err = ch.PrepareDoubleExternalMessage(aKey, bSig, []*wallet.Message{msg}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

reTx5:
	tx, _, _, err := api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx5
	}
	log.Println("double signed tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	_, bSig, err = ch.PrepareCoopCommitMessage(bKey, nil, 1, 1)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCommitMessage(aKey, bSig, 1, 1)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("commit tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh3:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh3
	}
	if ch.Storage.CommittedSeqnoA != 1 || ch.Storage.CommittedSeqnoB != 1 {
		t.Log("commit not yet updated")
		goto reCh3
	}

reCh4:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh4
	}
	if ch2.Storage.CommittedSeqnoA != 1 || ch2.Storage.CommittedSeqnoB != 1 {
		t.Log("commit not yet updated")
		goto reCh4
	}
	log.Println("commit updated")

	_, bSig, err = ch.PrepareCoopCloseMessage(bKey, nil, 1, 1)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	body, _, err = ch.PrepareCoopCloseMessage(aKey, bSig, 1, 1)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: channelAddr,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("close tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh5:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh5
	}
	if ch.Storage.Initialized {
		t.Log("close not yet updated")
		goto reCh5
	}

reCh6:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh6
	}
	if ch2.Storage.Initialized {
		t.Log("close not yet updated")
		goto reCh6
	}
	log.Println("close updated")

	until = uint32(time.Now().Add(90 * time.Second).Unix())
	text, _ := wallet.CreateCommentCell("респект тем кто с нами делится чудесами")
	msg = wallet.SimpleMessage(ch.Storage.PartyAddress.Value, tlb.MustFromTON("0.08"), text)
	body, err = ch.PrepareOwnerExternalMessage(aKey, []*wallet.Message{msg}, until)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to prepare double signed message b: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch.addr,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("owner tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	_, _, bSig, err = client.GetDeployAsyncUniversalChannelParams(chID, false, 2, 2, bKey, aPubKey, nil, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	body, _, _, err = client.GetDeployAsyncUniversalChannelParams(chID, true, 2, 2, aKey, bPubKey, bSig, closeConfig)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch.Storage.PartyAddress.Value,
		Body:    body,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("init external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh7:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh7
	}
	if !ch.Storage.Initialized {
		t.Log("init not yet updated")
		goto reCh7
	}

reCh8:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh8
	}
	if !ch2.Storage.Initialized {
		t.Log("init not yet updated")
		goto reCh8
	}
	log.Println("init updated")

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)
	_ = vKey

	condA := cell.NewDict(32)
	condB := cell.NewDict(32)
	_ = condB

	actA := cell.NewDict(256)
	actB := cell.NewDict(256)

	bd, _ := wallet.CreateCommentCell("test payout")
	a1 := ActionSendUniversal{
		AddressA: ch.addr,
		AddressB: ch2.addr,
		Body:     bd,
	}
	actC := a1.Serialize()
	_ = actA.SetIntKey(new(big.Int).SetBytes(actC.Hash()),
		cell.BeginCell().MustStoreBigCoins(tlb.MustFromTON("0.009999").Nano()).EndCell())
	_ = actB.SetIntKey(new(big.Int).SetBytes(actC.Hash()),
		cell.BeginCell().MustStoreBigCoins(tlb.MustFromTON("0.00").Nano()).EndCell())

	vch := VirtualChannelUniversal{
		ActionHash: actC.Hash(),
		Key:        vPubKey,
		Capacity:   tlb.MustFromTON("0.03").Nano(),
		Fee:        tlb.MustFromTON("0.01").Nano(),
		Prepay:     tlb.MustFromTON("0.00").Nano(),
		Deadline:   time.Now().Add(5 * time.Minute).Unix(),
	}
	_ = condA.SetIntKey(big.NewInt(0), vch.Serialize())
	vch2 := VirtualChannelUniversal{
		ActionHash: actC.Hash(),
		Key:        vPubKey,
		Capacity:   tlb.MustFromTON("0.04").Nano(),
		Fee:        tlb.MustFromTON("0.00").Nano(),
		Prepay:     tlb.MustFromTON("0.01").Nano(),
		Deadline:   time.Now().Add(5 * time.Minute).Unix(),
	}
	_ = condA.SetIntKey(big.NewInt(5), vch2.Serialize())

	aBody := SemiChannelUniversalBody{
		Seqno:            3,
		ConditionalsHash: condA.AsCell().Hash(),
		ActionsHash:      actA.AsCell().Hash(),
	}

	bBody := SemiChannelUniversalBody{
		Seqno:            3,
		ConditionalsHash: make([]byte, 32), //condB.AsCell().Hash(),
		ActionsHash:      actB.AsCell().Hash(),
	}

	aState, err := ch.SignSemiChannel(SemiChannelUniversal{
		Data:             aBody,
		CounterpartyData: bBody,
	}, aKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	bState, err := ch.SignSemiChannel(SemiChannelUniversal{
		Data:             bBody,
		CounterpartyData: aBody,
	}, bKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	body, err = ch.PrepareUncoopCloseMessage(aKey, &UncoopSignedStates{
		A: aState,
		B: bState,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	aBody.Seqno = 4
	bBody.Seqno = 4

	aState2, err := ch2.SignSemiChannel(SemiChannelUniversal{
		Data:             aBody,
		CounterpartyData: bBody,
	}, aKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	bState2, err := ch2.SignSemiChannel(SemiChannelUniversal{
		Data:             bBody,
		CounterpartyData: aBody,
	}, bKey)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to sign state: %w", err))
	}

	body2, err := ch2.PrepareUncoopCloseMessage(bKey, &UncoopSignedStates{
		A: aState2,
		B: bState2,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	var ok = true
	go func() {
		defer wg.Done()

		tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch.addr,
			Body:    body,
		})
		if err != nil {
			ok = false
			t.Fatal(fmt.Errorf("failed to send tx: %w", err))
		}
		log.Println("uncoop start A external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	}()

	go func() {
		defer wg.Done()

		tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
			DstAddr: ch2.addr,
			Body:    body2,
		})
		if err != nil {
			ok = false

			t.Fatal(fmt.Errorf("failed to send tx: %w", err))
		}
		log.Println("uncoop start B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))
	}()
	wg.Wait()

	if !ok {
		t.Fatal("failed to execute tx")
	}

reCh9:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh9
	}
	if ch.Storage.CommittedSeqnoA != 4 || ch.Storage.CommittedSeqnoB != 4 || ch.Storage.Quarantine == nil || !ch.Storage.Quarantine.CommittedByOwner {
		t.Log("seqno not yet updated")
		goto reCh9
	}

reCh10:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh10
	}
	if ch2.Storage.CommittedSeqnoA != 4 || ch2.Storage.CommittedSeqnoB != 4 || ch2.Storage.Quarantine == nil || !ch2.Storage.Quarantine.CommittedByOwner {
		t.Log("seqno not yet updated")
		goto reCh10
	}
	log.Println("seqno updated")

reCh11:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh11
	}
	if ch.calcState() != ChannelUniversalStatusSettlingConditionals {
		t.Log("waiting for quarantine end")
		goto reCh11
	}

reCh12:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh12
	}
	if ch2.calcState() != ChannelUniversalStatusSettlingConditionals {
		t.Log("waiting for quarantine end")
		goto reCh12
	}
	log.Println("ready to settle")

	block, err = api.WaitForBlock(block.SeqNo + 3).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	res, err := api.RunGetMethod(ctx, block, ch2.addr, "get_channel_state")
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel state: %w", err))
	}

	println("GET STATE", res.MustInt(0).Uint64())
	t.Log("cur actions hash", hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionsHash))

	var sk = cell.CreateProofSkeleton()
	sk.SetRecursive()
	condAProof, _ := condA.AsCell().CreateProof(sk)
	actAProof, _ := actA.AsCell().CreateProof(sk)

	state := VirtualChannelState{
		Amount: tlb.MustFromTON("0.03").Nano(),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	toSettle := cell.NewDict(32)
	toSettle.SetIntKey(big.NewInt(0), condInput)
	toSettle.SetIntKey(big.NewInt(5), condInput)

	body, err = ch2.PrepareSettleMessage(bKey, toSettle, condAProof, actAProof)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx3:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.addr,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx3
	}
	log.Println("settle B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

	_ = actA.SetIntKey(new(big.Int).SetBytes(actC.Hash()),
		cell.BeginCell().MustStoreBigCoins(tlb.MustFromTON("0.069999").Nano()).EndCell())

	println("UPDATED ACT", actA.AsCell().Dump())

reCh14:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh14
	}
	if !bytes.Equal(ch2.Storage.Quarantine.TheirState.ActionsHash, actA.AsCell().Hash()) {
		t.Log("waiting for actions updated, cur hash", hex.EncodeToString(ch2.Storage.Quarantine.TheirState.ActionsHash))
		goto reCh14
	}
	log.Println("settled, actions updated")

	body, err = ch2.PrepareFinalizeSettleMessage(bKey, actA.AsCell().Hash())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx4:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.addr,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx4
	}
	log.Println("finalize settle B external tx sent:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh15:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh15
	}
	if !ch2.Storage.Quarantine.OurSettlementFinalized {
		t.Log("waiting for settlement finalization")
		goto reCh15
	}

reCh16:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh16
	}
	if !bytes.Equal(ch.Storage.Quarantine.TheirActionsToExecuteHash, actA.AsCell().Hash()) {
		t.Log("waiting for actions hash replication")
		goto reCh16
	}
	if ch.Storage.Quarantine.OurSettlementFinalized {
		t.Fatal("A side settlement should be not finalized")
	}
	log.Println("settlement finalized, action hash replicated")

reCh17:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh17
	}
	if ch.calcState() != ChannelUniversalStatusExecutingActions {
		t.Log("waiting for settlement period end")
		goto reCh17
	}
	log.Println("ready for action")

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	actAProof, _ = actA.AsCell().CreateProof(sk)
	actBProof, _ := actB.AsCell().CreateProof(sk)

	body, err = ch2.PrepareExecuteActionsMessage(bKey, actC, actAProof, actBProof)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

reTx1:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Storage.PartyAddress.Value,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx1
	}
	log.Println("executed action B external on contract A:", base64.StdEncoding.EncodeToString(tx.Hash))

reCh18:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh18
	}
	if ch2.calcState() != ChannelUniversalStatusAwaitingFinalization {
		t.Log("waiting for execute period end")
		goto reCh18
	}
	log.Println("ready for finalize")

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	body, _ = tlb.ToCell(FinishUncooperativeClose{})
reTx2:
	tx, _, _, err = api.SendExternalMessageWaitTransaction(ctx, &tlb.ExternalMessage{
		DstAddr: ch2.Storage.PartyAddress.Value,
		Body:    body,
	})
	if err != nil {
		t.Log(fmt.Errorf("failed to send tx: %w", err))
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}
		goto reTx2
	}

reCh19:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch, err = client.GetAsyncUniversalChannel(ctx, channelAddr, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get channel: %w, retrying", err))
		goto reCh19
	}
	if ch.Storage.Initialized {
		t.Log("waiting for uninit")
		goto reCh19
	}

reCh20:
	block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	ch2, err = client.GetAsyncUniversalChannel(ctx, ch.Storage.PartyAddress.Value, true)
	if err != nil {
		t.Log(fmt.Errorf("failed to get party channel: %w, retrying", err))
		goto reCh20
	}
	if ch2.Storage.Initialized {
		t.Log("waiting for uninit")
		goto reCh20
	}

	log.Println("done", channelAddr.String())
}

func TestClient_AsyncChannelFullFlowTon(t *testing.T) {
	client := NewPaymentChannelClient(client2.NewTON(api))

	chID, err := RandomChannelID()
	if err != nil {
		t.Fatal(err)
	}

	_, aKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bPubKey, bKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := wallet.FromSeed(api, _seed, wallet.HighloadV2R2)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.Address().String())

	body, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       30,
		MisbehaviorFine:          tlb.MustFromTON("0.00"),
		ConditionalCloseDuration: 30,
	}, PaymentConfig{
		StorageFee:     tlb.MustFromTON("0.03"),
		DestA:          w.WalletAddress(),
		DestB:          address.MustParseAddr("EQBletedrsSdih8H_-bR0cDZhdbLRy73ol6psGCrRKDahFju"),
		CurrencyConfig: CurrencyConfigTon{},
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(context.Background(), tlb.MustFromTON("0.3"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err := client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}
	log.Println("channel is active")

	// topup
	c, err := tlb.ToCell(TopupBalance{
		IsA: true,
	})
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	tx, block, err := w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.2"), c))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	log.Println("top up done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.DepositA.Nano().Cmp(tlb.MustFromTON("0.17").Nano()) != 0 {
		t.Fatal("balance incorrect ", ch.Storage.Balance.DepositA.String(), ch.Storage.Balance.DepositB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.SentA = ch.Storage.Balance.SentA
	coc.Signed.SentB = ch.Storage.Balance.SentB
	coc.Signed.ChannelID = chID
	coc.Signed.SeqnoA = 1
	coc.Signed.SeqnoB = 1
	coc.Signed.WithdrawA = tlb.MustFromTON("0.02")
	coc.Signed.WithdrawB = tlb.ZeroCoins
	toSign, err := tlb.ToCell(coc.Signed)
	if err != nil {
		t.Fatal(err)
	}
	coc.SignatureA.Value = toSign.Sign(aKey)
	coc.SignatureB.Value = toSign.Sign(bKey)

	c, err = tlb.ToCell(coc)
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.2"), c))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.WithdrawA.Nano().Cmp(tlb.MustFromTON("0.02").Nano()) != 0 {
		t.Fatal("withdrawal incorrect after withdraw", ch.Storage.Balance.WithdrawA.String(), ch.Storage.Balance.WithdrawB.String())
	}

	log.Println("withdraw done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	var msg = StartUncooperativeClose{
		IsSignedByA: true,
	}

	bodyA := SemiChannelBody{
		Seqno: 2,
		Sent:  tlb.MustFromTON("0.01"),
	}
	condA := cell.NewDict(32)

	bodyB := SemiChannelBody{
		Seqno:            2,
		Sent:             tlb.MustFromTON("0"),
		ConditionalsHash: make([]byte, 32),
	}

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)

	vch := VirtualChannel{
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.03").Nano(),
		Fee:      tlb.MustFromTON("0.01").Nano(),
		Prepay:   tlb.MustFromTON("0.00").Nano(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}

	_ = condA.SetIntKey(big.NewInt(0), vch.Serialize())
	bodyA.ConditionalsHash = condA.AsCell().Hash()

	msg.Signed.ChannelID = chID
	msg.Signed.A = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyA,
			CounterpartyData: &bodyB,
		},
	}
	toSign, err = tlb.ToCell(msg.Signed.A.State)
	if err != nil {
		t.Fatal(err)
	}
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)

	msg.Signed.B = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyB,
			CounterpartyData: &bodyA,
		},
	}
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	toSign, _ = tlb.ToCell(msg.Signed)
	msg.Signature.Value = toSign.Sign(aKey)

	msgCell, _ := tlb.ToCell(msg)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok := false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusClosureStarted {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status not updated")
	}

	challenge := ChallengeQuarantinedState{
		IsChallengedByA: false,
	}
	challenge.Signed.ChannelID = chID
	bodyA.Seqno = 3
	bodyA.Sent = tlb.MustFromTON("0.01") // 0.01 + 0.01 + 0.02
	msg.Signed.A.State.Data = bodyA
	toSign, _ = tlb.ToCell(msg.Signed.A.State)
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	challenge.Signed.B = msg.Signed.B
	challenge.Signed.A = msg.Signed.A
	toSign, _ = tlb.ToCell(challenge.Signed)
	challenge.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(challenge)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("challenge started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok = false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusSettlingConditionals {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status not updated")
	}

	cond := SettleConditionals{
		IsFromA: false,
	}

	state := VirtualChannelState{
		Amount: tlb.MustFromTON("0.03").Nano(),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	var sk = cell.CreateProofSkeleton()
	sk.SetRecursive()
	pp, _ := condA.AsCell().CreateProof(sk)

	cond.Signed.ChannelID = chID
	cond.Signed.ConditionalsProof = pp
	cond.Signed.ConditionalsToSettle = cell.NewDict(32)
	_ = cond.Signed.ConditionalsToSettle.SetIntKey(big.NewInt(0), condInput)
	toSign, _ = tlb.ToCell(cond.Signed)
	cond.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(cond)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("conditions settled, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	comp := tx.Description.(tlb.TransactionDescriptionOrdinary).ComputePhase.Phase.(tlb.ComputePhaseVM)
	if comp.Details.ExitCode != 0 {
		t.Fatal(fmt.Errorf("failed to settle conditions, exit code: %d", comp.Details.ExitCode))
	}

	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}
		json.NewEncoder(os.Stdout).Encode(ch.Storage)

		if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.05").Nano()) != 0 ||
			ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
			continue
		}

		if ch.Status != ChannelStatusAwaitingFinalization {
			continue
		}
		break
	}

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.05").Nano()) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
		t.Fatal("channel seqno incorrect")
	}

	if ch.Status != ChannelStatusAwaitingFinalization {
		t.Fatal("channel status incorrect")
	}

	msgCell, _ = tlb.ToCell(FinishUncooperativeClose{})
	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close finished, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusUninitialized {
		t.Fatal("channel status incorrect")
	}
	log.Println("done", channelAddr.String())
}

func TestClient_AsyncChannelFullFlowJetton(t *testing.T) {
	master := address.MustParseAddr("EQBo66DB8ylazO91EErTqbKJervBx_DFjQdP8jqKLYmuDrY6")
	jettonClient := jetton.NewJettonMasterClient(api, master)

	client := NewPaymentChannelClient(client2.NewTON(api))

	chID, err := RandomChannelID()
	if err != nil {
		t.Fatal(err)
	}

	_, aKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bPubKey, bKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := wallet.FromSeed(api, _seed, wallet.V3)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.Address().String())

	body, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       30,
		MisbehaviorFine:          tlb.MustFromTON("0.00"),
		ConditionalCloseDuration: 30,
	}, PaymentConfig{
		StorageFee: tlb.MustFromTON("0.25"),
		DestA:      w.WalletAddress(),
		DestB:      address.MustParseAddr("EQBletedrsSdih8H_-bR0cDZhdbLRy73ol6psGCrRKDahFju"),
		CurrencyConfig: CurrencyConfigJetton{
			Info: CurrencyConfigJettonInfo{
				Master: master,
			},
		},
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(context.Background(), tlb.MustFromTON("0.3"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err := client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}
	log.Println("channel is active")

	// topup
	c, err := tlb.ToCell(TopupBalance{
		IsA: true,
	})
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	jw, err := jettonClient.GetJettonWallet(context.Background(), w.WalletAddress())
	if err != nil {
		t.Fatal(err)
	}

	jMsg, err := jw.BuildTransferPayloadV2(channelAddr, w.WalletAddress(), tlb.MustFromTON("0.17"), tlb.MustFromTON("0.1"), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	log.Println("jetton wallet", jw.Address().String())
	tx, block, err := w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(jw.Address(), tlb.MustFromTON("0.2"), jMsg))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 8).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	log.Println("jetton top up done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.DepositA.Nano().Cmp(tlb.MustFromTON("0.17").Nano()) != 0 {
		t.Fatal("deposit incorrect ", ch.Storage.Balance.DepositA.String(), ch.Storage.Balance.DepositB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.SentA = ch.Storage.Balance.SentA
	coc.Signed.SentB = ch.Storage.Balance.SentB
	coc.Signed.ChannelID = chID
	coc.Signed.SeqnoA = 1
	coc.Signed.SeqnoB = 1
	coc.Signed.WithdrawA = tlb.MustFromTON("0.02")
	coc.Signed.WithdrawB = tlb.ZeroCoins
	toSign, err := tlb.ToCell(coc.Signed)
	if err != nil {
		t.Fatal(err)
	}
	coc.SignatureA.Value = toSign.Sign(aKey)
	coc.SignatureB.Value = toSign.Sign(bKey)

	c, err = tlb.ToCell(coc)
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.2"), c))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.WithdrawA.Nano().Cmp(tlb.MustFromTON("0.02").Nano()) != 0 {
		t.Fatal("withdrawal incorrect after withdraw", ch.Storage.Balance.WithdrawA.String(), ch.Storage.Balance.WithdrawB.String())
	}

	log.Println("withdraw done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	var msg = StartUncooperativeClose{
		IsSignedByA: true,
	}

	bodyA := SemiChannelBody{
		Seqno: 2,
		Sent:  tlb.MustFromTON("0.01"),
	}
	condA := cell.NewDict(32)

	bodyB := SemiChannelBody{
		Seqno:            2,
		Sent:             tlb.MustFromTON("0"),
		ConditionalsHash: make([]byte, 32),
	}

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)

	vch := VirtualChannel{
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.03").Nano(),
		Fee:      tlb.MustFromTON("0.01").Nano(),
		Prepay:   tlb.MustFromTON("0.01").Nano(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}

	_ = condA.SetIntKey(big.NewInt(0), vch.Serialize())
	bodyA.ConditionalsHash = condA.AsCell().Hash()

	msg.Signed.ChannelID = chID
	msg.Signed.A = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyA,
			CounterpartyData: &bodyB,
		},
	}
	toSign, err = tlb.ToCell(msg.Signed.A.State)
	if err != nil {
		t.Fatal(err)
	}
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)

	msg.Signed.B = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyB,
			CounterpartyData: &bodyA,
		},
	}
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	toSign, _ = tlb.ToCell(msg.Signed)
	msg.Signature.Value = toSign.Sign(aKey)

	msgCell, _ := tlb.ToCell(msg)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok := false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusClosureStarted {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status not updated")
	}

	challenge := ChallengeQuarantinedState{
		IsChallengedByA: false,
	}
	challenge.Signed.ChannelID = chID
	bodyA.Seqno = 3
	bodyA.Sent = tlb.MustFromTON("0.01") // 0.01 + 0.01 + 0.02
	msg.Signed.A.State.Data = bodyA
	toSign, _ = tlb.ToCell(msg.Signed.A.State)
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	challenge.Signed.B = msg.Signed.B
	challenge.Signed.A = msg.Signed.A
	toSign, _ = tlb.ToCell(challenge.Signed)
	challenge.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(challenge)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("challenge started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok = false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusSettlingConditionals {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status incorrect", ch.Status)
	}

	cond := SettleConditionals{
		IsFromA: false,
	}

	state := VirtualChannelState{
		Amount: tlb.MustFromTON("0.03").Nano(),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	var sk = cell.CreateProofSkeleton()
	sk.SetRecursive()
	pp, _ := condA.AsCell().CreateProof(sk)

	cond.Signed.ChannelID = chID
	cond.Signed.ConditionalsProof = pp
	cond.Signed.ConditionalsToSettle = cell.NewDict(32)
	_ = cond.Signed.ConditionalsToSettle.SetIntKey(big.NewInt(0), condInput)
	toSign, _ = tlb.ToCell(cond.Signed)
	cond.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(cond)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("conditions settled, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	comp := tx.Description.(tlb.TransactionDescriptionOrdinary).ComputePhase.Phase.(tlb.ComputePhaseVM)
	if comp.Details.ExitCode != 0 {
		t.Fatal(fmt.Errorf("failed to settle conditions, exit code: %d", comp.Details.ExitCode))
	}

	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}
		json.NewEncoder(os.Stdout).Encode(ch.Storage)

		if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.04").Nano()) != 0 ||
			ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
			continue
		}

		if ch.Status != ChannelStatusAwaitingFinalization {
			continue
		}
		break
	}

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.04").Nano()) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
		t.Fatal("channel seqno incorrect")
	}

	if ch.Status != ChannelStatusAwaitingFinalization {
		t.Fatal("channel status incorrect")
	}

	msgCell, _ = tlb.ToCell(FinishUncooperativeClose{})
	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close finished, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusUninitialized {
		t.Fatal("channel status incorrect")
	}
	log.Println("done", channelAddr.String())
}

func TestClient_AsyncChannelFullFlowEC(t *testing.T) {
	const ecID = 100

	client := NewPaymentChannelClient(client2.NewTON(api))

	chID, err := RandomChannelID()
	if err != nil {
		t.Fatal(err)
	}

	_, aKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bPubKey, bKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := wallet.FromSeed(api, _seed, wallet.HighloadV2R2)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.WalletAddress().String())

	body, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       30,
		MisbehaviorFine:          tlb.MustFromTON("0.00"),
		ConditionalCloseDuration: 30,
	}, PaymentConfig{
		StorageFee: tlb.MustFromTON("0.1"),
		DestA:      w.WalletAddress(),
		DestB:      address.MustParseAddr("EQBletedrsSdih8H_-bR0cDZhdbLRy73ol6psGCrRKDahFju"),
		CurrencyConfig: CurrencyConfigEC{
			ID: ecID,
		},
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(context.Background(), tlb.MustFromTON("0.3"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}
	log.Println("channel deployed:", channelAddr.String())

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err := client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}
	log.Println("channel is active")

	// topup
	c, err := tlb.ToCell(TopupBalance{
		IsA: true,
	})
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	ecMsg := wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.2"), c)
	ecMsg.InternalMessage.ExtraCurrencies = cell.NewDict(32)
	ecMsg.InternalMessage.ExtraCurrencies.SetIntKey(big.NewInt(ecID), cell.BeginCell().MustStoreVarUInt(1000, 32).EndCell())

	tx, block, err := w.SendWaitTransaction(context.Background(), ecMsg)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}
	log.Println("ec top up done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.DepositA.Nano().Cmp(big.NewInt(1000)) != 0 {
		t.Fatal("deposit incorrect ", ch.Storage.Balance.DepositA.String(), ch.Storage.Balance.DepositB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.SentA = ch.Storage.Balance.SentA
	coc.Signed.SentB = ch.Storage.Balance.SentB
	coc.Signed.ChannelID = chID
	coc.Signed.SeqnoA = 1
	coc.Signed.SeqnoB = 1
	coc.Signed.WithdrawA = tlb.FromNanoTON(big.NewInt(300))
	coc.Signed.WithdrawB = tlb.ZeroCoins
	toSign, err := tlb.ToCell(coc.Signed)
	if err != nil {
		t.Fatal(err)
	}
	coc.SignatureA.Value = toSign.Sign(aKey)
	coc.SignatureB.Value = toSign.Sign(bKey)

	c, err = tlb.ToCell(coc)
	if err != nil {
		t.Fatal("failed to serialize message to cell:", err)
	}

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.2"), c))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.WithdrawA.Nano().Cmp(big.NewInt(300)) != 0 {
		t.Fatal("balance incorrect after withdraw", ch.Storage.Balance.WithdrawA.String(), ch.Storage.Balance.WithdrawB.String())
	}

	log.Println("withdraw done, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	var msg = StartUncooperativeClose{
		IsSignedByA: true,
	}

	bodyA := SemiChannelBody{
		Seqno: 2,
		Sent:  tlb.FromNanoTON(big.NewInt(50)),
	}
	condA := cell.NewDict(32)

	bodyB := SemiChannelBody{
		Seqno:            2,
		Sent:             tlb.MustFromTON("0"),
		ConditionalsHash: make([]byte, 32),
	}

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)

	vch := VirtualChannel{
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.03").Nano(),
		Fee:      big.NewInt(1),
		Prepay:   big.NewInt(0),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}

	_ = condA.SetIntKey(big.NewInt(0), vch.Serialize())
	bodyA.ConditionalsHash = condA.AsCell().Hash()

	msg.Signed.ChannelID = chID
	msg.Signed.A = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyA,
			CounterpartyData: &bodyB,
		},
	}
	toSign, err = tlb.ToCell(msg.Signed.A.State)
	if err != nil {
		t.Fatal(err)
	}
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)

	msg.Signed.B = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyB,
			CounterpartyData: &bodyA,
		},
	}
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	toSign, _ = tlb.ToCell(msg.Signed)
	msg.Signature.Value = toSign.Sign(aKey)

	msgCell, _ := tlb.ToCell(msg)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok := false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusClosureStarted {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status not updated")
	}

	challenge := ChallengeQuarantinedState{
		IsChallengedByA: false,
	}
	challenge.Signed.ChannelID = chID
	bodyA.Seqno = 3
	bodyA.Sent = tlb.FromNanoTON(big.NewInt(100)) // 0.01 + 0.01 + 0.02
	msg.Signed.A.State.Data = bodyA
	toSign, _ = tlb.ToCell(msg.Signed.A.State)
	msg.Signed.A.Signature.Value = toSign.Sign(aKey)
	toSign, _ = tlb.ToCell(msg.Signed.B.State)
	msg.Signed.B.Signature.Value = toSign.Sign(bKey)

	challenge.Signed.B = msg.Signed.B
	challenge.Signed.A = msg.Signed.A
	toSign, _ = tlb.ToCell(challenge.Signed)
	challenge.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(challenge)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("challenge started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ok = false
	for i := 0; i < 10; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}

		if ch.Status != ChannelStatusSettlingConditionals {
			log.Println("channel status not yet updated, waiting", ch.Status)
			continue
		}
		ok = true
		break
	}

	if !ok {
		t.Fatal("channel status not updated")
	}

	cond := SettleConditionals{
		IsFromA: false,
	}

	state := VirtualChannelState{
		Amount: big.NewInt(300),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	var sk = cell.CreateProofSkeleton()
	sk.SetRecursive()
	pp, _ := condA.AsCell().CreateProof(sk)

	cond.Signed.ChannelID = chID
	cond.Signed.ConditionalsProof = pp
	cond.Signed.ConditionalsToSettle = cell.NewDict(32)
	_ = cond.Signed.ConditionalsToSettle.SetIntKey(big.NewInt(0), condInput)
	toSign, _ = tlb.ToCell(cond.Signed)
	cond.Signature.Value = toSign.Sign(bKey)

	msgCell, _ = tlb.ToCell(cond)

	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("conditions settled, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	comp := tx.Description.(tlb.TransactionDescriptionOrdinary).ComputePhase.Phase.(tlb.ComputePhaseVM)
	if comp.Details.ExitCode != 0 {
		t.Fatal(fmt.Errorf("failed to settle conditions, exit code: %d", comp.Details.ExitCode))
	}

	for i := 0; i < 15; i++ {
		block, err = api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(context.Background())
		if err != nil {
			t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
		}

		ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
		if err != nil {
			t.Fatal(fmt.Errorf("failed to get channel: %w", err))
		}
		json.NewEncoder(os.Stdout).Encode(ch.Storage)

		if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(big.NewInt(401)) != 0 ||
			ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
			continue
		}

		if ch.Status != ChannelStatusAwaitingFinalization {
			continue
		}
		break
	}

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(big.NewInt(401)) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
		t.Fatal("channel seqno incorrect")
	}

	if ch.Status != ChannelStatusAwaitingFinalization {
		t.Fatal("channel status incorrect")
	}

	msgCell, _ = tlb.ToCell(FinishUncooperativeClose{})
	tx, block, err = w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close finished, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusUninitialized {
		t.Fatal("channel status incorrect")
	}
	log.Println("done", channelAddr.String())
}
