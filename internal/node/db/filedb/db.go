package filedb

import (
	"encoding/json"
	"fmt"
	"github.com/xssnick/payment-network/internal/node/db"
	"os"
	"sync"
	"time"
)

type FileDB struct {
	dir string
	mx  sync.Mutex
}

func NewFileDB(dir string) *FileDB {
	return &FileDB{dir: dir}
}

func (d *FileDB) SetBlockOffset(seqno uint32) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	_ = os.MkdirAll(d.dir+"/block/", os.ModePerm)

	path := d.dir + "/block/master.json"
	fl, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer fl.Close()

	if err = json.NewEncoder(fl).Encode(db.BlockOffset{
		Seqno:     seqno,
		UpdatedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to encode json to file: %w", err)
	}
	return nil
}

func (d *FileDB) GetBlockOffset() (*db.BlockOffset, error) {
	d.mx.Lock()
	defer d.mx.Unlock()

	path := d.dir + "/block/master.json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, db.ErrNotFound
	}

	fl, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	var off *db.BlockOffset
	if err = json.NewDecoder(fl).Decode(&off); err != nil {
		return nil, fmt.Errorf("failed to decode json file: %w", err)
	}
	return off, nil
}
