package tonpayments

import (
	"crypto/ed25519"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/actions"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestExecuteConditionalSettlementVirtual(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                      string
		amount, fee, prepay, want int64
	}{
		{"fee", 100, 10, 0, 110},
		{"partial_prepay", 100, 10, 40, 70},
		{"full_prepay", 100, 10, 110, 0},
		{"excess_prepay", 100, 10, 120, 0},
		{"zero_with_fee", 0, 10, 0, 10},
		{"zero_partial_fee_prepay", 0, 10, 4, 6},
		{"zero_prepaid_fee", 0, 10, 10, 0},
		{"zero_without_fee", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coin := &payments.CoinConfig{BalanceID: payments.GetTONBalanceID(), Decimals: 9}
			cond := &conditionals.ConditionalVirtualChannel{Key: pub, Capacity: big.NewInt(1000), Fee: big.NewInt(tc.fee), Prepay: big.NewInt(tc.prepay), Action: &actions.ActionSendTonInsured{Coin: coin}}
			state := conditionals.VirtualChannelState{Amount: big.NewInt(tc.amount)}
			state.Sign(key)
			resolve, err := state.ToCell()
			if err != nil {
				t.Fatal(err)
			}
			initial := actions.StateActionSend{Amount: actions.Coins{Val: big.NewInt(200)}, Commited: actions.Coins{Val: big.NewInt(50)}, CommitedSeqno: 7}
			before, err := tlb.ToCell(initial)
			if err != nil {
				t.Fatal(err)
			}
			locked := map[string]*payments.LockedDepositInfo{coin.BalanceID: {Amount: big.NewInt(1000), Used: big.NewInt(5), Till: time.Now().Add(time.Hour)}}
			after, err := executeConditionalSettlement(cond, before, resolve, locked)
			if err != nil {
				t.Fatal(err)
			}
			var got actions.StateActionSend
			if err = payments.LoadState(&got, after); err != nil {
				t.Fatal(err)
			}
			if got.Amount.Nano().Cmp(big.NewInt(200+tc.want)) != 0 || got.Commited.Nano().Int64() != 50 || got.CommitedSeqno != 7 {
				t.Fatalf("unexpected action state: %+v", got)
			}
			if locked[coin.BalanceID].Used.Cmp(big.NewInt(5+tc.want)) != 0 {
				t.Fatalf("locked deposit used=%s want=%d", locked[coin.BalanceID].Used, 5+tc.want)
			}
		})
	}
	t.Run("invalid_signature", func(t *testing.T) {
		state := conditionals.VirtualChannelState{Amount: big.NewInt(0), Signature: make([]byte, 64)}
		resolve, err := state.ToCell()
		if err != nil {
			t.Fatal(err)
		}
		cond := &conditionals.ConditionalVirtualChannel{Key: pub}
		if _, err = executeConditionalSettlement(cond, cell.BeginCell().EndCell(), resolve, nil); err == nil {
			t.Fatal("invalid signature accepted for zero amount")
		}
	})
}

func TestExecuteConditionalSettlementResolvable(t *testing.T) {
	key := make([]byte, 32)
	for _, initiator := range []bool{false, true} {
		for _, amount := range []int64{0, 100} {
			name := "counterparty"
			if initiator {
				name = "initiator"
			}
			if amount == 0 {
				name += "_zero"
			}
			t.Run(name, func(t *testing.T) {
				cond := &conditionals.ConditionalResolvable{Key: key, Amount: big.NewInt(1000), Fee: big.NewInt(10), IsInitiator: initiator, Action: &actions.ActionSendTonInsured{Coin: &payments.CoinConfig{BalanceID: payments.GetTONBalanceID()}}}
				resolve, err := tlb.ToCell(conditionals.ResolvableState{Key: key, Amount: big.NewInt(amount), At: 1})
				if err != nil {
					t.Fatal(err)
				}
				before := cond.Action.GetEmptyState()
				after, err := executeConditionalSettlement(cond, before, resolve, nil)
				if err != nil {
					t.Fatal(err)
				}
				var got actions.StateActionSend
				if err = payments.LoadState(&got, after); err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if amount > 0 {
					want = amount - 10
					if initiator {
						want = amount + 10
					}
				}
				if got.Amount.Nano().Cmp(big.NewInt(want)) != 0 {
					t.Fatalf("amount=%s want=%d", got.Amount.Nano(), want)
				}
			})
		}
	}
}
