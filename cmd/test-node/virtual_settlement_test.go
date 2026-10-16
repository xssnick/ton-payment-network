package testnode

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestIntegration_VirtualChannelSettlement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	for _, tc := range []struct {
		name              string
		nodes             int
		automatic         bool
		amount, committed string
	}{
		{"direct_final_state", 2, true, "1", ""},
		{"routed_final_state", 3, true, "1", ""},
		{"partial_prepay", 2, false, "1", "0.4"},
		{"full_prepay", 2, false, "1", "1"},
		{"zero_with_fee", 2, false, "0", ""},
		{"zero_prepaid_fee", 2, false, "0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			hub := newLoopbackHub()
			nodes := make([]*testNode, tc.nodes)
			for i := range nodes {
				nodes[i] = newTestNode(t, hub, i+1, 21000+tc.nodes*10+i)
				nodes[i].start()
				defer nodes[i].stop(t)
			}
			type endpoint struct {
				node    *testNode
				address string
				before  *big.Int
				delta   *big.Int
			}
			var ends []endpoint
			sender, receiver := nodes[0], nodes[len(nodes)-1]
			cc, err := sender.svc.ResolveCoinConfigBySymbol("TON")
			if err != nil {
				t.Fatal(err)
			}
			balance := func(n *testNode, addr string) *big.Int {
				ch, err := n.svc.GetChannel(ctx, addr)
				if err != nil {
					t.Fatal(err)
				}
				balances, err := ch.CalcBalance(ctx, false, n.svc)
				if err != nil {
					t.Fatal(err)
				}
				return new(big.Int).Set(balances[cc.BalanceID].Available())
			}
			amount := tlb.MustFromTON(tc.amount).Nano()
			var chain []transport.TunnelChainPart
			for i := 0; i < len(nodes)-1; i++ {
				a, b := openAndSeedChannel(t, nodes[i], nodes[i+1], tlb.MustFromTON("5").Nano())
				fee := big.NewInt(int64(len(nodes)-1-i) * 10_000_000)
				paid := new(big.Int).Add(amount, fee)
				ends = append(ends, endpoint{nodes[i], a.Our.Address, balance(nodes[i], a.Our.Address), new(big.Int).Neg(paid)}, endpoint{nodes[i+1], b.Our.Address, balance(nodes[i+1], b.Our.Address), new(big.Int).Set(paid)})
				chain = append(chain, transport.TunnelChainPart{Target: nodes[i+1].pub, Capacity: tlb.MustFromTON("1").Nano(), Fee: fee, Deadline: time.Now().Add(time.Duration(len(nodes)-1-i) * 24 * time.Hour)})
			}
			_, key, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			pub := key.Public().(ed25519.PublicKey)
			ik, ins, err := transport.GenerateTunnel(key, chain, 5, tc.automatic, sender.svc.GetPrivateKey(), cc)
			if err != nil {
				t.Fatal(err)
			}
			if err = sender.svc.CreateSendConditional(ctx, ik, key, chain[0], chain[len(chain)-1], ins, cc); err != nil {
				t.Fatal(err)
			}
			signed := func(amount string) *cell.Cell {
				state := conditionals.VirtualChannelState{Amount: tlb.MustFromTON(amount).Nano()}
				state.Sign(key)
				c, err := state.ToCell()
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			if !tc.automatic {
				waitVirtualMeta(t, sender, pub, false, true)
				waitVirtualMeta(t, receiver, pub, true, false)
				if tc.committed != "" {
					if err = sender.svc.AddConditionalResolve(ctx, pub, signed(tc.committed)); err != nil {
						t.Fatal(err)
					}
					if err = sender.svc.CommitVirtualChannel(ctx, pub); err != nil {
						t.Fatal(err)
					}
					wantPrepay := new(big.Int).Add(tlb.MustFromTON(tc.committed).Nano(), chain[0].Fee)
					waitFor(t, 15*time.Second, 50*time.Millisecond, func() (bool, string) {
						for i, ep := range ends {
							ch, err := ep.node.svc.GetChannel(ctx, ep.address)
							if err != nil {
								return false, err.Error()
							}
							meta, err := ep.node.svc.GetVirtualChannelMeta(ctx, pub)
							if err != nil {
								return false, err.Error()
							}
							var dict *cell.Dictionary
							var id []byte
							if i%2 == 0 {
								dict = ch.Our.Data.Conditionals
								id = meta.Outgoing.Conditional.Hash()
							} else {
								dict = ch.Their.Data.Conditionals
								id = meta.Incoming.Conditional.Hash()
							}
							_, cond, err := payments.FindConditional(ctx, dict, id, ep.node.svc)
							if err != nil {
								return false, err.Error()
							}
							if cond.(*conditionals.ConditionalVirtualChannel).Prepay.Cmp(wantPrepay) != 0 {
								return false, "waiting for prepaid state on both peers"
							}
						}
						return true, "prepay committed on both peers"
					})
					for i, ep := range ends {
						delta := new(big.Int).Sub(balance(ep.node, ep.address), ep.before)
						want := new(big.Int).Set(wantPrepay)
						if i%2 == 0 {
							want.Neg(new(big.Int).Add(chain[0].Capacity, chain[0].Fee))
						}
						if delta.Cmp(want) != 0 {
							t.Fatalf("prepay endpoint %d delta=%s want=%s", i, delta, want)
						}
					}
				}
				if err = receiver.svc.AddConditionalResolve(ctx, pub, signed(tc.amount)); err != nil {
					t.Fatal(err)
				}
				if err = receiver.svc.CloseConditional(ctx, pub); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, 25*time.Second, 50*time.Millisecond, func() (bool, string) {
				for i, ep := range ends {
					ch, err := ep.node.svc.GetChannel(ctx, ep.address)
					if err != nil {
						return false, err.Error()
					}
					if !ch.Our.Data.Conditionals.IsEmpty() || !ch.Their.Data.Conditionals.IsEmpty() {
						return false, fmt.Sprintf("endpoint %d conditionals remain", i)
					}
					meta, err := ep.node.svc.GetVirtualChannelMeta(ctx, pub)
					if err != nil {
						return false, err.Error()
					}
					if meta.Status != dbpkg.ConditionalStateClosed {
						return false, fmt.Sprintf("endpoint %d meta status=%d", i, meta.Status)
					}
					delta := new(big.Int).Sub(balance(ep.node, ep.address), ep.before)
					if delta.Cmp(ep.delta) != 0 {
						return false, fmt.Sprintf("endpoint %d delta=%s want=%s", i, delta, ep.delta)
					}
				}
				return true, "exact balances and conditional cleanup confirmed on every peer"
			})
		})
	}
}
