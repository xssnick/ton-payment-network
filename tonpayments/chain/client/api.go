package client

import (
	"encoding/json"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type Account struct {
	Address *address.Address
	Balance tlb.Coins

	ExtraCurrencies *cell.Dictionary

	HasState bool
	IsActive bool
	Code     *cell.Cell
	Data     *cell.Cell

	LastTxLT   uint64
	LastTxHash []byte
}

type accountRaw struct {
	Address         string
	Balance         string
	ExtraCurrencies []byte
	HasState        bool
	IsActive        bool
	Code            []byte
	Data            []byte
	LastTxLT        uint64
	LastTxHash      []byte
}

func (a *Account) UnmarshalJSON(bytes []byte) error {
	var temp accountRaw
	var err error
	if err = json.Unmarshal(bytes, &temp); err != nil {
		return err
	}

	if temp.Address != "" {
		a.Address, err = address.ParseAddr(temp.Address)
		if err != nil {
			return err
		}
	}

	if temp.Balance != "" {
		a.Balance, err = tlb.FromTON(temp.Balance)
		if err != nil {
			return err
		}
	}

	if len(temp.ExtraCurrencies) != 0 {
		dict, err := cell.FromBOC(temp.ExtraCurrencies)
		if err != nil {
			return err
		}
		a.ExtraCurrencies = dict.AsDict(32)
	}

	a.HasState = temp.HasState
	a.IsActive = temp.IsActive

	if len(temp.Code) != 0 {
		a.Code, err = cell.FromBOC(temp.Code)
		if err != nil {
			return err
		}
	}

	if len(temp.Data) != 0 {
		a.Data, err = cell.FromBOC(temp.Data)
		if err != nil {
			return err
		}
	}

	a.LastTxLT = temp.LastTxLT

	if len(temp.LastTxHash) == 32 {
		a.LastTxHash = temp.LastTxHash
	}

	return nil
}

func (a *Account) MarshalJSON() ([]byte, error) {
	return json.Marshal(&accountRaw{
		Address: func() string {
			if a.Address != nil {
				return a.Address.String()
			}
			return ""
		}(),
		Balance: a.Balance.String(),
		ExtraCurrencies: func() []byte {
			if a.ExtraCurrencies != nil && !a.ExtraCurrencies.IsEmpty() {
				return a.ExtraCurrencies.AsCell().ToBOC()
			}
			return nil
		}(),
		HasState: a.HasState,
		IsActive: a.IsActive,
		Code: func() []byte {
			if a.Code != nil {
				return a.Code.ToBOC()
			}
			return nil
		}(),
		Data: func() []byte {
			if a.Data != nil {
				return a.Data.ToBOC()
			}
			return nil
		}(),
		LastTxLT:   a.LastTxLT,
		LastTxHash: a.LastTxHash,
	})
}

type Transaction struct {
	Hash           []byte
	PrevTxHash     []byte
	LT             uint64
	PrevTxLT       uint64
	At             int64
	Success        bool
	InternalInBody *cell.Cell
}

type transactionRaw struct {
	Hash           []byte
	PrevTxHash     []byte
	LT             uint64
	PrevTxLT       uint64
	At             int64
	Success        bool
	InternalInBody []byte
}

func (t *Transaction) UnmarshalJSON(bytes []byte) error {
	var temp transactionRaw
	var err error
	if err = json.Unmarshal(bytes, &temp); err != nil {
		return err
	}

	t.Hash = temp.Hash
	t.PrevTxHash = temp.PrevTxHash
	t.LT = temp.LT
	t.PrevTxLT = temp.PrevTxLT
	t.At = temp.At
	t.Success = temp.Success

	if len(temp.InternalInBody) != 0 {
		t.InternalInBody, err = cell.FromBOC(temp.InternalInBody)
		if err != nil {
			return err
		}
	}

	return nil
}

func (t *Transaction) MarshalJSON() ([]byte, error) {
	return json.Marshal(&transactionRaw{
		Hash:       t.Hash,
		PrevTxHash: t.PrevTxHash,
		LT:         t.LT,
		PrevTxLT:   t.PrevTxLT,
		At:         t.At,
		Success:    t.Success,
		InternalInBody: func() []byte {
			if t.InternalInBody != nil {
				return t.InternalInBody.ToBOC()
			}
			return nil
		}(),
	})
}
