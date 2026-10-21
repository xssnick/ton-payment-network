//go:build !(js && wasm)

package wallet_test

import (
	"crypto/ed25519"
	"testing"

	nodewallet "github.com/xssnick/ton-payment-network/tonpayments/wallet"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

func TestHighloadQueryIDsSkipInvalidBitmapPositions(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	w, err := nodewallet.InitWallet(nil, key)
	if err != nil {
		t.Fatal(err)
	}
	messages := []*wallet.Message{
		wallet.SimpleMessage(w.WalletAddress(), tlb.MustFromTON("0.01"), nil),
	}

	// The low 15 bits span 32 bitmaps with 1023 usable positions each.
	const capacity = 32 * 1023
	seen := make(map[uint64]bool, capacity)
	var first uint64
	for i := 0; i <= capacity; i++ {
		msg, err := w.Wallet().PrepareExternalMessageForMany(t.Context(), false, messages)
		if err != nil {
			t.Fatalf("message %d: %v", i+1, err)
		}
		body := msg.Body.MustBeginParse()
		body.MustLoadSlice(512) // signature
		payload := body.MustLoadRef()
		payload.MustLoadUInt(32) // subwallet
		payload.MustLoadRef()    // internal message
		payload.MustLoadUInt(8)  // send mode
		id := payload.MustLoadUInt(23)
		createdAt := payload.MustLoadUInt(64)
		if id&1023 == 1023 {
			t.Fatalf("message %d: query ID %d uses invalid bitmap position 1023", i+1, id)
		}
		if id>>15 != createdAt%210 {
			t.Fatalf("message %d: query ID %d has an incorrect time prefix for %d", i+1, id, createdAt)
		}
		low := id & ((1 << 15) - 1)
		if i == capacity {
			if low != first {
				t.Fatalf("query ID did not wrap after %d valid positions: got %d, want %d", capacity, low, first)
			}
			break
		}
		if seen[low] {
			t.Fatalf("message %d: query ID position %d was reused before exhausting the time bucket", i+1, low)
		}
		seen[low] = true
		if i == 0 {
			first = low
		}
	}
}
