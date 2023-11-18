package leveldb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/xssnick/payment-network/internal/node/db"
)

func (d *DB) CreateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error {
	key := []byte("vch:" + hex.EncodeToString(meta.Key))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return db.ErrAlreadyExists
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) UpdateVirtualChannelMeta(ctx context.Context, meta *db.VirtualChannelMeta) error {
	key := []byte("vch:" + hex.EncodeToString(meta.Key))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return db.ErrNotFound
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) GetVirtualChannelMeta(ctx context.Context, key []byte) (*db.VirtualChannelMeta, error) {
	tx := d.getExecutor(ctx)

	data, err := tx.Get([]byte("vch:"+hex.EncodeToString(key)), nil)
	if err != nil {
		if errors.Is(err, leveldb.ErrNotFound) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var vc *db.VirtualChannelMeta
	if err = json.Unmarshal(data, &vc); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}
	return vc, nil
}
