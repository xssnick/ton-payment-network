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
	"github.com/xssnick/tonutils-go/ton/jetton"
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

func TestClient_AsyncChannelFullFlowTon(t *testing.T) {
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

	body, code, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       20,
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

	ch, err := client.GetAsyncChannel(context.Background(), block, channelAddr, true)
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(tlb.MustFromTON("0.17").Nano()) != 0 {
		t.Fatal("balance incorrect ", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.BalanceA = ch.Storage.Balance.BalanceA
	coc.Signed.BalanceB = ch.Storage.Balance.BalanceB
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(tlb.MustFromTON("0.15").Nano()) != 0 {
		t.Fatal("balance incorrect after withdraw", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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

	block, err = api.WaitForBlock(block.SeqNo + 5).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}
	json.NewEncoder(os.Stdout).Encode(ch.Storage)

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.05").Nano()) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
		t.Fatal(fmt.Errorf("incorrect state in quarantine"), ch.Storage.Quarantine.StateA.Sent.String(), ch.Storage.Quarantine.StateA.Seqno, ch.Storage.Quarantine.StateB.Seqno)
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
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

	w, err := wallet.FromSeed(api, _seed, wallet.V3)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to init wallet: %w", err))
	}
	log.Println("wallet:", w.Address().String())

	body, code, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       20,
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

	ch, err := client.GetAsyncChannel(context.Background(), block, channelAddr, true)
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(tlb.MustFromTON("0.17").Nano()) != 0 {
		t.Fatal("balance incorrect ", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.BalanceA = ch.Storage.Balance.BalanceA
	coc.Signed.BalanceB = ch.Storage.Balance.BalanceB
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(tlb.MustFromTON("0.15").Nano()) != 0 {
		t.Fatal("balance incorrect after withdraw", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
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
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
		Comment:  cell.BeginCell().MustStoreUInt(0xD9, 8).MustStoreUInt(734962347842312921, 61).EndCell(),
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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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

	block, err = api.WaitForBlock(block.SeqNo + 6).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}
	json.NewEncoder(os.Stdout).Encode(ch.Storage)

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(tlb.MustFromTON("0.05").Nano()) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
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

	body, code, data, err := client.GetDeployAsyncChannelParams(chID, true, aKey, bPubKey, ClosingConfig{
		QuarantineDuration:       20,
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

	ch, err := client.GetAsyncChannel(context.Background(), block, channelAddr, true)
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(big.NewInt(1000)) != 0 {
		t.Fatal("balance incorrect ", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
	}

	json.NewEncoder(os.Stdout).Encode(ch.Storage)
	log.Println("balance verified, starting withdraw")

	coc := CooperativeCommit{}
	coc.Signed.BalanceA = ch.Storage.Balance.BalanceA
	coc.Signed.BalanceB = ch.Storage.Balance.BalanceB
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Storage.Balance.BalanceA.Nano().Cmp(big.NewInt(700)) != 0 {
		t.Fatal("balance incorrect after withdraw", ch.Storage.Balance.BalanceA.String(), ch.Storage.Balance.BalanceB.String())
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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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

	block, err = api.WaitForBlock(block.SeqNo + 4).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

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
		Amount: tlb.FromNanoTON(big.NewInt(300)),
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

	block, err = api.WaitForBlock(block.SeqNo + 6).GetMasterchainInfo(context.Background())
	if err != nil {
		t.Fatal(fmt.Errorf("failed to wait for block: %w", err))
	}

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}
	json.NewEncoder(os.Stdout).Encode(ch.Storage)

	if ch.Storage.Quarantine.StateA.Sent.Nano().Cmp(big.NewInt(401)) != 0 ||
		ch.Storage.Quarantine.StateA.Seqno != 3 || ch.Storage.Quarantine.StateB.Seqno != 2 {
		t.Fatal(fmt.Errorf("incorrect state in quarantine"), ch.Storage.Quarantine.StateA.Sent.Nano().String(), ch.Storage.Quarantine.StateA.Seqno, ch.Storage.Quarantine.StateB.Seqno)
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

	ch, err = client.GetAsyncChannel(context.Background(), block, channelAddr, true)
	if err != nil {
		t.Fatal(fmt.Errorf("failed to get channel: %w", err))
	}

	if ch.Status != ChannelStatusUninitialized {
		t.Fatal("channel status incorrect")
	}
	log.Println("done", channelAddr.String())
}
