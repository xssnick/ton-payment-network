//go:build !(js && wasm)

package wallet

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"sync/atomic"
	"time"
)

type Wallet struct {
	apiClient wallet.TonAPI
	wallet    *wallet.Wallet

	firstTxDone bool
}

func InitWallet(apiClient wallet.TonAPI, key ed25519.PrivateKey) (*Wallet, error) {
	walletAbstractSeqno := uint32(0)
	w, err := wallet.FromPrivateKey(apiClient, key, wallet.ConfigHighloadV3{
		MessageTTL: 3*60 + 30,
		MessageBuilder: func(ctx context.Context, subWalletId uint32) (id uint32, createdAt int64, err error) {
			createdAt = time.Now().UTC().Unix() - 30 // something older than last master block, to pass through LS external's time validation
			// TODO: store seqno in db
			id = uint32((createdAt%(3*60+30))<<15) | atomic.AddUint32(&walletAbstractSeqno, 1)%(1<<15)
			return
		},
	})
	if err != nil {
		return nil, err
	}

	return &Wallet{
		wallet:    w,
		apiClient: apiClient,
	}, nil
}

func (w *Wallet) Wallet() *wallet.Wallet {
	return w.wallet
}

func (w *Wallet) WalletAddress() *address.Address {
	return w.wallet.WalletAddress()
}

func (w *Wallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.doTransactions(ctx, []*wallet.Message{wallet.SimpleMessage(to, amt, body)}, reason)
}

func (w *Wallet) DoTransactionEC(ctx context.Context, reason string, to *address.Address, tonAmt tlb.Coins, body *cell.Cell, ecID uint32, ecAmt tlb.Coins) ([]byte, error) {
	m := wallet.SimpleMessage(to, tonAmt, body)
	m.InternalMessage.ExtraCurrencies = cell.NewDict(32)
	_ = m.InternalMessage.ExtraCurrencies.SetIntKey(big.NewInt(int64(ecID)), cell.BeginCell().MustStoreBigVarUInt(ecAmt.Nano(), 32).EndCell())

	return w.doTransactions(ctx, []*wallet.Message{m}, reason)
}

func (w *Wallet) DoTransactionMany(ctx context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	var list []*wallet.Message
	for _, m := range messages {
		list = append(list, wallet.SimpleMessage(m.To, m.Amount, m.Body))
	}

	return w.doTransactions(ctx, list, reason)
}

func (w *Wallet) DeployContractWaitTransaction(ctx context.Context, amt tlb.Coins, body, code, data *cell.Cell) (*address.Address, []byte, error) {
	state := &tlb.StateInit{
		Data: data,
		Code: code,
	}

	stateCell, err := tlb.ToCell(state)
	if err != nil {
		return nil, nil, err
	}

	addr := address.NewAddress(0, 0, stateCell.Hash())

	msg, err := w.wallet.PrepareExternalMessageForMany(ctx, false, []*wallet.Message{
		{
			Mode: wallet.PayGasSeparately + wallet.IgnoreErrors,
			InternalMessage: &tlb.InternalMessage{
				IHRDisabled: true,
				Bounce:      false,
				DstAddr:     addr,
				Amount:      amt,
				Body:        body,
				StateInit:   state,
			},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare tx: %w", err)
	}

	_, _, _, err = w.apiClient.SendExternalMessageWaitTransaction(ctx, msg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send deploy tx: %w", err)
	}

	return addr, msg.NormalizedHash(), nil
}

func (w *Wallet) doTransactions(ctx context.Context, msgList []*wallet.Message, _ string) ([]byte, error) {
	msg, err := w.wallet.PrepareExternalMessageForMany(ctx, !w.firstTxDone, msgList)
	if err != nil {
		return nil, fmt.Errorf("failed to preapre tx: %w", err)
	}

	_, _, _, err = w.apiClient.SendExternalMessageWaitTransaction(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("failed to send tx: %w", err)
	}

	w.firstTxDone = true
	return msg.NormalizedHash(), nil
}
