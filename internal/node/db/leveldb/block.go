package leveldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/xssnick/ton-payment-network/internal/node/db"
	"time"
)

func (d *DB) SetBlockOffset(ctx context.Context, seqno uint32) error {
	tx := d.getExecutor(ctx)

	data, err := json.Marshal(db.BlockOffset{
		Seqno:     seqno,
		UpdatedAt: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("failed to encode json: %w", err)
	}

	if err = tx.Put([]byte("bo:master"), data, &opt.WriteOptions{
		Sync: true,
	}); err != nil {
		return fmt.Errorf("failed to put: %w", err)
	}

	return nil
}

func (d *DB) GetBlockOffset(ctx context.Context) (*db.BlockOffset, error) {
	tx := d.getExecutor(ctx)

	data, err := tx.Get([]byte("bo:master"), nil)
	if err != nil {
		if errors.Is(err, leveldb.ErrNotFound) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var off *db.BlockOffset
	if err = json.Unmarshal(data, &off); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	return off, nil
}

