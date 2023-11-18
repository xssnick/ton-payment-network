package leveldb

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/xssnick/payment-network/internal/node/db"
	"time"
)

func (d *DB) AcquireTask(ctx context.Context) (*db.Task, error) {
	var result *db.Task
	err := d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		iter := tx.NewIterator(util.BytesPrefix([]byte("ti:")), nil)
		defer iter.Release()

		now := time.Now()

		var toSkip []string
	next:
		for iter.Next() {
			key := iter.Key()
			if binary.LittleEndian.Uint64(key[3:]) > uint64(now.UnixNano()) {
				// no tasks ready to execute
				break
			}

			q := string(key[3+8:])
			for _, skip := range toSkip {
				if q == skip {
					continue next
				}
			}

			dataKey := iter.Value()

			data, err := tx.Get(dataKey, nil)
			if err != nil {
				return fmt.Errorf("failed to get task by index: %w", err)
			}

			var task *db.Task
			if err := json.Unmarshal(data, &task); err != nil {
				return fmt.Errorf("failed to decode json data: %w", err)
			}

			// it should be removed from index when completed, but just to be sure
			if task.CompletedAt != nil {
				continue
			}

			if task.LockedTill != nil && task.LockedTill.After(now) {
				// locked by someone else (already in progress)
				// we skip everything in this queue to not break the order
				toSkip = append(toSkip, task.Queue)
				continue
			}

			result = task

			// we need to lock task to not acquire it twice when using multiple workers
			till := time.Now().Add(2 * time.Minute)
			result.LockedTill = &till

			data, err = json.Marshal(result)
			if err != nil {
				return fmt.Errorf("failed to encode json: %w", err)
			}

			if err = tx.Put(dataKey, data, &opt.WriteOptions{
				Sync: true,
			}); err != nil {
				return fmt.Errorf("failed to put index: %w", err)
			}

			break
		}

		if err := iter.Error(); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// TODO: complete, retry

func (d *DB) CreateTask(ctx context.Context, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error {
	bts, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return d.Transaction(ctx, func(ctx context.Context) error {
		after := time.Now()
		if executeAfter != nil {
			after = *executeAfter
		}

		if err = d.createTask(ctx, &db.Task{
			ID:           id,
			Type:         typ,
			Queue:        queue,
			Data:         bts,
			ExecuteAfter: after,
			ExecuteTill:  executeTill,
			CreatedAt:    time.Now(),
		}); err != nil {
			if errors.Is(err, db.ErrAlreadyExists) {
				// idempotency
				return nil
			}
			return fmt.Errorf("failed to create task: %w", err)
		}
		return nil
	})
}

func (d *DB) CompleteTask(ctx context.Context, task *db.Task) error {
	if task.CompletedAt != nil {
		return nil
	}

	now := time.Now()
	task.CompletedAt = &now

	key := append([]byte("tv:"), []byte(task.ID)...)

	at := make([]byte, 8)
	binary.LittleEndian.PutUint64(at, uint64(task.ExecuteAfter.UTC().UnixNano()))

	// we need remove it from index to not pick it up again
	keyOrderIndex := append(append([]byte("ti:"), at...), []byte(task.Queue)...)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return db.ErrNotFound
		}

		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		if err = tx.Delete(keyOrderIndex, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put index: %w", err)
		}
		return nil
	})
}

func (d *DB) RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error {
	if task.CompletedAt != nil {
		return nil
	}

	task.LastError = reason
	task.ExecuteAfter = retryAt

	key := append([]byte("tv:"), []byte(task.ID)...)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return db.ErrNotFound
		}

		data, err := json.Marshal(task)
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

func (d *DB) createTask(ctx context.Context, task *db.Task) error {
	key := append([]byte("tv:"), []byte(task.ID)...)

	at := make([]byte, 8)
	binary.LittleEndian.PutUint64(at, uint64(task.ExecuteAfter.UTC().UnixNano()))

	// we need an index to know processing order
	keyOrderIndex := append(append([]byte("ti:"), at...), []byte(task.Queue)...)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return db.ErrAlreadyExists
		}

		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put data: %w", err)
		}
		if err = tx.Put(keyOrderIndex, key, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put index: %w", err)
		}
		return nil
	})
}
