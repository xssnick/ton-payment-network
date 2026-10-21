package testnode

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/ton-payment-network/tonpayments"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var errPendingTestWrite = errors.New("injected pending-state transaction failure")
var errPendingTestReply = errors.New("injected lost proposal response")

func TestIntegration_PendingStateLostResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	for _, prepay := range []bool{false, true} {
		t.Run(pendingOperationName(prepay), func(t *testing.T) {
			flow := newPendingStateFlow(t, 2, nil)
			proposer, peer := flow.parties(prepay)
			lost := flow.loseResponse(t, peer, prepay, false)
			before := flow.channel(t, proposer)
			flow.perform(t, prepay)
			accepted := lost.wait(t)
			flow.assertPending(t, proposer, peer, accepted)
			committed := flow.channel(t, proposer)
			assertPendingData(t, committed.Our.Data, before.Our.Data)
			assertPendingData(t, committed.Their.Data, before.Their.Data)
			if committed.LoadSignedState().Body.Seqno >= accepted.Body.Seqno {
				t.Fatal("proposer committed the proposal before receiving the response")
			}
			lost.release()
			flow.finish(t, prepay)
			if _, err := flow.endpoints[proposer].node.db.GetChannelPendingState(t.Context(), before, accepted.Body); !errors.Is(err, dbpkg.ErrNotFound) {
				t.Fatalf("confirmed proposal must remove its pending state, got %v", err)
			}
		})
	}
}

func TestIntegration_PendingStateRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	for _, tc := range []struct {
		name         string
		prepay       bool
		nodes, index int
	}{
		{name: "settlement_local_commit", nodes: 2, index: 1},
		{name: "prepay_local_commit", prepay: true, nodes: 2, index: 0},
		{name: "intermediary_commit", nodes: 3, index: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := newPendingWriteFault(t)
			flow := newPendingStateFlow(t, tc.nodes, fault)
			t.Cleanup(fault.release)
			endpoint := flow.endpoints[tc.index]
			before := flow.channel(t, tc.index)
			fault.arm(before, tc.prepay)
			flow.perform(t, tc.prepay)
			fault.wait(t)

			after := flow.channel(t, tc.index)
			var previous dbpkg.Channel
			if err := json.Unmarshal(fault.previous, &previous); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after.SignedState.Hash(), previous.SignedState.Hash()) {
				t.Fatal("rolled-back transaction changed the signed channel state")
			}
			assertPendingData(t, after.Our.Data, before.Our.Data)
			assertPendingData(t, after.Their.Data, before.Their.Data)
			if tc.nodes == 3 {
				meta, err := endpoint.node.db.GetVirtualChannelMeta(t.Context(), flow.pub)
				if err != nil {
					t.Fatal(err)
				}
				if meta.Status != dbpkg.ConditionalStateActive || meta.LastKnownResolve != nil {
					t.Fatal("rolled-back transaction persisted intermediary metadata")
				}
				tasks, err := endpoint.node.db.DumpTasks(t.Context(), "close-conditional-")
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 0 {
					t.Fatal("rolled-back transaction persisted a close task")
				}
			} else {
				proposer, peer := flow.parties(tc.prepay)
				flow.assertPending(t, proposer, peer, flow.channel(t, peer).LoadSignedState())
			}
			fault.release()
			flow.finish(t, tc.prepay)
		})
	}
}

func TestIntegration_PendingStateRecoveryAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	for _, prepay := range []bool{false, true} {
		t.Run(pendingOperationName(prepay), func(t *testing.T) {
			flow := newPendingStateFlow(t, 2, nil)
			proposer, peer := flow.parties(prepay)
			lost := flow.loseResponse(t, peer, prepay, true)
			before := flow.channel(t, proposer)
			flow.perform(t, prepay)
			accepted := lost.wait(t)
			flow.assertPending(t, proposer, peer, accepted)
			lost.release()

			// Wait until the worker has returned from the failed query before
			// closing its database. All further acknowledgements remain lost.
			waitFor(t, 5*time.Second, 10*time.Millisecond, func() (bool, string) {
				tasks, err := flow.nodes[proposer].db.DumpTasks(t.Context(), "")
				if err != nil {
					return false, err.Error()
				}
				for _, task := range tasks {
					if strings.Contains(task.LastError, errPendingTestReply.Error()) && task.LockedTill == nil {
						return true, "proposal task is waiting to retry"
					}
				}
				return false, "waiting for the failed proposal task"
			})
			committed := flow.channel(t, proposer)
			assertPendingData(t, committed.Our.Data, before.Our.Data)
			assertPendingData(t, committed.Their.Data, before.Their.Data)

			old := flow.nodes[proposer]
			opts := flow.options[proposer]
			opts.privateKey = old.key
			old.stop(t)
			restarted := newTestNodeWithOptions(t, flow.hub, old.idx, old.port, opts)
			flow.nodes[proposer] = restarted
			flow.endpoints[proposer].node = restarted
			flow.assertPending(t, proposer, peer, accepted)
			if !bytes.Equal(flow.channel(t, proposer).SignedState.Hash(), committed.SignedState.Hash()) {
				t.Fatal("restart must load the old committed state before recovery")
			}

			// Exercise Service.Start's actual onchain recovery path, without
			// allowing an offchain response to repair the local state first.
			update := flow.replicatedClose(t, proposer, accepted)
			restarted.updates <- update
			restarted.start()
			waitFor(t, 5*time.Second, 10*time.Millisecond, func() (bool, string) {
				ch := flow.channel(t, proposer)
				return bytes.Equal(ch.SignedState.Hash(), acceptedCell(t, accepted).Hash()) &&
					ch.Our.LatestProcessedLT == update.Transaction.LT, "waiting for onchain pending-state recovery"
			})
			flow.assertPendingDataFromPeer(t, proposer, peer)
			flow.assertReceiverBalance(t, prepay)
		})
	}
}

type pendingEndpoint struct {
	node    *testNode
	address string
	before  *big.Int
	fee     *big.Int
}

type pendingStateFlow struct {
	hub       *loopbackHub
	nodes     []*testNode
	options   []testNodeOptions
	endpoints []pendingEndpoint
	key       ed25519.PrivateKey
	pub       ed25519.PublicKey
	coin      *payments.CoinConfig
}

func newPendingStateFlow(t *testing.T, count int, fault *pendingWriteFault) *pendingStateFlow {
	t.Helper()
	f := &pendingStateFlow{hub: newLoopbackHub(), nodes: make([]*testNode, count), options: make([]testNodeOptions, count)}
	for i := range f.nodes {
		opts := testNodeOptions{acceptingDerivatives: true, dbPath: filepath.Join(t.TempDir(), "db")}
		if fault != nil {
			opts.wrapStorage = func(storage dbpkg.Storage) dbpkg.Storage {
				return &pendingFaultStorage{Storage: storage, fault: fault}
			}
		}
		f.options[i] = opts
		f.nodes[i] = newTestNodeWithOptions(t, f.hub, i+1, 24000+i, opts)
		f.nodes[i].start()
		t.Cleanup(func() { f.nodes[i].stop(t) })
	}
	var err error
	f.coin, err = f.nodes[0].svc.ResolveCoinConfigBySymbol("TON")
	if err != nil {
		t.Fatal(err)
	}
	var chain []transport.TunnelChainPart
	for i := 0; i < count-1; i++ {
		a, b := openAndSeedChannel(t, f.nodes[i], f.nodes[i+1], tlb.MustFromTON("5").Nano())
		fee := big.NewInt(int64(count-1-i) * 10_000_000)
		for j, ch := range []*dbpkg.Channel{a, b} {
			node := f.nodes[i+j]
			f.endpoints = append(f.endpoints, pendingEndpoint{node: node, address: ch.Our.Address,
				before: pendingBalance(t, node, ch.Our.Address, f.coin), fee: fee})
		}
		chain = append(chain, transport.TunnelChainPart{Target: f.nodes[i+1].pub,
			Capacity: tlb.MustFromTON("1").Nano(), Fee: fee,
			Deadline: time.Now().Add(time.Duration(count-1-i) * 24 * time.Hour)})
	}
	f.pub, f.key, err = ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ik, instructions, err := transport.GenerateTunnel(f.key, chain, 5, false, f.nodes[0].key, f.coin)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.nodes[0].svc.CreateSendConditional(t.Context(), ik, f.key, chain[0], chain[len(chain)-1], instructions, f.coin); err != nil {
		t.Fatal(err)
	}
	for i, node := range f.nodes {
		waitVirtualMeta(t, node, f.pub, i > 0, i < count-1)
	}
	return f
}

func (f *pendingStateFlow) parties(prepay bool) (proposer, peer int) {
	if prepay {
		return 0, 1
	}
	return len(f.endpoints) - 1, len(f.endpoints) - 2
}

func (f *pendingStateFlow) channel(t *testing.T, index int) *dbpkg.Channel {
	t.Helper()
	ep := f.endpoints[index]
	ch, err := ep.node.svc.GetChannel(t.Context(), ep.address)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func (f *pendingStateFlow) perform(t *testing.T, prepay bool) {
	t.Helper()
	amount := "1"
	proposer, _ := f.parties(prepay)
	if prepay {
		amount = "0.4"
	}
	state := conditionals.VirtualChannelState{Amount: tlb.MustFromTON(amount).Nano()}
	state.Sign(f.key)
	resolve, err := state.ToCell()
	if err != nil {
		t.Fatal(err)
	}
	node := f.endpoints[proposer].node
	if err := node.svc.AddConditionalResolve(t.Context(), f.pub, resolve); err != nil {
		t.Fatal(err)
	}
	if prepay {
		err = node.svc.CommitVirtualChannel(t.Context(), f.pub)
	} else {
		err = node.svc.CloseConditional(t.Context(), f.pub)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (f *pendingStateFlow) finish(t *testing.T, prepay bool) {
	t.Helper()
	if prepay {
		want := new(big.Int).Add(tlb.MustFromTON("0.4").Nano(), f.endpoints[0].fee)
		waitFor(t, 20*time.Second, 20*time.Millisecond, func() (bool, string) {
			for i, ep := range f.endpoints {
				ch := f.channel(t, i)
				meta, err := ep.node.db.GetVirtualChannelMeta(t.Context(), f.pub)
				if err != nil {
					return false, err.Error()
				}
				dict, id := ch.Our.Data.Conditionals, meta.Outgoing
				if i%2 == 1 {
					dict, id = ch.Their.Data.Conditionals, meta.Incoming
				}
				_, cond, err := payments.FindConditional(t.Context(), dict, id.Conditional.Hash(), ep.node.svc)
				if err != nil || cond.(*conditionals.ConditionalVirtualChannel).Prepay.Cmp(want) != 0 {
					return false, "waiting for prepay on both peers"
				}
			}
			return true, "prepay committed once"
		})
		f.assertReceiverBalance(t, true)
		f.perform(t, false)
	}
	waitFor(t, 30*time.Second, 20*time.Millisecond, func() (bool, string) {
		for i, ep := range f.endpoints {
			ch := f.channel(t, i)
			meta, err := ep.node.db.GetVirtualChannelMeta(t.Context(), f.pub)
			if err != nil {
				return false, err.Error()
			}
			if !ch.Our.Data.Conditionals.IsEmpty() || !ch.Their.Data.Conditionals.IsEmpty() || meta.Status != dbpkg.ConditionalStateClosed {
				return false, "waiting for conditional cleanup"
			}
			want := new(big.Int).Add(tlb.MustFromTON("1").Nano(), ep.fee)
			if i%2 == 0 {
				want.Neg(want)
			}
			got := new(big.Int).Sub(pendingBalance(t, ep.node, ep.address, f.coin), ep.before)
			if got.Cmp(want) != 0 {
				return false, fmt.Sprintf("endpoint %d balance delta = %s, want %s", i, got, want)
			}
		}
		return true, "balances and cleanup confirmed on every peer"
	})
}

func (f *pendingStateFlow) assertPending(t *testing.T, index, peer int, state *payments.StateBodySigned) {
	t.Helper()
	ep := f.endpoints[index]
	pending, err := ep.node.db.GetChannelPendingState(t.Context(), f.channel(t, index), state.Body)
	if err != nil {
		t.Fatalf("pending state cannot be restored: %v", err)
	}
	if pending.Seqno != state.Body.Seqno {
		t.Fatalf("pending seqno = %d, want %d", pending.Seqno, state.Body.Seqno)
	}
	remote := f.channel(t, peer)
	assertPendingData(t, pending.OurData, remote.Their.Data)
	assertPendingData(t, pending.TheirData, remote.Our.Data)
}

func (f *pendingStateFlow) assertPendingDataFromPeer(t *testing.T, index, peer int) {
	t.Helper()
	local, remote := f.channel(t, index), f.channel(t, peer)
	assertPendingData(t, local.Our.Data, remote.Their.Data)
	assertPendingData(t, local.Their.Data, remote.Our.Data)
}

func (f *pendingStateFlow) assertReceiverBalance(t *testing.T, prepay bool) {
	t.Helper()
	ep := f.endpoints[len(f.endpoints)-1]
	amount := "1"
	if prepay {
		amount = "0.4"
	}
	want := new(big.Int).Add(tlb.MustFromTON(amount).Nano(), ep.fee)
	got := new(big.Int).Sub(pendingBalance(t, ep.node, ep.address, f.coin), ep.before)
	if got.Cmp(want) != 0 {
		t.Fatalf("receiver balance delta = %s, want %s", got, want)
	}
}

func pendingBalance(t *testing.T, node *testNode, addr string, coin *payments.CoinConfig) *big.Int {
	t.Helper()
	ch, err := node.svc.GetChannel(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	balances, err := ch.CalcBalance(t.Context(), false, node.svc)
	if err != nil {
		t.Fatal(err)
	}
	return new(big.Int).Set(balances[coin.BalanceID].Available())
}

func assertPendingData(t *testing.T, got, want dbpkg.AgreedData) {
	t.Helper()
	gotCell := cell.BeginCell().MustStoreDict(got.Conditionals).MustStoreDict(got.ActionStates).EndCell()
	wantCell := cell.BeginCell().MustStoreDict(want.Conditionals).MustStoreDict(want.ActionStates).EndCell()
	if !bytes.Equal(gotCell.Hash(), wantCell.Hash()) {
		t.Fatal("pending-state dictionaries differ from the accepted state")
	}
}

func pendingOperationName(prepay bool) string {
	if prepay {
		return "prepay"
	}
	return "settlement"
}

func acceptedCell(t *testing.T, state *payments.StateBodySigned) *cell.Cell {
	t.Helper()
	c, err := tlb.ToCell(state)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type pendingLostResponse struct {
	accepted chan *payments.StateBodySigned
	resume   chan struct{}
	once     sync.Once
}

func (f *pendingStateFlow) loseResponse(t *testing.T, peer int, prepay, every bool) *pendingLostResponse {
	t.Helper()
	lost := &pendingLostResponse{accepted: make(chan *payments.StateBodySigned, 1), resume: make(chan struct{})}
	var fired atomic.Bool
	net := f.hub.get(f.endpoints[peer].node.pub)
	net.mu.Lock()
	net.afterQuery = func(msg, response any) error {
		q, ok := msg.(transport.ProposeAction)
		if !ok {
			return nil
		}
		_, isPrepay := q.Action.(transport.CommitVirtualAction)
		_, isSettlement := q.Action.(transport.ExecuteConditionalAction)
		decision, ok := response.(transport.ProposalDecision)
		if !ok || !decision.Agreed || (prepay && !isPrepay) || (!prepay && !isSettlement) {
			return nil
		}
		if fired.CompareAndSwap(false, true) {
			var state payments.StateBodySigned
			if err := tlb.Parse(&state, decision.SignedState); err != nil {
				return err
			}
			lost.accepted <- &state
			<-lost.resume
			return errPendingTestReply
		}
		if every {
			return errPendingTestReply
		}
		return nil
	}
	net.mu.Unlock()
	t.Cleanup(lost.release)
	return lost
}

func (l *pendingLostResponse) wait(t *testing.T) *payments.StateBodySigned {
	t.Helper()
	select {
	case state := <-l.accepted:
		return state
	case <-time.After(10 * time.Second):
		t.Fatal("proposal was not accepted by the peer")
		return nil
	}
}

func (l *pendingLostResponse) release() { l.once.Do(func() { close(l.resume) }) }

type pendingWriteFault struct {
	armed      atomic.Bool
	key        []byte
	initial    []byte
	previous   []byte
	prepay     bool
	rolledBack chan struct{}
	resume     chan struct{}
	once       sync.Once
}

func newPendingWriteFault(t *testing.T) *pendingWriteFault {
	t.Helper()
	f := &pendingWriteFault{rolledBack: make(chan struct{}, 1), resume: make(chan struct{})}
	return f
}

func (f *pendingWriteFault) arm(channel *dbpkg.Channel, prepay bool) {
	f.key = []byte("ch:" + channel.Our.Address)
	f.prepay = prepay
	if prepay {
		f.initial = channel.Our.Data.Conditionals.AsCell().Hash()
	}
	f.armed.Store(true)
}

func (f *pendingWriteFault) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.rolledBack:
	case <-time.After(10 * time.Second):
		t.Fatal("transaction failure was not injected")
	}
}

func (f *pendingWriteFault) release() { f.once.Do(func() { close(f.resume) }) }

type pendingFaultStorage struct {
	dbpkg.Storage
	fault *pendingWriteFault
}

type pendingFaultExecutor struct {
	dbpkg.Executor
	fault *pendingWriteFault
}

type pendingFaultTxKey struct{}

func (s *pendingFaultStorage) Transaction(ctx context.Context, fn func(context.Context) error) error {
	nested, _ := ctx.Value(pendingFaultTxKey{}).(bool)
	err := s.Storage.Transaction(ctx, func(ctx context.Context) error {
		return fn(context.WithValue(ctx, pendingFaultTxKey{}, true))
	})
	if !nested && errors.Is(err, errPendingTestWrite) {
		s.fault.rolledBack <- struct{}{}
		<-s.fault.resume
	}
	return err
}

func (s *pendingFaultStorage) GetExecutor(ctx context.Context) dbpkg.Executor {
	return &pendingFaultExecutor{Executor: s.Storage.GetExecutor(ctx), fault: s.fault}
}

func (e *pendingFaultExecutor) Put(key, value []byte) error {
	f := e.fault
	if f.armed.Load() && bytes.Equal(key, f.key) {
		var channel dbpkg.Channel
		if err := json.Unmarshal(value, &channel); err != nil {
			return err
		}
		matches := channel.Our.Data.Conditionals.IsEmpty() && channel.Their.Data.Conditionals.IsEmpty()
		if f.prepay {
			matches = !channel.Our.Data.Conditionals.IsEmpty() &&
				!bytes.Equal(channel.Our.Data.Conditionals.AsCell().Hash(), f.initial)
		}
		if matches && f.armed.CompareAndSwap(true, false) {
			previous, err := e.Executor.Get(key)
			if err != nil {
				return err
			}
			f.previous = append([]byte(nil), previous...)
			return errPendingTestWrite
		}
	}
	return e.Executor.Put(key, value)
}

func (f *pendingStateFlow) replicatedClose(t *testing.T, index int, state *payments.StateBodySigned) *tonpayments.ChannelUpdatedEvent {
	t.Helper()
	chain := f.hub.chain
	chain.mu.Lock()
	defer chain.mu.Unlock()
	target := chain.channelStates[f.endpoints[index].address]
	target.storage.Quarantine = &payments.QuarantinedState{
		Seqno: state.Body.Seqno, TheirState: stateSideForContract(state, target.storage.IsA),
		QuarantineStarts: uint32(time.Now().Unix()), ActionsToExecuteHash: make([]byte, 32),
	}
	account, err := target.rebuildAccount()
	if err != nil {
		t.Fatal(err)
	}
	chain.accounts[target.addr.String()] = account
	body, err := tlb.ToCell(payments.UncoopCloseReplicateMsg{At: uint32(time.Now().Unix()), State: state})
	if err != nil {
		t.Fatal(err)
	}
	tx := chain.nextTransactionLocked(target.addr, tlb.MsgTypeInternal, body, nil)
	tx.In.From = target.pairAddr.String()
	update, err := chain.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		t.Fatal(err)
	}
	return update
}
