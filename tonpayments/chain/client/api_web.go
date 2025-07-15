//go:build js && wasm

package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"math/big"
	"syscall/js"
)

type TON struct {
}

func NewTON() *TON {
	return &TON{}
}

func (t *TON) GetAccount(ctx context.Context, addr *address.Address) (*Account, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/account?address=%s", addr.String())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var acc Account
	if err := json.Unmarshal(resp, &acc); err != nil {
		return nil, err
	}

	return &acc, nil
}

func (t *TON) GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/jetton/wallet?jetton=%s&address=%s", root.String(), addr.String())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct{ Address string }
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return address.ParseAddr(result.Address)
}

func (t *TON) GetJettonBalance(ctx context.Context, root, addr *address.Address) (*big.Int, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/jetton/balance?jetton=%s&address=%s", root.String(), addr.String())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct{ Balance string }
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	bal := new(big.Int)
	bal.SetString(result.Balance, 10)
	return bal, nil
}

func (t *TON) GetLastTransaction(ctx context.Context, addr *address.Address) (*Transaction, *Account, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/transaction/last?address=%s", addr.String())
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, nil, err
	}

	var result struct {
		Account     *Account
		Transaction *Transaction
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, nil, err
	}

	return result.Transaction, result.Account, nil
}

func fetchJSON(ctx context.Context, url string) ([]byte, error) {
	ch := make(chan struct {
		data []byte
		err  error
	}, 1)

	// JS fetch
	fetch := js.Global().Get("fetch")
	if fetch.IsUndefined() {
		return nil, fmt.Errorf("fetch not supported")
	}

	promise := fetch.Invoke(url)
	then := js.FuncOf(func(this js.Value, args []js.Value) any {
		resp := args[0]
		jsonPromise := resp.Call("json")
		thenJson := js.FuncOf(func(this js.Value, args []js.Value) any {
			jsObj := args[0]
			str := js.Global().Get("JSON").Call("stringify", jsObj).String()
			ch <- struct {
				data []byte
				err  error
			}{[]byte(str), nil}
			return nil
		})
		jsonPromise.Call("then", thenJson)
		return nil
	})

	catch := js.FuncOf(func(this js.Value, args []js.Value) any {
		errMsg := args[0].String()
		ch <- struct {
			data []byte
			err  error
		}{nil, fmt.Errorf(errMsg)}
		return nil
	})

	promise.Call("then", then).Call("catch", catch)

	select {
	case result := <-ch:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *TON) GetTransactionsList(ctx context.Context, addr *address.Address, lt uint64, hash []byte) ([]Transaction, error) {
	url := fmt.Sprintf("/web-channel/api/v1/ton/transaction/list?address=%s&lt=%d&hash=%s", addr.String(), lt, base64.URLEncoding.EncodeToString(hash))
	resp, err := fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Transactions []Transaction
	}

	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	return result.Transactions, nil
}
