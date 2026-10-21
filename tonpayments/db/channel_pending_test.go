package db_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestChannelPendingStateSurvivesRestart(t *testing.T) {
	for _, populated := range []bool{false, true} {
		name := "empty"
		if populated {
			name = "populated"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			database := openPendingTestDB(t, path)
			channel, body := pendingTestChannel(t, populated)
			if err := database.SaveChannelPendingState(t.Context(), channel, body); err != nil {
				t.Fatal(err)
			}
			assertPendingState(t, database, channel, body)

			database.Close()
			database = openPendingTestDB(t, path)
			assertPendingState(t, database, channel, body)
		})
	}
}

func TestChannelPendingStateRollback(t *testing.T) {
	database := openPendingTestDB(t, filepath.Join(t.TempDir(), "db"))
	channel, body := pendingTestChannel(t, true)
	if err := database.CreateChannel(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveChannelPendingState(t.Context(), channel, body); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetChannel(t.Context(), channel.Our.Address)
	if err != nil {
		t.Fatal(err)
	}

	next := body
	next.Seqno++
	aborted := errors.New("abort pending state transaction")
	err = database.Transaction(t.Context(), func(ctx context.Context) error {
		if err := database.SaveChannelPendingState(ctx, channel, next); err != nil {
			return err
		}
		if err := database.CleanupChannelPendingStates(ctx, channel, body); err != nil {
			return err
		}
		channel.Their.Data = db.NewAgreedData()
		if err := database.UpdateChannel(ctx, channel); err != nil {
			return err
		}
		return aborted
	})
	if !errors.Is(err, aborted) {
		t.Fatalf("expected transaction rollback, got %v", err)
	}
	assertPendingState(t, database, before, body)
	if _, err := database.GetChannelPendingState(t.Context(), before, next); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("rolled-back pending state must not exist, got %v", err)
	}
	after, err := database.GetChannel(t.Context(), channel.Our.Address)
	if err != nil {
		t.Fatal(err)
	}
	assertAgreedData(t, after.Their.Data, before.Their.Data)
}

func openPendingTestDB(t *testing.T, path string) *db.DB {
	t.Helper()
	storage, _, err := leveldb.NewLevelDB(path)
	if err != nil {
		t.Fatal(err)
	}
	database := db.NewDB(storage, nil)
	t.Cleanup(database.Close)
	return database
}

func pendingTestChannel(t *testing.T, populated bool) (*db.Channel, payments.StateBody) {
	t.Helper()
	channel := &db.Channel{
		Our:   db.Side{Address: "pending-state-test", Data: db.NewAgreedData()},
		Their: db.Side{Data: db.NewAgreedData()},
	}
	if populated {
		for i, dict := range []*cell.Dictionary{
			channel.Our.Data.Conditionals, channel.Our.Data.ActionStates,
			channel.Their.Data.Conditionals, channel.Their.Data.ActionStates,
		} {
			key := cell.BeginCell().MustStoreUInt(uint64(i+1), 256).EndCell()
			value := cell.BeginCell().MustStoreUInt(uint64(i+10), 64).
				MustStoreRef(cell.BeginCell().MustStoreUInt(uint64(i+20), 32).EndCell()).EndCell()
			if err := dict.Set(key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	body := payments.StateBody{
		ChannelID: bytes.Repeat([]byte{1}, 16),
		Seqno:     7,
		A:         pendingTestSide(channel.Our.Data),
		B:         pendingTestSide(channel.Their.Data),
	}
	return channel, body
}

func pendingTestSide(data db.AgreedData) payments.StateSide {
	return payments.StateSide{
		ConditionalsHash: pendingTestDictHash(data.Conditionals),
		ActionStatesHash: pendingTestDictHash(data.ActionStates),
	}
}

func pendingTestDictHash(dict *cell.Dictionary) []byte {
	if dict.IsEmpty() {
		return make([]byte, 32)
	}
	return dict.AsCell().Hash()
}

func assertPendingState(t *testing.T, database *db.DB, channel *db.Channel, body payments.StateBody) {
	t.Helper()
	state, err := database.GetChannelPendingState(t.Context(), channel, body)
	if err != nil {
		t.Fatalf("saved pending state cannot be restored: %v", err)
	}
	if state.Seqno != body.Seqno {
		t.Fatalf("seqno = %d, want %d", state.Seqno, body.Seqno)
	}
	assertAgreedData(t, state.OurData, channel.Our.Data)
	assertAgreedData(t, state.TheirData, channel.Their.Data)
}

func assertAgreedData(t *testing.T, got, want db.AgreedData) {
	t.Helper()
	if got.Conditionals == nil || got.ActionStates == nil {
		t.Fatal("restored dictionaries must be initialized")
	}
	if !bytes.Equal(pendingTestDictHash(got.Conditionals), pendingTestDictHash(want.Conditionals)) ||
		!bytes.Equal(pendingTestDictHash(got.ActionStates), pendingTestDictHash(want.ActionStates)) {
		t.Fatal("restored dictionaries differ from saved state")
	}
}
