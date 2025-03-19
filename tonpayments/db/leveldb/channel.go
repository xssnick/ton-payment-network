package leveldb

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"time"
)

func (d *DB) SetOnChannelUpdated(f func(ch *db.Channel)) {
	d.onChannelStateChange = f
}

func (d *DB) CreateChannel(ctx context.Context, channel *db.Channel) error {
	key := []byte("ch:" + channel.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		has, err := tx.Has(key, nil)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return db.ErrAlreadyExists
		}

		channel.DBVersion = time.Now().UnixNano()
		data, err := json.Marshal(channel)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil {
			d.onChannelStateChange(channel)
		}
		return nil
	})
}

func (d *DB) UpdateChannel(ctx context.Context, channel *db.Channel) error {
	key := []byte("ch:" + channel.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.getExecutor(ctx)

		curChannel, err := d.GetChannel(ctx, channel.Address)
		if err != nil {
			return fmt.Errorf("failed to get channel: %w", err)
		}

		if curChannel.DBVersion != channel.DBVersion {
			return fmt.Errorf("version mismatch retry changes (current %d, update %d)", curChannel.DBVersion, channel.DBVersion)
		}

		channel.DBVersion = time.Now().UnixNano()
		data, err := json.Marshal(channel)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data, &opt.WriteOptions{
			Sync: true,
		}); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil && curChannel.Status != channel.Status {
			d.onChannelStateChange(channel)
		}

		return nil
	})
}

func (d *DB) GetChannel(ctx context.Context, addr string) (*db.Channel, error) {
	tx := d.getExecutor(ctx)

	key := []byte("ch:" + addr)

	data, err := tx.Get(key, nil)
	if err != nil {
		if errors.Is(err, leveldb.ErrNotFound) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var channel *db.Channel
	if err = json.Unmarshal(data, &channel); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	if err = channel.Our.Verify(d.pubKey); err != nil {
		log.Warn().Msg(channel.Our.State.Dump())
		return nil, fmt.Errorf("looks like our db state was tampered: %w", err)
	}
	if err = channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
		log.Warn().Msg(channel.Their.State.Dump())
		return nil, fmt.Errorf("looks like their db state was tampered: %w", err)
	}

	return channel, nil
}

func (d *DB) GetChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error) {
	tx := d.getExecutor(ctx)

	iter := tx.NewIterator(util.BytesPrefix([]byte("ch:")), nil)
	defer iter.Release()

	// TODO: optimize, use indexing
	var channels []*db.Channel
	for iter.Next() {
		var channel *db.Channel
		if err := json.Unmarshal(iter.Value(), &channel); err != nil {
			return nil, fmt.Errorf("failed to decode json data: %w", err)
		}

		if (status == db.ChannelStateAny || channel.Status == status) && (key == nil || bytes.Equal(channel.TheirOnchain.Key, key)) {
			if err := channel.Our.Verify(d.pubKey); err != nil {
				log.Warn().Msg(channel.Our.State.Dump())
				return nil, fmt.Errorf("looks like our db state was tampered: %w", err)
			}
			if err := channel.Their.Verify(channel.TheirOnchain.Key); err != nil {
				log.Warn().Msg(channel.Their.State.Dump())
				return nil, fmt.Errorf("looks like their db state was tampered: %w", err)
			}

			channels = append(channels, channel)
		}
	}

	if err := iter.Error(); err != nil {
		return nil, err
	}

	return channels, nil
}
