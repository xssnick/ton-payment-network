//go:build js && wasm

package browser

import (
	"context"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"sync"
	"syscall/js"
	"time"
)

const (
	dbName    = "payments-db"
	storeName = "kv"
	txKey     = "__idbTx"
	dbVersion = 1
)

func bytesToJS(b []byte) js.Value {
	arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(arr, b)
	return arr
}

func jsToBytes(v js.Value) []byte {
	u8 := js.Global().Get("Uint8Array")

	if v.InstanceOf(u8) {
		out := make([]byte, v.Get("byteLength").Int())
		js.CopyBytesToGo(out, v)
		return out
	}

	if v.InstanceOf(js.Global().Get("ArrayBuffer")) {
		arr := u8.New(v) // Uint8Array(view)
		out := make([]byte, arr.Get("byteLength").Int())
		js.CopyBytesToGo(out, arr)
		return out
	}

	s := js.Global().Get("String").Invoke(v).String()
	return []byte(s)
}

func await(p js.Value) (js.Value, error) {
	ch := make(chan struct{})
	var res js.Value
	var err error

	then := js.FuncOf(func(this js.Value, args []js.Value) any {
		res = args[0]
		close(ch)
		return nil
	})
	defer then.Release()

	catch := js.FuncOf(func(this js.Value, args []js.Value) any {
		reason := args[0]
		var msg string

		if reason.Type() == js.TypeObject {
			if reason.Get("message").Truthy() {
				msg = reason.Get("message").String()
			} else {
				msg = js.Global().Get("JSON").Call("stringify", reason).String()
			}
		} else {
			msg = reason.String()
		}

		err = errors.New(msg)
		close(ch)
		return nil
	})

	defer catch.Release()

	p.Call("then", then).Call("catch", catch)
	<-ch

	return res, err
}

func wrapIDBRequest(req js.Value) js.Value {
	return js.Global().Get("Promise").New(js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]

		var onSuccess js.Func
		var onError js.Func

		onSuccess = js.FuncOf(func(this js.Value, args []js.Value) any {
			resolve.Invoke(req.Get("result"))
			onSuccess.Release()
			onError.Release()
			return nil
		})

		onError = js.FuncOf(func(this js.Value, args []js.Value) any {
			reject.Invoke(req.Get("error").Get("message").String())
			onSuccess.Release()
			onError.Release()
			return nil
		})

		req.Set("onsuccess", onSuccess)
		req.Set("onerror", onError)
		return nil
	}))
}

type IndexedDB struct {
	db js.Value
	m  sync.Mutex
}

func NewIndexedDB() (*IndexedDB, bool, error) {
	op := js.Global().Get("indexedDB").Call("open", dbName, dbVersion)

	fresh := false
	up := js.FuncOf(func(this js.Value, args []js.Value) any {
		args[0].Get("target").Get("result").Call("createObjectStore", storeName)
		fresh = true
		return nil
	})
	op.Set("onupgradeneeded", up)
	defer up.Release()

	done := make(chan error)
	var dbVal js.Value

	onSuccess := js.FuncOf(func(this js.Value, args []js.Value) any {
		dbVal = op.Get("result")
		done <- nil
		return nil
	})
	op.Set("onsuccess", onSuccess)
	defer onSuccess.Release()

	onErr := js.FuncOf(func(this js.Value, args []js.Value) any {
		e := op.Get("error").Get("message").String()
		done <- fmt.Errorf(e)
		return nil
	})
	op.Set("onerror", onErr)
	defer onErr.Release()

	if err := <-done; err != nil {
		return nil, false, err
	}

	rp, err := requestPersistentStorage()
	if err != nil {
		return nil, false, err
	}
	if !rp {
		println("[WARNING] Persistent storage denied, db is not safe, use with caution!")
	}

	return &IndexedDB{db: dbVal}, fresh, nil
}

func requestPersistentStorage() (bool, error) {
	storage := js.Global().Get("navigator").Get("storage")
	if storage.IsUndefined() || !storage.Truthy() {
		return false, errors.New("navigator.storage is not supported")
	}

	persisted, err := await(storage.Call("persisted"))
	if err == nil && persisted.Bool() {
		return true, nil
	}

	result, err := await(storage.Call("persist"))
	if err != nil {
		return false, err
	}

	return result.Bool(), nil
}

func estimateStorage() (usage uint64, quota uint64, err error) {
	storage := js.Global().Get("navigator").Get("storage")
	if storage.IsUndefined() || !storage.Truthy() {
		return 0, 0, errors.New("navigator.storage is not supported")
	}

	result, err := await(storage.Call("estimate"))
	if err != nil {
		return 0, 0, err
	}

	usageVal := result.Get("usage")
	quotaVal := result.Get("quota")

	return uint64(usageVal.Float()), uint64(quotaVal.Float()), nil
}

func (d *IndexedDB) Close() {
	d.db.Call("close")
}

type executor interface {
	Put(key, value []byte) error
	Delete(key []byte) error
	Get(key []byte) ([]byte, error)
	Has(key []byte) (bool, error)
	NewIterator(p []byte, forward bool) Iterator
}

type snapshotExec struct {
	tx js.Value
}

func (e snapshotExec) Get(k []byte) ([]byte, error) {
	req := e.tx.Call("get", bytesToJS(k))
	res, err := await(wrapIDBRequest(req))
	if err != nil {
		return nil, err
	}
	if res.IsUndefined() {
		return nil, db.ErrNotFound
	}
	return jsToBytes(res), nil
}

func (e snapshotExec) Has(k []byte) (bool, error) {
	_, err := e.Get(k)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (e snapshotExec) Put(_, _ []byte) error { return errors.New("readonly") }
func (e snapshotExec) Delete(_ []byte) error { return errors.New("readonly") }
func (e snapshotExec) Error() error          { return nil }

/*func (e snapshotExec) NewIterator(p []byte, forward bool) Iterator {
	keyRange := js.Undefined()

	if p != nil {
		upperKey := make([]byte, len(p))
		copy(upperKey, p)

		for i := len(upperKey) - 1; i >= 0; i-- {
			if upperKey[i] < 0xFF {
				upperKey[i]++
				upperKey = upperKey[:i+1]
				break
			}
		}

		keyRange = js.Global().Get("IDBKeyRange").Call(
			"bound",
			bytesToJS(p),
			bytesToJS(upperKey),
			false,
			true,
		)
	}

	dir := "prev"
	if forward {
		dir = "next"
	}

	req := e.tx.Call("openCursor", keyRange, dir)
	chRes := make(chan js.Value, 1)
	chErr := make(chan error, 1)

	var onOk, onErr js.Func
	onOk = js.FuncOf(func(this js.Value, args []js.Value) any {
		cur := args[0].Get("target").Get("result")
		chRes <- cur
		return nil
	})
	onErr = js.FuncOf(func(this js.Value, args []js.Value) any {
		chErr <- errors.New(args[0].Get("message").String())
		return nil
	})
	req.Set("onsuccess", onOk)
	req.Set("onerror", onErr)

	return &sliceIter{
		prefix: p,
		onOk:   onOk,
		onErr:  onErr,
		chRes:  chRes,
		chErr:  chErr,
	}
}*/

func (e snapshotExec) NewIterator(prefix []byte, forward bool) Iterator {
	keyRange := js.Undefined()
	if prefix != nil {
		upper := make([]byte, len(prefix))
		copy(upper, prefix)
		for i := len(upper) - 1; i >= 0; i-- {
			if upper[i] < 0xFF {
				upper[i]++
				upper = upper[:i+1]
				break
			}
		}
		keyRange = js.Global().Get("IDBKeyRange").Call(
			"bound",
			bytesToJS(prefix),
			bytesToJS(upper),
			false, // lowerOpen
			true,  // upperOpen
		)
	}

	dir := "prev"
	if forward {
		dir = "next"
	}

	req := e.tx.Call("openCursor", keyRange, dir)

	chRes := make(chan js.Value, 1)
	chErr := make(chan error, 1)

	var onOk, onErr js.Func

	onOk = js.FuncOf(func(this js.Value, args []js.Value) any {
		cur := args[0].Get("target").Get("result")

		if !cur.IsNull() {
			cur.Call("continue")
		}

		chRes <- cur
		return nil
	})

	onErr = js.FuncOf(func(this js.Value, args []js.Value) any {
		chErr <- errors.New(args[0].Get("message").String())
		return nil
	})

	req.Set("onsuccess", onOk)
	req.Set("onerror", onErr)

	return &sliceIter{
		release: func() {
			req.Set("onsuccess", js.Undefined())
			req.Set("onerror", js.Undefined())
			onOk.Release()
			onErr.Release()
		},
		chRes: chRes,
		chErr: chErr,
	}
}

type batchWrap struct{ ops []func(js.Value) }

func (b *batchWrap) Put(key, val []byte) error {
	b.ops = append(b.ops, func(store js.Value) { store.Call("put", bytesToJS(val), bytesToJS(key)) })
	return nil
}

func (b *batchWrap) Delete(key []byte) error {
	b.ops = append(b.ops, func(store js.Value) { store.Call("delete", bytesToJS(key)) })
	return nil
}

type sliceIter struct {
	currentKey   []byte
	currentValue []byte
	release      func()
	chRes        chan js.Value
	chErr        chan error
}

func (it *sliceIter) Next() bool {
	for {
		select {
		case cur := <-it.chRes:
			if cur.IsNull() {
				return false
			}
			it.currentKey = jsToBytes(cur.Get("key"))
			it.currentValue = jsToBytes(cur.Get("value"))
			// cur.Call("continue")
			return true
		case err := <-it.chErr:
			panic(err.Error())
		}
	}
}

func (it *sliceIter) Key() []byte {
	return it.currentKey
}

func (it *sliceIter) Value() []byte {
	return it.currentValue
}

func (it *sliceIter) Release() {
	it.release()
}

func (it *sliceIter) Error() error { return nil }

type Tx struct {
	Snapshot
	batchWrap
}

func (d *IndexedDB) Transaction(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := ctx.Value(txKey).(*Tx); ok {
		return fn(ctx)
	}
	d.m.Lock()
	defer d.m.Unlock()

	usage, left, err := estimateStorage()
	if err != nil {
		return err
	}

	if left < 1024*1024 {
		return errors.New("not enough storage left in indexed db " + fmt.Sprintf("%d/%d", left, usage))
	}

	ro := d.db.Call("transaction", storeName, "readonly").Call("objectStore", storeName)
	tx := &Tx{Snapshot: &snapshotExec{tx: ro}}

	if err := fn(context.WithValue(ctx, txKey, tx)); err != nil {
		return err
	}
	if len(tx.ops) == 0 {
		return nil
	}

	tr := d.db.Call("transaction", storeName, "readwrite")
	store := tr.Call("objectStore", storeName)
	for _, op := range tx.ops {
		op(store)
	}

	done := make(chan error)
	onc := js.FuncOf(func(this js.Value, args []js.Value) any {
		done <- nil
		return nil
	})
	defer onc.Release()

	one := js.FuncOf(func(this js.Value, args []js.Value) any {
		done <- fmt.Errorf(tr.Get("error").Get("message").String())
		return nil
	})
	defer one.Release()

	tr.Set("oncomplete", onc)
	tr.Set("onerror", one)

	return <-done
}

type rootExec struct {
	db js.Value
}

func (r *rootExec) Put(k, v []byte) error {
	tx := r.db.Call("transaction", storeName, "readwrite").Call("objectStore", storeName)
	_, err := await(wrapIDBRequest(tx.Call("put", bytesToJS(v), bytesToJS(k))))
	return err
}

func (r *rootExec) Delete(k []byte) error {
	tx := r.db.Call("transaction", storeName, "readwrite").Call("objectStore", storeName)
	_, err := await(wrapIDBRequest(tx.Call("delete", bytesToJS(k))))
	return err
}

func (r *rootExec) Get(k []byte) ([]byte, error) {
	tx := r.db.Call("transaction", storeName, "readonly").Call("objectStore", storeName)
	res, err := await(wrapIDBRequest(tx.Call("get", bytesToJS(k))))
	if err != nil {
		return nil, err
	}

	if res.IsUndefined() {
		return nil, db.ErrNotFound
	}

	return jsToBytes(res), nil
}

func (r *rootExec) Has(k []byte) (bool, error) {
	_, err := r.Get(k)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (r *rootExec) NewIterator(p []byte, forward bool) Iterator {
	sn := snapshotExec{tx: r.db.Call("transaction", storeName, "readonly").Call("objectStore", storeName)}
	return sn.NewIterator(p, forward)
}

type Executor struct {
	e executor
}

func (e Executor) Put(k, v []byte) error                          { return e.e.Put(k, v) }
func (e Executor) Delete(k []byte) error                          { return e.e.Delete(k) }
func (e Executor) Get(k []byte) ([]byte, error)                   { return e.e.Get(k) }
func (e Executor) Has(k []byte) (bool, error)                     { return e.e.Has(k) }
func (e Executor) NewIterator(p []byte, forward bool) db.Iterator { return e.e.NewIterator(p, forward) }

func (d *IndexedDB) GetExecutor(ctx context.Context) db.Executor {
	if tx, ok := ctx.Value(txKey).(*Tx); ok {
		return &Executor{e: tx}
	}
	return &Executor{e: &rootExec{db: d.db}}
}

func (d *IndexedDB) Backup() error {
	backupName := fmt.Sprintf("%s_backup_%d", dbName, time.Now().UnixMilli())

	openReq := js.Global().Get("indexedDB").Call("open", backupName, dbVersion)
	up := js.FuncOf(func(this js.Value, args []js.Value) any {
		db := args[0].Get("target").Get("result")
		db.Call("createObjectStore", storeName)
		return nil
	})
	openReq.Set("onupgradeneeded", up)
	defer up.Release()

	openRes, err := await(wrapIDBRequest(openReq))
	if err != nil {
		return fmt.Errorf("backup open: %w", err)
	}

	ex := d.GetExecutor(context.Background())
	it := ex.NewIterator(nil, true)
	defer it.Release()

	for it.Next() {
		k := it.Key()
		v := it.Value()

		tx := openRes.Call("transaction", storeName, "readwrite").Call("objectStore", storeName)
		_, err := await(wrapIDBRequest(tx.Call("put", bytesToJS(v), bytesToJS(k))))
		if err != nil {
			return fmt.Errorf("backup put: %w", err)
		}
	}

	if err = it.Error(); err != nil {
		return err
	}

	return nil
}
