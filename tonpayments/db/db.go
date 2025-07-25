package db

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
)

type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Release()
	Error() error
}

type Executor interface {
	Put(key, value []byte) error
	Delete(key []byte) error
	Get(key []byte) (value []byte, err error)
	Has(key []byte) (ret bool, err error)
	NewIterator(p []byte, forward bool) Iterator
}

type Storage interface {
	Transaction(ctx context.Context, f func(ctx context.Context) error) error
	GetExecutor(ctx context.Context) Executor
	Backup() error
	Close()
}

type DB struct {
	pubKey  ed25519.PublicKey
	storage Storage

	onChannelStateChange   func(ctx context.Context, ch *Channel, statusChanged bool)
	onChannelHistoryUpdate func(ctx context.Context, ch *Channel, item ChannelHistoryItem)
}

func NewDB(storage Storage, pubKey ed25519.PublicKey) *DB {
	return &DB{
		pubKey:  pubKey,
		storage: storage,
	}
}

func (d *DB) Close() {
	d.storage.Close()
}

func (d *DB) Transaction(ctx context.Context, f func(ctx context.Context) error) error {
	return d.storage.Transaction(ctx, f)
}

// SetMigrationVersion sets the migration version in the DB.
func (d *DB) SetMigrationVersion(ctx context.Context, version int) error {
	exec := d.storage.GetExecutor(ctx)
	err := exec.Put([]byte("__migration_version"), []byte(fmt.Sprintf("%d", version)))
	if err != nil {
		return fmt.Errorf("failed to set migration version: %w", err)
	}
	return nil
}

// GetMigrationVersion retrieves the current migration version from the DB.
func (d *DB) GetMigrationVersion(ctx context.Context) (int, error) {
	exec := d.storage.GetExecutor(ctx)
	value, err := exec.Get([]byte("__migration_version"))
	if errors.Is(err, ErrNotFound) {
		return 0, nil // Default to version 0 if no version is stored
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get migration version: %w", err)
	}

	var version int
	if _, err := fmt.Sscanf(string(value), "%d", &version); err != nil {
		return 0, fmt.Errorf("failed to parse migration version: %w", err)
	}

	return version, nil
}
