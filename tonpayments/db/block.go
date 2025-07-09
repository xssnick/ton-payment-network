package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (d *DB) SetBlockOffset(ctx context.Context, seqno uint32) error {
	tx := d.storage.GetExecutor(ctx)

	data, err := json.Marshal(BlockOffset{
		Seqno:     seqno,
		UpdatedAt: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("failed to encode json: %w", err)
	}

	if err = tx.Put([]byte("bo:master"), data); err != nil {
		return fmt.Errorf("failed to put: %w", err)
	}

	return nil
}

func (d *DB) GetBlockOffset(ctx context.Context) (*BlockOffset, error) {
	tx := d.storage.GetExecutor(ctx)

	data, err := tx.Get([]byte("bo:master"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var off *BlockOffset
	if err = json.Unmarshal(data, &off); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	return off, nil
}
