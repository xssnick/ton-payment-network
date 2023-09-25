package db

import (
	"encoding/json"
	"time"
)

type Task struct {
	ID           string
	Type         string
	IsExecuted   bool
	Data         json.RawMessage
	ExecuteAfter *time.Time
	ExecuteTill  *time.Time
	CreatedAt    time.Time
	LastError    string
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
}

type CloseNextTask struct {
	VirtualKey []byte
	State      []byte
}
