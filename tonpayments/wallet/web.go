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
	doTransaction := js.Global().Get("doTransaction")
	if doTransaction.Type() != js.TypeFunction {
		panic("doTransaction func is not registered globally")
	}

	promise := doTransaction.Invoke(
		js.ValueOf(reason),
		js.ValueOf(to.String()),
		js.ValueOf(amt.Nano().String()),
		js.ValueOf(base64.StdEncoding.EncodeToString(body.ToBOC())),
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

func (w *Wallet) DeployContractWaitTransaction(ctx context.Context, amt tlb.Coins, body, code, data *cell.Cell) (*address.Address, []byte, error) {
	state := &tlb.StateInit{
		Data: data,
		Code: code,
	}

	stateCell, err := tlb.ToCell(state)
	if err != nil {
		return nil, nil, err
	}

	to := address.NewAddress(0, 0, stateCell.Hash())

	doTransaction := js.Global().Get("doTransaction")
	if doTransaction.Type() != js.TypeFunction {
		panic("doTransaction func is not registered globally")
	}

	promise := doTransaction.Invoke(
		js.ValueOf("Deploy channel contract"),
		js.ValueOf(to.String()),
		js.ValueOf(amt.Nano().String()),
		js.ValueOf(base64.StdEncoding.EncodeToString(body.ToBOC())),
		js.ValueOf(base64.StdEncoding.EncodeToString(stateCell.ToBOC())),
	)

	val, err := awaitPromise(promise)
	if err != nil {
		return nil, nil, err
	}

	hash, err := parseResultMsg(val.String())
	if err != nil {
		return nil, nil, err
	}

	return to, hash, nil
}

func (w *Wallet) DoTransactionMany(ctx context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	if len(messages) > 1 {
		panic("attempt to execute multiple messages in web")
	}
	return w.DoTransaction(ctx, reason, messages[0].To, messages[0].Amount, messages[0].Body)
}

func (w *Wallet) DoTransactionEC(ctx context.Context, reason string, to *address.Address, tonAmt tlb.Coins, body *cell.Cell, ecID uint32, ecAmt tlb.Coins) ([]byte, error) {
	return nil, fmt.Errorf("ec is not supported on web")
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
