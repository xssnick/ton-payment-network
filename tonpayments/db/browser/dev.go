//go:build !(js && wasm)

package browser

import (
	"context"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
)

func setLocalStorage(key string, value []byte) {
}

func getLocalStorage(key string) ([]byte, error) {
	panic("dev")
}

func hasLocalStorage(key string) bool {
	panic("dev")
}

func removeLocalStorage(key string) {
	panic("dev")
}

type IndexedDB struct{}

func (i *IndexedDB) Transaction(ctx context.Context, f func(ctx context.Context) error) error {
	panic("dev")
}

func (i *IndexedDB) GetExecutor(ctx context.Context) db.Executor {
	panic("dev")
}

func (i *IndexedDB) Backup() error {
	panic("dev")
}

func (i *IndexedDB) Close() {
	panic("dev")
}

func NewIndexedDB(path string) (*IndexedDB, error) {
	return nil, nil
}
