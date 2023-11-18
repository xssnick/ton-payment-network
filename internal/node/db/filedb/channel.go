package filedb

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/xssnick/payment-network/internal/node/db"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (d *FileDB) CreateChannel(channel *db.Channel) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	_ = os.MkdirAll(d.dir+"/channels/", os.ModePerm)

	path := d.dir + "/channels/" + channel.Address + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		fl, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		defer fl.Close()

		if err = json.NewEncoder(fl).Encode(channel); err != nil {
			return fmt.Errorf("failed to encode json to file: %w", err)
		}
		return nil
	}

	if err == nil {
		return db.ErrAlreadyExists
	}
	return err
}

func (d *FileDB) UpdateChannel(channel *db.Channel) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	path := d.dir + "/channels/" + channel.Address + ".json"

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	fl, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	if err = json.NewEncoder(fl).Encode(channel); err != nil {
		return fmt.Errorf("failed to write json file: %w", err)
	}
	return nil
}

func (d *FileDB) GetChannel(addr string) (*db.Channel, error) {
	d.mx.Lock()
	defer d.mx.Unlock()
	path := d.dir + "/channels/" + addr + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, db.ErrNotFound
	}

	fl, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	var channel *db.Channel
	if err = json.NewDecoder(fl).Decode(&channel); err != nil {
		return nil, fmt.Errorf("failed to decode json file: %w", err)
	}
	return channel, nil
}

func (d *FileDB) GetActiveChannelsWithKey(key ed25519.PublicKey) ([]*db.Channel, error) {
	var channels []*db.Channel
	err := filepath.Walk(d.dir+"/channels/", func(path string, info fs.FileInfo, err error) error {
		if strings.HasSuffix(path, ".json") {
			spl := strings.Split(path, "/")
			name := spl[len(spl)-1]

			ch, err := d.GetChannel(name[:len(name)-len(".json")])
			if err != nil {
				return err
			}

			if ch.Status == db.ChannelStateActive && bytes.Equal(ch.TheirOnchain.Key, key) {
				channels = append(channels, ch)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return channels, nil
}

func (d *FileDB) GetActiveChannels() ([]*db.Channel, error) {
	var channels []*db.Channel
	err := filepath.Walk(d.dir+"/channels/", func(path string, info fs.FileInfo, err error) error {
		if strings.HasSuffix(path, ".json") {
			spl := strings.Split(path, "/")
			name := spl[len(spl)-1]

			ch, err := d.GetChannel(name[:len(name)-len(".json")])
			if err != nil {
				return err
			}

			if ch.Status == db.ChannelStateActive {
				channels = append(channels, ch)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return channels, nil
}

func (d *FileDB) CreateVirtualChannelMeta(meta *db.VirtualChannelMeta) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	_ = os.MkdirAll(d.dir+"/virtual-channels/", os.ModePerm)

	path := d.dir + "/virtual-channels/" + hex.EncodeToString(meta.Key) + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		fl, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		defer fl.Close()

		if err = json.NewEncoder(fl).Encode(meta); err != nil {
			return fmt.Errorf("failed to encode json to file: %w", err)
		}
		return nil
	}

	if err == nil {
		return db.ErrAlreadyExists
	}
	return err
}

func (d *FileDB) UpdateVirtualChannelMeta(meta *db.VirtualChannelMeta) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	path := d.dir + "/virtual-channels/" + hex.EncodeToString(meta.Key) + ".json"

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	fl, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	if err = json.NewEncoder(fl).Encode(meta); err != nil {
		return fmt.Errorf("failed to write json file: %w", err)
	}
	return nil
}

func (d *FileDB) GetVirtualChannelMeta(key []byte) (*db.VirtualChannelMeta, error) {
	d.mx.Lock()
	defer d.mx.Unlock()
	path := d.dir + "/virtual-channels/" + hex.EncodeToString(key) + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, db.ErrNotFound
	}

	fl, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	var channel *db.VirtualChannelMeta
	if err = json.NewDecoder(fl).Decode(&channel); err != nil {
		return nil, fmt.Errorf("failed to decode json file: %w", err)
	}
	return channel, nil
}
