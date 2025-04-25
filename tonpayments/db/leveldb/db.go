package leveldb

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"sync"
)

type DB struct {
	_db    *leveldb.DB
	pubKey ed25519.PublicKey

	onChannelStateChange func(ctx context.Context, ch *db.Channel, statusChanged bool)

	mx sync.Mutex
}

type Tx struct {
	*leveldb.Snapshot
	batchWrap
}

type batchWrap struct {
	b *leveldb.Batch
}

func (b batchWrap) Put(key, value []byte, wo *opt.WriteOptions) error {
	if !wo.GetSync() {
		panic("must be sync write")
	}

	b.b.Put(key, value)
	return nil
}

func (b batchWrap) Delete(key []byte, wo *opt.WriteOptions) error {
	if !wo.GetSync() {
		panic("must be sync write")
	}

	b.b.Delete(key)
	return nil
}

type executor interface {
	Put(key, value []byte, wo *opt.WriteOptions) error
	Delete(key []byte, wo *opt.WriteOptions) error
	Get(key []byte, ro *opt.ReadOptions) (value []byte, err error)
	Has(key []byte, ro *opt.ReadOptions) (ret bool, err error)
	NewIterator(slice *util.Range, ro *opt.ReadOptions) iterator.Iterator
}

func NewDB(path string, pubKey ed25519.PublicKey) (*DB, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, err
	}

	return &DB{
		_db:    db,
		pubKey: pubKey,
	}, nil
}

func (d *DB) Close() {
	d._db.Close()
}

const txKey = "__ldbTx"

// Transaction - kinda ACID achievement using leveldb
func (d *DB) Transaction(ctx context.Context, f func(ctx context.Context) error) error {
	tx, ok := ctx.Value(txKey).(*Tx)
	if ok {
		// already inside tx
		return f(ctx)
	}

	// lock gives us consistency
	d.mx.Lock()
	defer d.mx.Unlock()

	// snapshot gives us kinda reads isolation
	snap, err := d._db.GetSnapshot()
	if err != nil {
		return fmt.Errorf("failed to get db snapshot: %w", err)
	}
	defer snap.Release()

	tx = &Tx{
		batchWrap: batchWrap{new(leveldb.Batch)},
		Snapshot:  snap,
	}

	if err := f(context.WithValue(ctx, txKey, tx)); err != nil {
		return err
	}

	// batches are atomic, and durable when sync = true
	if err := d._db.Write(tx.batchWrap.b, &opt.WriteOptions{
		Sync: true,
	}); err != nil {
		return fmt.Errorf("failed to write batch to db: %w", err)
	}
	return nil
}

func (d *DB) getExecutor(ctx context.Context) executor {
	if tx, ok := ctx.Value(txKey).(*Tx); ok {
		return tx
	}
	return d._db
}
