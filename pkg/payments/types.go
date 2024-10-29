package payments

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

// AsyncPaymentChannelCodeBoC https://github.com/xssnick/tonutils-contracts/tree/master/contracts/payments
const AsyncPaymentChannelCodeBoC = "b5ee9c7241022901000824000114ff00f4a413f4bcf2c80b0102012003020006f2f01602014807040201200605008dbd0cabbf8047c20c8b870fc253748b8f07c256840206b90fd0018c020eb90fd0018b8eb90e98f987c23b7882908507c11de491839707c23b788507c23b789507c11de48b9f03a40075bc7fe3bf8047c25e87d007d207d20184100d0caf6a1ec7c217c21b7817c227c22b7817c237c23fc247c24b7817c2524c3b7818823881b22a0219840202cb180804b7a376d176fde98f90c10833e3e940dd4998f8057010c108070310615d4a98f805ed98f010c1082abbac3f5d474298ed9e6d98f010c1083cd09377dd474298ed9e6d98f010c1080f8a8d67dd474298ed9e6d98f010c108044755195d401716130903568e8531db3cdb31e021821066f6f069ba8e8531db3cdb31e030821025432a91ba8e84db3cdb31e0840ff2f00e0b0a00d477f008f84a6eb3f2e06bf84ad0d33ffa00d3ff31d33ffa00d3ff31d31f30f8476f10a0f8476f12a0f823b9f2e071f800f84221a023a1f862f8435003a058a1f863f843c1009af842f843a0f86270f863def842c1009af843f842a0f86370f863de01a4f868a4f869f00d01f273f008f84a6eb3f2e06bd2008308d71820f9012392f84492f845e24130f910f2e065d31f018210436c436ebaf2e068f84601d37f59baf2e072f404d4d430f84ad0d33ffa00d3ffd33ffa00d3ffd31ff8476f105220a020f823b9f2e06ef8476f12a0f823bcf2e070d200d2003054767c935b5334de0bd739b30c01fef2d074d30701c303f2d07420d70bff01d74cf9000dd739b3f2d073d30701c303f2d07320d70bff5003bdf2d07301d74c8e270d8020f4966fa5208e178b085911118020f42e6fa1f2e073ed1e12da111da00c0e926c21e2b31ee63d0cf9001bbdf2d0740b926c219734341037054313e204c8cb3f5003fa02cbffcb3f58fa020d002013cbff12cb1f12ca00ca00c9f86af00901ce73f008f84a6eb3f2e06bd2008308d71820f9012392f84492f845e24130f910f2e065d31f01821043686751baf2e068f84601d37f59baf2e072d401d08308d718d74c20f900f8444130f910f2e065d001d430d08308d718d74c20f900f8454130f910f2e065d0010f01fcd31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e2303205d31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e230325339be5381beb0f8495250beb0f8485290beb01002fc5237be16b05262beb0f2e06927c20097f84918bef2e0699137e222c20097f84813bef2e0699132e2f84ad0d33ffa00d3ffd33ffa00d3ffd31ff8476f105220a0f823bcf2e06fd200d20030b3f2e073209c3537373a5274bc5263bc12b18e11323939395250bc5299bc18b14650134440e25319bab3f2e06d9130e30d04c81211003acb3f5003fa0214cbff12cb3f5003fa02cbffcb1fca00cf83c9f86af009002496f8476f1114a098f8476f1117a00603e20301cc73f008f84a6ef2e06ad2008308d71820f9012392f84492f845e24130f910f2e065d31f018210556e436cbaf2e068f84601d37f59baf2e072d401d08308d718d74c20f900f8444130f910f2e065d001d430d08308d718d74c20f900f8454130f910f2e065d0011401fed31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e2303205d31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e23032f8485280bef8495250beb0524bbe1ab0527abe19150062b05215be14b05248be17b0f2e069f82304c8cb3f5003fa0214cbff14cb3f58fa0212cbffcb1fca00cf81c9f86af800f00900e473f008d401d001d401d021f90102d31f01821043436d74baf2e068f84601d37f59baf2e072f844544355f910f8454330f910b0f2e065d33fd33f30f84822b9f84922b9b0f2e06c21f86820f869f84a6e915b8e19f84ad0d33ffa003171d721d33f305033bc02bcb1936df86adee2f800f00900b477f008d401d001d401d021f90102d31f018210436c6f73baf2e068f84601d37f59baf2e072f844544355f910f8454330f910b0f2e065fa0001f862fa0001f863d33fd33f30f84822b9f84922b9b0f2e06c01f868f869f800f00d02012024190201201d1a0201481c1b006f3e12f43e800c7e903e900c3e09dbc41cbe10d62f24cc20c1b7be10fe11963c033e10be11a04020bc031c3e185c3e189c3e18db7e1abc0260003d20843777222e9c20043232c145b394013e808532da84b2c7f2dff2407ec020020120211e020120201f00d51dfc023e106cfcb819348020c235c6083e4040e4be1124be117890cc3e443cb81974c7c060841a5b9a5d2ebcb81a3e118074dfd66ebcb81cbe803e800c3e1094882fbe10d4882fac3cb819807e18be18fe12f43e800c3e10be10e800683e09dbc42e7cb8199ffe187c026000531c3c023e106cfcb8193e803e800c3e1096283e18be10c0683e18fe10be10e83e09dbc42efcb819bc02600201202322008d3e13723e11be117e113e10540132803e10be80be10fe8084f2ffc4b2fff2dffc01c87080a7fe12be127e121400f2c7c4b2c7fd0037807081a53e12c073253e130073b8b27b5520008b083e1b7b51343480007e187e80007e18be80007e18f4ffc07e1934ffc07e1974dfc07e19bc00c87080a7f4c7c07e1a34c7c07e1a7d01007e1ab7807081a535007e1af7be1b200201202625002df7c23b7897c23b78864658ffc23b788fd01658fe480e640201202827001d5d401d0d31ffa00d31f306f03f867800054f01685ac2c833"

var AsyncPaymentChannelCode = func() *cell.Cell {
	codeBoC, _ := hex.DecodeString(AsyncPaymentChannelCodeBoC)
	code, _ := cell.FromBOC(codeBoC)
	return code
}()
var AsyncPaymentChannelCodeHash = AsyncPaymentChannelCode.Hash()

// Data types

type Signature struct {
	Value []byte `tlb:"bits 512"`
}

type ClosingConfig struct {
	QuarantineDuration       uint32    `tlb:"## 32"`
	MisbehaviorFine          tlb.Coins `tlb:"."`
	ConditionalCloseDuration uint32    `tlb:"## 32"`
}

type ConditionalPayment struct {
	Amount    tlb.Coins  `tlb:"."`
	Condition *cell.Cell `tlb:"."`
}

type SemiChannelBody struct {
	Seqno            uint64    `tlb:"## 64"`
	Sent             tlb.Coins `tlb:"."`
	ConditionalsHash []byte    `tlb:"bits 256"`
}

type SemiChannel struct {
	_                tlb.Magic        `tlb:"#43685374"`
	ChannelID        ChannelID        `tlb:"bits 128"`
	Data             SemiChannelBody  `tlb:"."`
	CounterpartyData *SemiChannelBody `tlb:"maybe ^"`
}

type SignedSemiChannel struct {
	Signature Signature   `tlb:"."`
	State     SemiChannel `tlb:"^"`
}

type QuarantinedState struct {
	StateA            SemiChannelBody `tlb:"."`
	StateB            SemiChannelBody `tlb:"."`
	QuarantineStarts  uint32          `tlb:"## 32"`
	StateCommittedByA bool            `tlb:"bool"`
	StateChallenged   bool            `tlb:"bool"`
}

type PaymentConfig struct {
	ExcessFee tlb.Coins        `tlb:"."`
	DestA     *address.Address `tlb:"addr"`
	DestB     *address.Address `tlb:"addr"`
}

type JettonConfig struct {
	Root    *address.Address `tlb:"addr"`
	Wallet  *address.Address `tlb:"addr"`
	Balance tlb.Coins        `tlb:"."`
}

type AsyncJettonChannelStorageData struct {
	Initialized     bool              `tlb:"bool"`
	BalanceA        tlb.Coins         `tlb:"."`
	BalanceB        tlb.Coins         `tlb:"."`
	KeyA            []byte            `tlb:"bits 256"`
	KeyB            []byte            `tlb:"bits 256"`
	ChannelID       ChannelID         `tlb:"bits 128"`
	JettonConfig    *JettonConfig     `tlb:"-"`
	ClosingConfig   ClosingConfig     `tlb:"^"`
	CommittedSeqnoA uint32            `tlb:"## 32"`
	CommittedSeqnoB uint32            `tlb:"## 32"`
	Quarantine      *QuarantinedState `tlb:"maybe ^"`
	Payments        PaymentConfig     `tlb:"^"`
}

/// Messages

type InitChannel struct {
	_         tlb.Magic `tlb:"#0e0620c2"`
	IsA       bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_         tlb.Magic `tlb:"#696e6974"`
		ChannelID ChannelID `tlb:"bits 128"`
		BalanceA  tlb.Coins `tlb:"."`
		BalanceB  tlb.Coins `tlb:"."`
	} `tlb:"."`
}

type TopupBalance struct {
	_    tlb.Magic `tlb:"#67c7d281"`
	AddA tlb.Coins `tlb:"."`
	AddB tlb.Coins `tlb:"."`
}

type CooperativeClose struct {
	_          tlb.Magic `tlb:"#5577587e"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#436c6f73"`
		ChannelID ChannelID `tlb:"bits 128"`
		BalanceA  tlb.Coins `tlb:"."`
		BalanceB  tlb.Coins `tlb:"."`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type CooperativeCommit struct {
	_          tlb.Magic `tlb:"#79a126ef"`
	IsA        bool      `tlb:"bool"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#43436d74"`
		ChannelID ChannelID `tlb:"bits 128"`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type StartUncooperativeCloseBody struct {
	_         tlb.Magic         `tlb:"#556e436c"`
	ChannelID ChannelID         `tlb:"bits 128"`
	A         SignedSemiChannel `tlb:"^"`
	B         SignedSemiChannel `tlb:"^"`
}

type StartUncooperativeClose struct {
	_           tlb.Magic `tlb:"#1f151acf"`
	IsSignedByA bool      `tlb:"bool"`
	Signature   Signature `tlb:"."`
	Signed      struct {
		_         tlb.Magic         `tlb:"#556e436c"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type ChallengeQuarantinedState struct {
	_               tlb.Magic `tlb:"#088eaa32"`
	IsChallengedByA bool      `tlb:"bool"`
	Signature       Signature `tlb:"."`
	Signed          struct {
		_         tlb.Magic         `tlb:"#43686751"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type SettleConditionals struct {
	_         tlb.Magic `tlb:"#66f6f069"`
	IsFromA   bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                        tlb.Magic        `tlb:"#436c436e"`
		ChannelID                ChannelID        `tlb:"bits 128"`
		ConditionalsToSettle     *cell.Dictionary `tlb:"dict 32"`
		ConditionalsProof        *cell.Cell       `tlb:"^"`
		ConditionalsProofUpdated *cell.Cell       `tlb:"^"`
	} `tlb:"."`
}

type FinishUncooperativeClose struct {
	_ tlb.Magic `tlb:"#25432a91"`
}

func (s *SignedSemiChannel) Verify(key ed25519.PublicKey) error {
	if bytes.Equal(s.Signature.Value, make([]byte, 64)) &&
		s.State.Data.Sent.Nano().Sign() == 0 &&
		bytes.Equal(s.State.Data.ConditionalsHash, make([]byte, 32)) {
		// TODO: use more reliable approach
		// empty
		return nil
	}

	c, err := tlb.ToCell(s.State)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, c.Hash(2), s.Signature.Value) {
		log.Warn().Hex("sig", s.Signature.Value).Msg("invalid signature")
		return fmt.Errorf("invalid signature")
	}
	return nil
}

var ErrNotFound = fmt.Errorf("not found")

func FindVirtualChannel(conditionals *cell.Dictionary, key ed25519.PublicKey) (*big.Int, *VirtualChannel, error) {
	return FindVirtualChannelWithProof(conditionals, key, nil)
}

func FindVirtualChannelWithProof(conditionals *cell.Dictionary, key ed25519.PublicKey, proofRoot *cell.ProofSkeleton) (*big.Int, *VirtualChannel, error) {
	var tempProofRoot *cell.ProofSkeleton
	if proofRoot != nil {
		tempProofRoot = cell.CreateProofSkeleton()
	}

	idx := big.NewInt(int64(binary.LittleEndian.Uint32(key)))
	sl, proofBranch, err := conditionals.LoadValueWithProof(cell.BeginCell().MustStoreBigUInt(idx, 32).EndCell(), tempProofRoot)
	if err != nil {
		if errors.Is(err, cell.ErrNoSuchKeyInDict) {
			if proofRoot != nil {
				proofBranch.SetRecursive()
				proofRoot.Merge(tempProofRoot)
			}
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	vch, err := ParseVirtualChannelCond(sl)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse state of one of virtual channels")
	}

	if !bytes.Equal(vch.Key, key) {
		return nil, nil, ErrNotFound
	}

	if proofRoot != nil {
		proofBranch.SetRecursive()
		proofRoot.Merge(tempProofRoot)
	}
	return idx, vch, nil
}

func (s *SemiChannel) CheckSynchronized(with *SemiChannel) error {
	if !bytes.Equal(s.ChannelID, with.ChannelID) {
		return fmt.Errorf("diff channel id")
	}

	if with.CounterpartyData == nil {
		return fmt.Errorf("our state on their side is empty")
	}

	ourStateOnTheirSide, err := tlb.ToCell(with.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize our state on their side: %w", err)
	}
	ourState, err := tlb.ToCell(s.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize our state: %w", err)
	}

	if !bytes.Equal(ourStateOnTheirSide.Hash(2), ourState.Hash()) {
		return fmt.Errorf("our state on their side is diff")
	}

	if s.CounterpartyData == nil {
		return fmt.Errorf("their state on our side is empty")
	}

	theirStateOnOurSide, err := tlb.ToCell(s.CounterpartyData)
	if err != nil {
		return fmt.Errorf("failed to serialize their state on our side: %w", err)
	}
	theirState, err := tlb.ToCell(with.Data)
	if err != nil {
		return fmt.Errorf("failed to serialize their state: %w", err)
	}

	if !bytes.Equal(theirStateOnOurSide.Hash(2), theirState.Hash(2)) {
		return fmt.Errorf("their state on our side is diff")
	}

	return nil
}

func (s *SemiChannel) Dump() string {
	c, err := tlb.ToCell(s.Data)
	if err != nil {
		return "failed cell"
	}

	cpData := "none"
	if s.CounterpartyData != nil {
		cp, err := tlb.ToCell(s.CounterpartyData)
		if err != nil {
			return "failed cell"
		}
		cpData = fmt.Sprintf("(data_hash: %s seqno: %d; sent: %s; conditionals_hash: %s)",
			hex.EncodeToString(cp.Hash()[:8]),
			s.CounterpartyData.Seqno, s.CounterpartyData.Sent.String(), hex.EncodeToString(s.CounterpartyData.ConditionalsHash))
	}

	return fmt.Sprintf("data_hash: %s seqno: %d; sent: %s; conditionals_hash: %s; counterparty: %s",
		hex.EncodeToString(c.Hash()[:8]),
		s.Data.Seqno, s.Data.Sent.String(), hex.EncodeToString(s.Data.ConditionalsHash), cpData)
}

func (s *SemiChannelBody) Copy() (SemiChannelBody, error) {
	return SemiChannelBody{
		Seqno:            s.Seqno,
		Sent:             tlb.FromNanoTON(s.Sent.Nano()),
		ConditionalsHash: append([]byte{}, s.ConditionalsHash...),
	}, nil
}
