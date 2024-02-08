package db

import (
	"encoding/json"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"time"
)

type Task struct {
	ID             string
	Type           string
	Queue          string
	Data           json.RawMessage
	LockedTill     *time.Time
	ExecuteAfter   time.Time
	ReExecuteAfter *time.Time
	ExecuteTill    *time.Time
	CreatedAt      time.Time
	CompletedAt    *time.Time
	LastError      string
}

type ChannelTask struct {
	Address string
}

type BlockOffset struct {
	Seqno     uint32
	UpdatedAt time.Time
}

type ChannelUncooperativeCloseTask struct {
	Address                 string
	CheckVirtualStillExists []byte
	ChannelInitiatedAt      *time.Time
}

type ChannelCooperativeCloseTask struct {
	Address            string
	ChannelInitiatedAt time.Time
}

type ConfirmCloseVirtualTask struct {
	VirtualKey []byte
}

type CloseNextVirtualTask struct {
	VirtualKey []byte
	State      []byte
	IsTransfer bool
}

type OpenVirtualTask struct {
	PrevChannelAddress string
	ChannelAddress     string
	VirtualKey         []byte
	Deadline           int64
	Fee                string
	Capacity           string
	Action             transport.OpenVirtualAction
}

type AskRemoveVirtualTask struct {
	Key            []byte
	ChannelAddress string
}

type IncrementStatesTask struct {
	ChannelAddress string
	WantResponse   bool
}

type RemoveVirtualTask struct {
	Key []byte
}
