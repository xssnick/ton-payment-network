package browser

import (
	"crypto/ed25519"
)

type DB struct {
	pubKey ed25519.PublicKey
}

type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Release()
	Error() error
}

type Snapshot interface {
	Get(k []byte) ([]byte, error)
	Has(k []byte) (bool, error)
	NewIterator(p []byte, forward bool) Iterator
}
