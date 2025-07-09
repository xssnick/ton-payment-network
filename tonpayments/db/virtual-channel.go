package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

func (d *DB) CreateVirtualChannelMeta(ctx context.Context, meta *VirtualChannelMeta) error {
	key := []byte("vch:" + base64.StdEncoding.EncodeToString(meta.Key))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return ErrAlreadyExists
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) UpdateVirtualChannelMeta(ctx context.Context, meta *VirtualChannelMeta) error {
	key := []byte("vch:" + base64.StdEncoding.EncodeToString(meta.Key))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return ErrNotFound
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) GetVirtualChannelMeta(ctx context.Context, key []byte) (*VirtualChannelMeta, error) {
	tx := d.storage.GetExecutor(ctx)

	data, err := tx.Get([]byte("vch:" + base64.StdEncoding.EncodeToString(key)))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var vc *VirtualChannelMeta
	if err = json.Unmarshal(data, &vc); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}
	return vc, nil
}
