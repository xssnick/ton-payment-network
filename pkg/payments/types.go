package payments

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

// AsyncPaymentChannelCodeBoC Modified version of https://github.com/ton-blockchain/payment-channels/tree/master#compiled-code
const AsyncPaymentChannelCodeBoC = "b5ee9c7241023201000800000114ff00f4a413f4bcf2c80b010201200302000af26c21f01402014807040201200605008dbd0caba78037c20c8b870fc253748b8f07c256840206b90fd0018c020eb90fd0018b8eb90e98f987c23b7882908507c11de491839707c23b788507c23b789507c11de48b9f03a40075bc7fe3a78037c25e87d007d207d20184100d0caf6a1ec7c217c21b7817c227c22b7817c237c23fc247c24b7817c2524c3b7818823881b22a0219840202cb1d080201480c0902f5d76d176fde98f90c10833e3e940dd4998f8047010c108070310615d4a98f804ed98f0191041082abbac3f5d4a9878066d98f01041083cd09377dd4a987806ed98f01041080f8a8d67dd4a9878086d98f0104108044755195d4a987808ed98f0104108337b7834dd4a9878096d98f018c10812a19548dd71814207c0b0a0004f2f00008f013db31020120140d0201200f0e00d51d3c01be129bacfcb81afe12b434cffe803d010c74cffe803d010c74c7cc3e11dbc4283e11dbc4a83e08ee7cb81c7e003e10886808e87e18be10d400e816287e18fe10f04026be10be10e83e189c3e18f7be10b04026be10fe10a83e18dc3e18f780693e1a293e1a7c02e004f71cfc01be129bacfcb81af48020c235c6083e4048e4be1124be1178904c3e443cb81974c7c0608410db10dbaebcb81a3e118074dfd66ebcb81cbd010c3e12b434cffe803d0134cffe803d0134c7fe11dbc4148828083e08ee7cb81bbe11dbc4a83e08ef3cb81c348034800c151d9ea4d6d4cd37a2b98ec2f8c3c1b22013121110003acb3f5005fa0213f400cb3f5004fa0214f400cb1fca00ca00c9f86af007000c3636478045400004333300520b8020f4966fa5208e193050038020f4666fa1209901ed1e14da111da00c923430e202926c21e2b31c0201201a1501c31cfc01be129bacfcb81af48020c235c6083e4048e4be1124be1178904c3e443cb81974c7c0608410da19d46ebcb81a3e118074dfd66ebcb81cb5007420c235c6083e407e11104c3e443cb81940750c3420c235c6083e407e11504c3e443cb81940601601fcd31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00f404552003d200019ad401d0d33ffa00f40430937f206de2303205d31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00f404552003d200019ad401d0d33ffa00f40430937f206de230325339be5381beb0f8495250beb0f8485290beb01702fe5237be16b05262beb0f2e06927c20097f84918bef2e0699137e222c20097f84813bef2e0699132e2f84ad0d33ffa00f404d33ffa00f404d31ff8476f105220a0f823bcf2e06fd200d20030b3f2e073209c3537373a5274bc5263bc12b18e11323939395250bc5299bc18b14650134440e25319bab3f2e06d9130e30d7f05c81918003ecb3f5004fa0215f40012cb3f5004fa0213f400cb1f12ca00ca00c9f86af007002496f8476f1114a098f8476f1117a00603e20301c11cfc01be129bbcb81ab48020c235c6083e4048e4be1124be1178904c3e443cb81974c7c06084155b90db2ebcb81a3e118074dfd66ebcb81cb5007420c235c6083e407e11104c3e443cb81940750c3420c235c6083e407e11504c3e443cb81940601b01fed31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00f404552003d200019ad401d0d33ffa00f40430937f206de2303205d31f01821043685374baf2e068f84601d37f59baf2e072d33ffa00f404552003d200019ad401d0d33ffa00f40430937f206de23032f8485280bef8495250beb0524bbe1ab0527abe191c0068b05215be14b05248be17b0f2e06970f82305c8cb3f5004fa0215f40015cb3f5004fa0212f400cb1f12ca00ca00c9f86af800f007020120291e020120221f020148212000e51cfc01b5007400750074087e4040b4c7c0608410d0db5d2ebcb81a3e118074dfd66ebcb81cbe111510d57e443e1150cc3e442c3cb81974cff4cfcc3e1208ae7e1248ae6c3cb81b087e1a083e1a7e129ba456e3867e12b434cffe800c5c75c874cfcc140cef00af2c64db7e1ab7b8be003c01e000b51d3c01b5007400750074087e4040b4c7c0608410db1bdceebcb81a3e118074dfd66ebcb81cbe111510d57e443e1150cc3e442c3cb8197e80007e18be80007e18f4cff4cfcc3e1208ae7e1248ae6c3cb81b007e1a3e1a7e003c02e002012026230201202524006f3e12f43e800c7e903e900c3e09dbc41cbe10d62f24cc20c1b7be10fe11963c02be10be11a04020bc029c3e185c3e189c3e18db7e1abc01e0004120843777222e9c20043232c15401b3c594013e808532da84b2c7f2dff2407ec020020120282700cf1d3c01be106cfcb819348020c235c6083e4040e4be1124be117890cc3e443cb81974c7c060841a5b9a5d2ebcb81a3e118074dfd66ebcb81cbe803e800c3e1094882fbe10d4882fac3cb819807e18be18fe12f43e800c3e10be10e80068006e7cb8199ffe187c01e0004d1c3c01be106cfcb8193e803e800c3e1096283e18be10c0683e18fe10be10e8006efcb819bc01e00201202f2a0201202e2b0201202d2c008d3e13723e11be117e113e10540132803e10be80be10fe8084f2ffc4b2fff2dffc01487080a7fe12be127e121400f2c7c4b2c7fd0037807080e53e12c073253e1333c5b8b27b5520008b083e1b7b51343480007e187e80007e18be80007e18f4ffc07e1934ffc07e1974dfc07e19bc00487080a7f4c7c07e1a34c7c07e1a7d01007e1ab7807080e535007e1af7be1b20002d5f8476f12f8476f10c8cb1ff8476f11fa02cb1fc901cc80201483130001d35007434c7fe8034c7cc1bc0fe19e000091b087c0520cec0d56c"

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
	Seqno        uint64           `tlb:"## 64"`
	Sent         tlb.Coins        `tlb:"."`
	Conditionals *cell.Dictionary `tlb:"dict 32"`
}

type SemiChannel struct {
	_                tlb.Magic        `tlb:"#43685374"`
	ChannelID        ChannelID        `tlb:"bits 128"`
	Data             SemiChannelBody  `tlb:"."`
	CounterpartyData *SemiChannelBody `tlb:"maybe ^"`
}

type SignedSemiChannel struct {
	Signature Signature   `tlb:"."`
	State     SemiChannel `tlb:"."`
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

type AsyncChannelStorageData struct {
	Initialized     bool              `tlb:"bool"`
	BalanceA        tlb.Coins         `tlb:"."`
	BalanceB        tlb.Coins         `tlb:"."`
	KeyA            []byte            `tlb:"bits 256"`
	KeyB            []byte            `tlb:"bits 256"`
	ChannelID       ChannelID         `tlb:"bits 128"`
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
		_                    tlb.Magic         `tlb:"#436c436e"`
		ChannelID            ChannelID         `tlb:"bits 128"`
		ConditionalsToSettle *cell.Dictionary  `tlb:"dict 32"`
		B                    SignedSemiChannel `tlb:"^"`
	} `tlb:"."`
}

type FinishUncooperativeClose struct {
	_ tlb.Magic `tlb:"#25432a91"`
}

func (s *SignedSemiChannel) Verify(key ed25519.PublicKey) error {
	if bytes.Equal(s.Signature.Value, make([]byte, 64)) &&
		s.State.Data.Sent.Nano().Sign() == 0 &&
		s.State.Data.Conditionals.Size() == 0 {
		// TODO: use more reliable approach
		// empty
		return nil
	}

	c, err := tlb.ToCell(s.State)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, c.Hash(), s.Signature.Value) {
		log.Warn().Hex("sig", s.Signature.Value).Msg("invalid signature")
		return fmt.Errorf("invalid signature")
	}
	return nil
}

var ErrNotFound = fmt.Errorf("not found")

func (s *SemiChannel) FindVirtualChannel(key ed25519.PublicKey) (*big.Int, *VirtualChannel, error) {
	for _, kv := range s.Data.Conditionals.All() {
		vch, err := ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to patse state of one of virtual channels")
		}

		if !bytes.Equal(vch.Key, key) {
			continue
		}

		return kv.Key.BeginParse().MustLoadBigUInt(32), vch, nil
	}
	return nil, nil, ErrNotFound
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

	if !bytes.Equal(ourStateOnTheirSide.Hash(), ourState.Hash()) {
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

	if !bytes.Equal(theirStateOnOurSide.Hash(), theirState.Hash()) {
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
		cpData = fmt.Sprintf("(data_hash: %s seqno: %d; sent: %s; conditionals: %d)",
			hex.EncodeToString(cp.Hash()[:8]),
			s.CounterpartyData.Seqno, s.CounterpartyData.Sent.String(), s.CounterpartyData.Conditionals.Size())
	}

	return fmt.Sprintf("data_hash: %s seqno: %d; sent: %s; conditionals: %d; counterparty: %s",
		hex.EncodeToString(c.Hash()[:8]),
		s.Data.Seqno, s.Data.Sent.String(), s.Data.Conditionals.Size(), cpData)
}

func (s *SemiChannelBody) Copy() (SemiChannelBody, error) {
	conditions := cell.NewDict(32)
	if s.Conditionals.Size() > 0 {
		cl, err := s.Conditionals.ToCell()
		if err != nil {
			return SemiChannelBody{}, err
		}

		// TODO: more efficient copy
		conditions, err = cl.BeginParse().ToDict(32)
		if err != nil {
			return SemiChannelBody{}, err
		}
	}

	return SemiChannelBody{
		Seqno:        s.Seqno,
		Sent:         tlb.FromNanoTON(s.Sent.Nano()),
		Conditionals: conditions,
	}, nil
}
