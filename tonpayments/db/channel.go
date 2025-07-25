package db

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"time"
)

func (d *DB) SetOnChannelUpdated(f func(ctx context.Context, ch *Channel, statusChanged bool)) {
	d.onChannelStateChange = f
}

func (d *DB) SetOnChannelHistoryUpdated(f func(ctx context.Context, ch *Channel, item ChannelHistoryItem)) {
	d.onChannelHistoryUpdate = f
}

func (d *DB) GetOnChannelUpdated() func(ctx context.Context, ch *Channel, statusChanged bool) {
	return d.onChannelStateChange
}

func (d *DB) GetOnChannelHistoryUpdated() func(ctx context.Context, ch *Channel, item ChannelHistoryItem) {
	return d.onChannelHistoryUpdate
}

func (d *DB) AddUrgentPeer(ctx context.Context, peerAddress []byte) error {
	if len(peerAddress) != 32 {
		return fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
	}

	key := []byte("urgent-peer:" + base64.StdEncoding.EncodeToString(peerAddress))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existence: %w", err)
		}
		if has {
			// Peer already exists, no need to add
			return nil
		}

		if err = tx.Put(key, []byte{}); err != nil {
			return fmt.Errorf("failed to add urgent peer to db: %w", err)
		}
		return nil
	})
}

func (d *DB) RemoveUrgentPeer(ctx context.Context, peerAddress []byte) error {
	if len(peerAddress) != 32 {
		return fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
	}

	key := []byte("urgent-peer:" + base64.StdEncoding.EncodeToString(peerAddress))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existence: %w", err)
		}
		if !has {
			// reverse compatibility
			key = []byte("urgent-peer:" + string(peerAddress))

			has, err = tx.Has(key)
			if err != nil {
				return fmt.Errorf("failed to check alt existence: %w", err)
			}
			if !has {
				// Peer doesn't exist, nothing to remove
				return nil
			}
		}

		if err = tx.Delete(key); err != nil {
			return fmt.Errorf("failed to remove urgent peer from db: %w", err)
		}
		return nil
	})
}

func (d *DB) GetUrgentPeers(ctx context.Context) ([][]byte, error) {
	tx := d.storage.GetExecutor(ctx)

	iter := tx.NewIterator([]byte("urgent-peer:"), true)
	defer iter.Release()

	var peers [][]byte
	for iter.Next() {
		peerAddress := iter.Key()[len("urgent-peer:"):]
		if len(peerAddress) != 32 { // reverse compatibility before migration
			peerAddress, _ = base64.StdEncoding.DecodeString(string(peerAddress))
			if len(peerAddress) != 32 {
				return nil, fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
			}
		}
		peers = append(peers, append([]byte{}, peerAddress...))
	}

	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("failed to retrieve urgent peers: %w", err)
	}

	return peers, nil
}

func (d *DB) CreateChannel(ctx context.Context, channel *Channel) error {
	key := []byte("ch:" + channel.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return ErrAlreadyExists
		}

		channel.DBVersion = time.Now().UnixNano()
		data, err := json.Marshal(channel)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil {
			d.onChannelStateChange(ctx, channel, true)
		}
		return nil
	})
}

func (d *DB) UpdateChannel(ctx context.Context, channel *Channel) error {
	key := []byte("ch:" + channel.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

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

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil {
			d.onChannelStateChange(ctx, channel, curChannel.Status != channel.Status)
		}

		return nil
	})
}

func (d *DB) GetChannel(ctx context.Context, addr string) (*Channel, error) {
	tx := d.storage.GetExecutor(ctx)

	key := []byte("ch:" + addr)

	data, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var channel *Channel
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

func (d *DB) GetChannels(ctx context.Context, key ed25519.PublicKey, status ChannelStatus) ([]*Channel, error) {
	tx := d.storage.GetExecutor(ctx)

	iter := tx.NewIterator([]byte("ch:"), true)
	defer iter.Release()

	// TODO: optimize, use indexing
	var channels []*Channel
	for iter.Next() {
		var channel *Channel
		if err := json.Unmarshal(iter.Value(), &channel); err != nil {
			return nil, fmt.Errorf("failed to decode json data: %w", err)
		}

		if (status == ChannelStateAny || channel.Status == status) && (key == nil || bytes.Equal(channel.TheirOnchain.Key, key)) {
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

func (d *DB) CreateChannelEvent(ctx context.Context, channel *Channel, at time.Time, item ChannelHistoryItem) error {
	key := channel.getChannelHistoryIndexKey(at, item.Action)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return nil
		}

		data, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelHistoryUpdate != nil {
			d.onChannelHistoryUpdate(ctx, channel, item)
		}

		return nil
	})
}

func (d *DB) GetChannelsHistoryByPeriod(
	ctx context.Context, addr string, limit int,
	before, after *time.Time,
) ([]ChannelHistoryItem, error) {
	tx := d.storage.GetExecutor(ctx)

	historyKeyPrefix := []byte("chs:" + addr + ":")
	iter := tx.NewIterator(historyKeyPrefix, false)
	defer iter.Release()

	var results []ChannelHistoryItem

	for iter.Next() {
		k := iter.Key()

		if len(k) < len(historyKeyPrefix)+8 {
			continue
		}

		tsBytes := k[len(historyKeyPrefix) : len(historyKeyPrefix)+8]
		ts := time.Unix(0, int64(binary.BigEndian.Uint64(tsBytes)))

		if after != nil && ts.Before(*after) {
			continue
		}
		if before != nil && ts.After(*before) {
			break
		}

		var hist ChannelHistoryItem
		if err := json.Unmarshal(iter.Value(), &hist); err != nil {
			return nil, fmt.Errorf("failed to decode history json: %w", err)
		}
		hist.At = ts

		results = append(results, hist)

		if limit > 0 && len(results) >= limit {
			break
		}
	}

	return results, nil
}

func (ch *Channel) getChannelHistoryIndexKey(at time.Time, typ ChannelHistoryEventType) []byte {
	atBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(atBytes, uint64(at.UTC().UnixNano()))

	return append(append([]byte("chs:"+ch.Address+":"), atBytes...), fmt.Sprintf("%d", typ)...)
}
