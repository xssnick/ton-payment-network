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

// PaymentChannelCodeBoC https://github.com/xssnick/payment-channel-contract/blob/master/contracts/payment_channel.tolk
const PaymentChannelCodeBoC = "b5ee9c7241023a01000b67000114ff00f4a413f4bcf2c80b0102016202370202cb0329020120041e020120051803add76d176fd99f802e8698180b8d8492f81f07d2001e98f90c1082c9f1c49dd47991a10410839b1684e5d4b1801780bed98f01910c10868b9aa005d4b1880f80d6d98f010c10803b5fef8dd47421880ed9e71877186f80cc06091702ead401d001d401d021f90102d31f01821048baa61abaf2e068f84a01d37f59baf2e072f848544355f910f8494330f910b0f2e065fa00fa005321a0f842f843a0baf2e07e02f862f863d33fd33ff84b23b9f84c23b9b0f2e06c22f86b21f86cf84d6e926c21e30efa00fa00305cb1c200915be30df00607080036f84dd0d33ffa003171d721d33f305044bc5023bc12b1936df86dde02a420c2008ea1f84321a1f863f84721a0f867f843c2fff2e076f854f84a128210a32f0b3c70db3c9130e220c2008ea1f84221a1f862f84621a0f866f842c2fff2e076f853f84a128210a32f0b3c70db3c9130e22828036021821079ae99b5ba943101f0088fa0218210d2b1eeebba8e855bdb3cdb31e02182108175e15dba8e843101db3ce30ee20a0b0e00c0d401d001d401d021f90102d31f0182100802ada3baf2e068f84a01d37f59baf2e072f848544355f910f8494330f910b0f2e065fa00fa005321a0f842f843a0baf2e07e02f862f863d33fd33f30f84b22b9f84c22b9b0f2e06c01f86bf86cf00e01c6f84d6ef2e06ad2008308d71820f9012392f84892f849e24130f910f2e065d31f0182108c623692baf2e068f84a01d37f59baf2e072d401d08308d718d74c20f900f8484130f910f2e065d001d430d08308d718d74c20f900f8494130f910f2e065d0010c01fed31f01821043685374baf2e068f84a01d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e2303205d31f01821043685374baf2e068f84a01d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e23032f84b5280bef84c5250beb0524bbe1ab0527abe190d0060b05215be14b05248be17b0f2e06903c8cb3f58fa0213cbff13cb3f01fa02cbfff82301cb1fca007001ca00c9f86df006036c2182109a77c0dbba8e843101db3c8f2521821056c39b4cba8e843101db3c8e9432821025432a91ba8e8530db3cdb31e0840ff2f0e2e20f141601c6f84d6ef2d06bd2008308d71820f9012392f84892f849e24130f910f2e065d31f018210b8a21379baf2e068f84a01d37f59baf2e072d401d08308d718d74c20f900f8484130f910f2e065d001d430d08308d718d74c20f900f8494130f910f2e065d0011001fcd31f01821043685374baf2e068f84a01d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e2303205d31f01821043685374baf2e068f84a01d37f59baf2e072d33ffa00d3ff552003d200019ad401d0d33ffa00d3ff30937f5300e230325339be5381beb0f84c5250beb0f84b5290beb01102fe5237be16b05262beb0f2e06927c20097f84c18bef2e0699137e222c20097f84b13bef2e0699132e2f84dd0d33ffa00d3ffd33ffa00d3ffd31ff84f5220a0f823bcf2e06fd200d20030b3f2e07d209c3537373a5274bc5263bc12b18e11323939395250bc5299bc18b14650134440e25319bdf2e06d9130e30d04c8cb3f50031213001c94f85114a096f85117a00603e2030036fa0214cbff12cb3f5003fa02cbffcb1fca007f01ca00c9f86df00601f6f84d6ef2d06bd2008308d71820f9012392f84892f849e24130f910f2e065d31f01821014588aabbaf2e068f84a01d37f59baf2e072f404d430f84dd0d33ffa00d3ffd33ffa00d3ffd31ff84f5220a020f823b9f2e06ef850a0f823bcf2e070d200d2003054767b935b5334de0bd739f2e073d30701c003f2e073201500c8d70bff58baf2e073d74c8e260b8020f4966fa5208e168b08400f8020f42e6fa1f2e073ed1e12da111ca00b0c926c21e2b31ce63b0b956c2207d76899353507d76803506714e205c8cb3f5004fa0212cbffcb3f5003fa02cbffcb1fca00ca00c9f86df00600c0f84d6ef2d06bf84dd0d33ffa00d3ff31d33ffa00d3ff31d31f30f84fa0f850a0f823b9f2e071f84221a023a1f862f8435003a058a1f863f843c1009af842f843a0f86270f863def842c1009af843f842a0f86370f863de01a4f86ba4f86cf00e007e31f856c0018e200282097d7840bef2e07702f404308020f85759f40e6fa1f2e079fa043001f0078e1533218209c9c380bef2e077018209c9c380a158f007e2020120191b01bb5ed44d0d20001f861d401d0fa0001f864fa0001f865fa0001f866fa0001f867fa0001f862fa0030f863d3ff01f868d3ff01f869d37f01f86ad401f86ef84ed0d31f01f86ffa0001f871d31f30f870d31f01f86bd31f01f86cf40401f86d81a0082d401f872f852d0fa0001f875fa4001f873fa4001f874d200018e1fd200018e10d430d072f876fa4001f878fa4030f8799871f876d31f30f877e2943070f876e2300201201c1d0097323e10407280323e113e80be117e80be11be80be11fe80be10be80be10fe80b240733e120072fffe124072fffe128072dffe1380733e12c072c7fe130072c7fe13407d003e148073327b552000573e107cb81d7e135bbcb81ab4800c273e1108683e193e1080683e18a73e1148683e197e10c0683e18f8bc01a00201201f25020120202201f54f841f2d064d2008308d71820f9010392f84892f849e24330f910f2e065d31f018210481ebc44baf2e068f84a01d37f30baf2e072f842f843b1f844b1f845b1f846b1f847b1c000f2e0668208989680f856c0028e123082100bebc200f859d70b01c00092f01bde9df856c00197308210042c1d80dee2f85501bc8210020f8276f10f855beb0f2e07b7ff861f006020120232400393220040072c1540173c59400fe809c0072da84b2c7f2dff2407ec20c2000651b60083e15f21401fe8180d199bd10f220040072c1540173c5a0827270e03e80853d001c0072da44f2c7c4b2dff2407ec20c200201202627009d4c8801001cb05f859cf16821004c4b400fa027001cb697f01ca00c882100f8a7ea501cb1f7001cb3f5005fa0225cf165005cf166d01f400820a160ec0fa027f01ca00cb1fcb7fc901ccc901fb0830802754f854f843f84a8210dddc88ba72db3cf853f842f84a8210dddc88ba810082db3c70f86170f86270f86370f86470f86570f86670f8676df86df00682828003cf856c00092f00a8e14f856c00292f00c9bf856c00192f00b925f05e2e2e20201202a2c0101fc2b00ec8e72eda2edfbf856c002f2e0788040d721fa00fa40d2000193d430d0de03820a160ec0b9f85925c705b3b1956c1202f018e0543313ed41ed43ed44ed45ed47945b02f018ed67ed65ed64ed63ed61737fed118e1901d21f018210593e3893ba96f007f019db31e05f03840ff2f0ed41edf101f2ffdb030201482d320201202e300101202f006ec8801001cb0501cf1670fa027001cb6a82100f8a7ea501cb1f7001cb3f01fa0221cf1601cf166d01f40071fa027001ca00c98042fb08300101203100b46df855f856c00096f842f843a0a08e21f856c001f842f843a0c200b08e128020f857c8f842f843a0fa06034144f44301dee20170fb03c8801001cb0501cf1670fa027001cb6a8210d53276db01cb1f7001cb3fc9810082fb0830020120333501012034007a8040d721f859d70b01c000f2e07cf85858c705f2e065fa4030f879c8f855fa02f853cf16f854cf167301cb01c8f858cf16f859cf16c901ccc9f872f00601012036005ec8801001cb05f858cf168209c9c380fa027001cb6a82102c76b97301cb1f7001cb3ff828cf167001ca00c970fb08300201203839007fbc517f802fc20c8b870fc26b748b8f07c26e840206b90fd0018c020eb90fd0018b8eb90e98f987c27a908507c11de491839707c27d07c28507c11de48b9f03a40067bfd747802c1008517f6a1ec7c217c21fc227c22fc237c23b7837c247c24b7817c257c277c25fc2637817c26fc2afc29fc2a3781c4ef61183"

var PaymentChannelCode = func() *cell.Cell {
	codeBoC, _ := hex.DecodeString(PaymentChannelCodeBoC)
	code, _ := cell.FromBOC(codeBoC)
	return code
}()
var PaymentChannelCodeHash = PaymentChannelCode.Hash()

func init() {
	tlb.Register(CurrencyConfigJetton{})
	tlb.Register(CurrencyConfigEC{})
	tlb.Register(CurrencyConfigTon{})
}

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

type CurrencyConfigEC struct {
	_  tlb.Magic `tlb:"$10"`
	ID uint32    `tlb:"## 32"`
}

type CurrencyConfigTon struct {
	_ tlb.Magic `tlb:"$0"`
}

type CurrencyConfigJettonInfo struct {
	Master *address.Address `tlb:"addr"`
	Wallet *address.Address `tlb:"addr"`
}

type CurrencyConfigJetton struct {
	_    tlb.Magic                `tlb:"$11"`
	Info CurrencyConfigJettonInfo `tlb:"^"`
}

type PaymentConfig struct {
	StorageFee     tlb.Coins        `tlb:"."`
	DestA          *address.Address `tlb:"addr"`
	DestB          *address.Address `tlb:"addr"`
	CurrencyConfig any              `tlb:"[CurrencyConfigTon,CurrencyConfigJetton,CurrencyConfigEC]"`
}

type Balance struct {
	DepositA  tlb.Coins `tlb:"."`
	DepositB  tlb.Coins `tlb:"."`
	WithdrawA tlb.Coins `tlb:"."`
	WithdrawB tlb.Coins `tlb:"."`
	BalanceA  tlb.Coins `tlb:"."`
	BalanceB  tlb.Coins `tlb:"."`
}

type AsyncJettonChannelStorageData struct {
	Initialized     bool              `tlb:"bool"`
	Balance         Balance           `tlb:"^"`
	KeyA            []byte            `tlb:"bits 256"`
	KeyB            []byte            `tlb:"bits 256"`
	ChannelID       ChannelID         `tlb:"bits 128"`
	ClosingConfig   ClosingConfig     `tlb:"^"`
	CommittedSeqnoA uint32            `tlb:"## 32"`
	CommittedSeqnoB uint32            `tlb:"## 32"`
	Quarantine      *QuarantinedState `tlb:"maybe ^"`
	PaymentConfig   PaymentConfig     `tlb:"^"`
}

/// Messages

type InitChannel struct {
	_         tlb.Magic `tlb:"#79ae99b5"`
	IsA       bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_         tlb.Magic `tlb:"#481ebc44"`
		ChannelID ChannelID `tlb:"bits 128"`
	} `tlb:"."`
}

type TopupBalance struct {
	_   tlb.Magic `tlb:"#593e3893"`
	IsA bool      `tlb:"bool"`
}

type CooperativeClose struct {
	_          tlb.Magic `tlb:"#d2b1eeeb"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#0802ada3"`
		ChannelID ChannelID `tlb:"bits 128"`
		BalanceA  tlb.Coins `tlb:"."`
		BalanceB  tlb.Coins `tlb:"."`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type CooperativeCommit struct {
	_          tlb.Magic `tlb:"#076bfdf1"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#48baa61a"`
		ChannelID ChannelID `tlb:"bits 128"`
		BalanceA  tlb.Coins `tlb:"."`
		BalanceB  tlb.Coins `tlb:"."`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
		WithdrawA tlb.Coins `tlb:"."`
		WithdrawB tlb.Coins `tlb:"."`
	} `tlb:"."`
}

type StartUncooperativeClose struct {
	_           tlb.Magic `tlb:"#8175e15d"`
	IsSignedByA bool      `tlb:"bool"`
	Signature   Signature `tlb:"."`
	Signed      struct {
		_         tlb.Magic         `tlb:"#8c623692"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type ChallengeQuarantinedState struct {
	_               tlb.Magic `tlb:"#9a77c0db"`
	IsChallengedByA bool      `tlb:"bool"`
	Signature       Signature `tlb:"."`
	Signed          struct {
		_         tlb.Magic         `tlb:"#b8a21379"`
		ChannelID ChannelID         `tlb:"bits 128"`
		A         SignedSemiChannel `tlb:"^"`
		B         SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type SettleConditionals struct {
	_         tlb.Magic `tlb:"#56c39b4c"`
	IsFromA   bool      `tlb:"bool"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                    tlb.Magic        `tlb:"#14588aab"`
		ChannelID            ChannelID        `tlb:"bits 128"`
		ConditionalsToSettle *cell.Dictionary `tlb:"dict 32"`
		ConditionalsProof    *cell.Cell       `tlb:"^"`
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
