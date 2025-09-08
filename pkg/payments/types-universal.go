package payments

import (
	"crypto/ed25519"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type AddrWrapped struct {
	Value *address.Address `tlb:"addr"`
}
type AsyncUniversalChannelStorageData struct {
	IsA             bool   `tlb:"bool"`
	Initialized     bool   `tlb:"bool"`
	CommittedSeqnoA uint64 `tlb:"## 64"`
	CommittedSeqnoB uint64 `tlb:"## 64"`
	WalletSeqno     uint32 `tlb:"## 32"`

	KeyA          ed25519.PublicKey      `tlb:"bits 256"`
	KeyB          ed25519.PublicKey      `tlb:"bits 256"`
	ChannelID     ChannelID              `tlb:"bits 128"`
	ClosingConfig UniversalClosingConfig `tlb:"^"`
	PartyAddress  *AddrWrapped           `tlb:"maybe ^"`

	Quarantine *UniversalQuarantinedState `tlb:"maybe ^"`
}

type UniversalQuarantinedState struct {
	TheirState                *SemiChannelUniversalBody `tlb:"maybe ."`
	QuarantineStarts          uint32                    `tlb:"## 32"`
	CommittedByOwner          bool                      `tlb:"bool"`
	OurSettlementFinalized    bool                      `tlb:"bool"`
	TheirActionsToExecuteHash []byte                    `tlb:"bits 256"`
}

type UniversalClosingConfig struct {
	QuarantineDuration             uint32    `tlb:"## 32"`
	ConditionalCloseDuration       uint32    `tlb:"## 32"`
	ActionsDuration                uint32    `tlb:"## 32"`
	ReplicationMessageAttachAmount tlb.Coins `tlb:"."`
}

type SemiChannelUniversalBody struct {
	Seqno            uint64 `tlb:"## 64"`
	ConditionalsHash []byte `tlb:"bits 256"`
	ActionsHash      []byte `tlb:"bits 256"`
}

type SemiChannelUniversal struct {
	Data             SemiChannelUniversalBody `tlb:"."`
	CounterpartyData SemiChannelUniversalBody `tlb:"^"`
}

type SemiChannelPacked struct {
	_         tlb.Magic            `tlb:"#98b70b88"`
	ChannelID ChannelID            `tlb:"bits 128"`
	State     SemiChannelUniversal `tlb:"."`
}

type SemiChannelUniversalSigned struct {
	Signature Signature         `tlb:"."`
	Channel   SemiChannelPacked `tlb:"^"`
}

type InitChannelUniversal struct {
	_          tlb.Magic `tlb:"#a19eda8c"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#2aa93806"`
		ChannelID ChannelID `tlb:"bits 128"`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type ExternalMsgDoubleSigned struct {
	_          tlb.Magic `tlb:"#a887b8fb"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_           tlb.Magic  `tlb:"#22d61a69"`
		ChannelID   ChannelID  `tlb:"bits 128"`
		SideA       bool       `tlb:"bool"`
		ValidUntil  uint32     `tlb:"## 32"`
		WalletSeqno uint32     `tlb:"## 32"`
		OutActions  *cell.Cell `tlb:"maybe ^"`
	} `tlb:"."`
}

type ExternalMsgOwnerSigned struct {
	_         tlb.Magic `tlb:"#31a4168c"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_           tlb.Magic  `tlb:"#22d61a69"`
		ChannelID   ChannelID  `tlb:"bits 128"`
		SideA       bool       `tlb:"bool"`
		ValidUntil  uint32     `tlb:"## 32"`
		WalletSeqno uint32     `tlb:"## 32"`
		OutActions  *cell.Cell `tlb:"maybe ^"`
	} `tlb:"."`
}

type CooperativeCommitUniversal struct {
	_          tlb.Magic `tlb:"#e02310f7"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#c2c8e579"`
		ChannelID ChannelID `tlb:"bits 128"`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type CooperativeCloseUniversal struct {
	_          tlb.Magic `tlb:"#66b93f47"`
	SignatureA Signature `tlb:"^"`
	SignatureB Signature `tlb:"^"`
	Signed     struct {
		_         tlb.Magic `tlb:"#3e72726c"`
		ChannelID ChannelID `tlb:"bits 128"`
		SeqnoA    uint64    `tlb:"## 64"`
		SeqnoB    uint64    `tlb:"## 64"`
	} `tlb:"."`
}

type UncoopCloseMsgUniversal struct {
	_         tlb.Magic `tlb:"#bd1cdbdb"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_         tlb.Magic           `tlb:"#a5af1d07"`
		ChannelID ChannelID           `tlb:"bits 128"`
		States    *UncoopSignedStates `tlb:"maybe ."`
	} `tlb:"."`
}

type UncoopSignedStates struct {
	A SemiChannelUniversalSigned `tlb:"^"`
	B SemiChannelUniversalSigned `tlb:"^"`
}

type SettleMsgUniversal struct {
	_         tlb.Magic `tlb:"#255e10b6"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                 tlb.Magic        `tlb:"#8a3056b7"`
		ChannelID         ChannelID        `tlb:"bits 128"`
		WalletSeqno       uint32           `tlb:"## 32"`
		ToSettle          *cell.Dictionary `tlb:"dict 32"`
		ConditionalsProof *cell.Cell       `tlb:"^"`
		ActionsInputProof *cell.Cell       `tlb:"^"`
	} `tlb:"."`
}

type FinalizeSettleMsgUniversal struct {
	_         tlb.Magic `tlb:"#b916bc7c"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                tlb.Magic `tlb:"#1c1b99b8"`
		ChannelID        ChannelID `tlb:"bits 128"`
		WalletSeqno      uint32    `tlb:"## 32"`
		ActionsInputHash []byte    `tlb:"bits 256"`
	} `tlb:"."`
}

type ExecuteActionsMsgUniversal struct {
	_         tlb.Magic `tlb:"#3fb3e346"`
	Signature Signature `tlb:"."`
	Signed    struct {
		_                      tlb.Magic  `tlb:"#048b33af"`
		ChannelID              ChannelID  `tlb:"bits 128"`
		Action                 *cell.Cell `tlb:"^"`
		OurActionsInputProof   *cell.Cell `tlb:"^"`
		TheirActionsInputProof *cell.Cell `tlb:"^"`
	} `tlb:"."`
}
