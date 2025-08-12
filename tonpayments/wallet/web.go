//go:build js && wasm

package wallet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"syscall/js"
)

type Wallet struct {
}

func InitWallet() (*Wallet, error) {
	return &Wallet{}, nil
}

func (w *Wallet) WalletAddress() *address.Address {
	walletAddress := js.Global().Get("walletAddress")
	if walletAddress.Type() != js.TypeFunction {
		panic("walletAddress func is not registered globally")
	}
	res := walletAddress.Invoke().String()
	return address.MustParseAddr(res).Testnet(false).Bounce(false)
}

func (w *Wallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{
		{
			To:     to,
			Amount: amt,
			Body:   body,
		},
	})
}

func (w *Wallet) DoTransactionMany(ctx context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	if len(messages) > 4 {
		panic("attempt to execute more than 4 messages in web")
	}

	doTransaction := js.Global().Get("doTransaction")
	if doTransaction.Type() != js.TypeFunction {
		panic("doTransaction func is not registered globally")
	}

	txMessages := make([]interface{}, len(messages))
	for i, msg := range messages {
		if msg.EC != nil {
			panic("ec is not supported on web")
		}

		txMsg := map[string]interface{}{
			"to":      msg.To.String(),
			"amtNano": msg.Amount.Nano().String(),
			"body":    base64.StdEncoding.EncodeToString(msg.Body.ToBOC()),
		}

		if msg.StateInit != nil {
			stateCell, err := tlb.ToCell(msg.StateInit)
			if err != nil {
				return nil, err
			}
			txMsg["stateInit"] = base64.StdEncoding.EncodeToString(stateCell.ToBOC())
		}

		txMessages[i] = txMsg
	}

	promise := doTransaction.Invoke(
		js.ValueOf(reason),
		js.ValueOf(txMessages),
	)

	val, err := awaitPromise(promise)
	if err != nil {
		return nil, err
	}

	hash, err := parseResultMsg(val.String())
	if err != nil {
		return nil, err
	}

	return hash, nil
}

func awaitPromise(p js.Value) (js.Value, error) {
	ch := make(chan struct{})
	var res js.Value
	var err error

	var ok, fail js.Func

	ok = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) > 0 {
			res = args[0]
		}
		ok.Release()
		fail.Release()
		close(ch)
		return nil
	})

	fail = js.FuncOf(func(this js.Value, args []js.Value) any {
		var msg string
		if len(args) == 0 || args[0].IsUndefined() || args[0].IsNull() {
			msg = "promise rejected"
		} else if args[0].Type() == js.TypeString {
			msg = args[0].String()
		} else {
			msg = js.Global().Get("String").Invoke(args[0]).String()
		}
		err = errors.New(msg)
		ok.Release()
		fail.Release()
		close(ch)
		return nil
	})

	p.Call("then", ok, fail)
	<-ch
	return res, err
}

func parseResultMsg(val string) ([]byte, error) {
	boc, err := base64.StdEncoding.DecodeString(val)
	if err != nil {
		return nil, fmt.Errorf("failed to decode res boc: %w", err)
	}

	msgCell, err := cell.FromBOC(boc)
	if err != nil {
		return nil, fmt.Errorf("failed to parse res cell: %w", err)
	}

	var msg tlb.ExternalMessageIn
	if err = tlb.LoadFromCell(&msg, msgCell.BeginParse()); err != nil {
		return nil, fmt.Errorf("failed to parse res msg: %w", err)
	}

	return msg.NormalizedHash(), nil
}
