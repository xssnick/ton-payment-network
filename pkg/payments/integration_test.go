package payments

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"log"
	"math/big"
	"os"
	"strings"
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

var _seed = strings.Split(os.Getenv("WALLET_SEED"), " ")

func TestClient_AsyncChannelFullFlow(t *testing.T) {
	client := NewPaymentChannelClient(api)

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

	body, code, data, err := client.GetDeployAsyncChannelParams(chID, true, tlb.MustFromTON("0.1"), aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       20,
		MisbehaviorFine:          tlb.MustFromTON("0.00"),
		ConditionalCloseDuration: 30,
	}, PaymentConfig{
		ExcessFee: tlb.MustFromTON("0.00"),
		DestA:     w.Address(),
		DestB:     address.MustParseAddr("EQBletedrsSdih8H_-bR0cDZhdbLRy73ol6psGCrRKDahFju"),
	})
	if err != nil {
		t.Fatal(fmt.Errorf("failed to build deploy channel params: %w", err))
	}

	channelAddr, _, block, err := w.DeployContractWaitTransaction(context.Background(), tlb.MustFromTON("0.12"), body, code, data)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to deploy channel: %w", err))
	}

	ch, err := client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusOpen {
		t.Fatal("channel status incorrect")
	}

	if ch.Storage.BalanceA.Nano().Cmp(tlb.MustFromTON("0.1").Nano()) != 0 {
		t.Fatal("balance incorrect")
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("channel deployed:", channelAddr.String())

	var msg = StartUncooperativeClose{
		IsSignedByA: true,
	}

	bodyA := SemiChannelBody{
		Seqno:        2,
		Sent:         tlb.MustFromTON("0.01"),
		Conditionals: cell.NewDict(32),
	}

	bodyB := SemiChannelBody{
		Seqno:        1,
		Sent:         tlb.MustFromTON("0"),
		Conditionals: cell.NewDict(32),
	}

	vPubKey, vKey, _ := ed25519.GenerateKey(nil)

	vch := VirtualChannel{
		Key:      vPubKey,
		Capacity: tlb.MustFromTON("0.03").Nano(),
		Fee:      tlb.MustFromTON("0.01").Nano(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
	}

	_ = bodyA.Conditionals.SetIntKey(big.NewInt(0), vch.Serialize())

	msg.Signed.ChannelID = chID
	msg.Signed.A = SignedSemiChannel{
		State: SemiChannel{
			ChannelID:        chID,
			Data:             bodyA,
			CounterpartyData: &bodyB,
		},
	}
	toSign, _ := tlb.ToCell(msg.Signed.A.State)
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

	tx, block, err := w.SendWaitTransaction(context.Background(), wallet.SimpleMessage(channelAddr, tlb.MustFromTON("0.02"), msgCell))
	if err != nil {
		t.Fatal(fmt.Errorf("failed to send tx: %w", err))
	}
	log.Println("uncooperative close started, tx:", base64.StdEncoding.EncodeToString(tx.Hash))

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusClosureStarted {
		t.Fatal("channel status incorrect")
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

	time.Sleep(10 * time.Second)

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusSettlingConditionals {
		t.Fatal("channel status incorrect")
	}

	cond := SettleConditionals{
		IsFromA: false,
	}

	state := VirtualChannelState{
		Amount: tlb.MustFromTON("0.03"),
	}
	state.Sign(vKey)
	condInput, _ := state.ToCell()

	cond.Signed.ChannelID = chID
	cond.Signed.B = msg.Signed.B
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

	comp := tx.Description.Description.(tlb.TransactionDescriptionOrdinary).ComputePhase.Phase.(tlb.ComputePhaseVM)
	if comp.Details.ExitCode != 0 {
		t.Fatal(fmt.Errorf("failed to settle conditions, exit code: %d", comp.Details.ExitCode))
	}

	time.Sleep(30 * time.Second)

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}
	json.NewEncoder(os.Stdout).Encode(ch.Storage)

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.05").Nano()) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 1 {
		t.Fatal(fmt.Errorf("incorrect state in quarantine"))
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusUninitialized {
		t.Fatal("channel status incorrect")
	}
	log.Println("done", channelAddr.String())
}
