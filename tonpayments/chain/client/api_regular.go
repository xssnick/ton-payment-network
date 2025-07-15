//go:build !(js && wasm)

package client

import (
	"bytes"
	"context"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/jetton"
	"math/big"
)

type TON struct {
	api ton.APIClientWrapped
}

func NewTON(api ton.APIClientWrapped) *TON {
	return &TON{
		api: api,
	}
}

func (t *TON) GetAccount(ctx context.Context, addr *address.Address) (*Account, error) {
	block, err := t.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current masterchain info: %w", err)
	}

	acc, err := t.api.GetAccount(ctx, block, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	a := &Account{
		Address:    addr,
		IsActive:   true,
		Code:       acc.Code,
		Data:       acc.Data,
		LastTxLT:   acc.LastTxLT,
		LastTxHash: acc.LastTxHash,
	}

	if acc.State != nil {
		a.HasState = true
		a.Balance = acc.State.Balance
		a.ExtraCurrencies = acc.State.ExtraCurrencies
	}

	if !acc.IsActive || !acc.State.IsValid || acc.State.Status != tlb.AccountStatusActive {
		a.IsActive = false
	}

	return a, nil
}

func (t *TON) GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error) {
	jw, err := jetton.NewJettonMasterClient(t.api, root).GetJettonWallet(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton wallet: %w", err)
	}
	return jw.Address(), nil
}

func (t *TON) GetJettonBalance(ctx context.Context, root, addr *address.Address) (*big.Int, error) {
	jw, err := jetton.NewJettonMasterClient(t.api, root).GetJettonWallet(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton wallet: %w", err)
	}

	balance, err := jw.GetBalance(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get jetton balance: %w", err)
	}
	return balance, nil
}

func (t *TON) GetLastTransaction(ctx context.Context, addr *address.Address) (*Transaction, *Account, error) {
	acc, err := t.GetAccount(ctx, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get account: %w", err)
	}

	if acc.LastTxLT == 0 {
		return nil, acc, nil
	}

	txList, err := t.api.ListTransactions(ctx, addr, 1, acc.LastTxLT, acc.LastTxHash)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get transactions: %w", err)
	}

	if len(txList) == 0 {
		return nil, nil, fmt.Errorf("failed to get transactions: no transactions returned")
	}

	tx := txList[0]
	if !bytes.Equal(tx.Hash, acc.LastTxHash) || tx.LT != acc.LastTxLT {
		return nil, nil, fmt.Errorf("failed to get transactions: last tx mismatch")
	}

	res := &Transaction{
		Hash:       tx.Hash,
		PrevTxHash: tx.PrevTxHash,
		PrevTxLT:   tx.PrevTxLT,
		LT:         tx.LT,
		At:         int64(tx.Now),
	}

	if desc, ok := tx.Description.(tlb.TransactionDescriptionOrdinary); ok {
		if comp, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM); ok && comp.Success {
			res.Success = true
			if tx.IO.In.MsgType == tlb.MsgTypeInternal {
				res.InternalInBody = tx.IO.In.AsInternal().Body
			}
		}
	}

	return res, acc, nil
}

func (t *TON) GetTransactionsList(ctx context.Context, addr *address.Address, lt uint64, hash []byte) ([]Transaction, error) {
	txList, err := t.api.ListTransactions(ctx, addr, 5, lt, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get transactions: %w", err)
	}

	if len(txList) == 0 {
		return nil, nil
	}

	var list []Transaction
	for _, tx := range txList {
		res := Transaction{
			PrevTxHash: tx.PrevTxHash,
			Hash:       tx.Hash,
			PrevTxLT:   tx.PrevTxLT,
			LT:         tx.LT,
			At:         int64(tx.Now),
		}

		if desc, ok := tx.Description.(tlb.TransactionDescriptionOrdinary); ok {
			if comp, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM); ok && comp.Success {
				res.Success = true
				if tx.IO.In.MsgType == tlb.MsgTypeInternal {
					res.InternalInBody = tx.IO.In.AsInternal().Body
				}
			}
		}

		list = append(list, res)
	}

	return list, nil
}
