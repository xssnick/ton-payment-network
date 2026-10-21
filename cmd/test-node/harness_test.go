package testnode

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments"
	chainscan "github.com/xssnick/ton-payment-network/tonpayments/chain"
	chainclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	cfgpkg "github.com/xssnick/ton-payment-network/tonpayments/config"
	dbpkg "github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/hedgeauth"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	tonwallet "github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type walletAddresser interface {
	WalletAddress() *address.Address
}

type stopper interface {
	Stop()
}

type testNode struct {
	idx         int
	port        int
	key         ed25519.PrivateKey
	pub         ed25519.PublicKey
	transport   *transport.Transport
	chain       *testChain
	svc         *tonpayments.Service
	derivatives *tonpayments.DerivativesService
	db          *dbpkg.DB
	updates     chan any
	wallet      walletAddresser
	walletRaw   *tonwallet.Wallet
	scanner     stopper
	done        chan struct{}
}

func newTestNode(t *testing.T, hub *loopbackHub, idx, port int) *testNode {
	return newTestNodeWithOptions(t, hub, idx, port, testNodeOptions{acceptingDerivatives: true})
}

type testNodeOptions struct {
	acceptingDerivatives bool
	hedgeWebhookURL      string
	hedgeWebhookKey      string
	hedgeWebhookSecret   string
	privateKey           ed25519.PrivateKey
	dbPath               string
	wrapStorage          func(dbpkg.Storage) dbpkg.Storage
}

func newTestNodeWithOptions(t *testing.T, hub *loopbackHub, idx, port int, opts testNodeOptions) *testNode {
	t.Helper()

	cfg, err := cfgpkg.Generate()
	if err != nil {
		t.Fatalf("node %d: generate config failed: %v", idx, err)
	}
	cfg.ChannelConfig.BufferTimeToCommit = 1
	cfg.ChannelConfig.QuarantineDurationSec = 2
	cfg.ChannelConfig.ActionsDuration = 8
	cfg.ChannelConfig.ConditionalCloseDurationSec = 8
	cfg.ChannelConfig.MinSafeVirtualChannelTimeoutSec = 1
	cfg.ChannelConfig.AcceptingDerivatives = opts.acceptingDerivatives
	cfg.ChannelConfig.DerivativesHedge.WebhookURL = opts.hedgeWebhookURL
	if opts.hedgeWebhookKey != "" {
		cfg.ChannelConfig.DerivativesHedge.WebhookKey = opts.hedgeWebhookKey
	}
	if opts.hedgeWebhookSecret != "" {
		cfg.ChannelConfig.DerivativesHedge.WebhookSignatureHMACSHA256Key = opts.hedgeWebhookSecret
	}
	cfg.ChannelConfig.SupportedCoins.Ton.BalanceControl = nil
	for key, jetton := range cfg.ChannelConfig.SupportedCoins.Jettons {
		jetton.BalanceControl = nil
		cfg.ChannelConfig.SupportedCoins.Jettons[key] = jetton
	}
	for key, ec := range cfg.ChannelConfig.SupportedCoins.ExtraCurrencies {
		ec.BalanceControl = nil
		cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[key] = ec
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("node %d: generate node key failed: %v", idx, err)
	}
	if opts.privateKey != nil {
		priv = opts.privateKey
		pub = priv.Public().(ed25519.PublicKey)
	}

	dbPath := opts.dbPath
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), fmt.Sprintf("node-%d-db", idx))
	}
	storage, fresh, err := leveldb.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("node %d: open db failed: %v", idx, err)
	}

	var nodeStorage dbpkg.Storage = storage
	if opts.wrapStorage != nil {
		nodeStorage = opts.wrapStorage(storage)
	}
	database := dbpkg.NewDB(nodeStorage, pub)
	if fresh {
		if err = database.SetMigrationVersion(context.Background(), len(dbpkg.Migrations)); err != nil {
			t.Fatalf("node %d: set migration version failed: %v", idx, err)
		}
	} else {
		if err = dbpkg.RunMigrations(database); err != nil {
			t.Fatalf("node %d: run migrations failed: %v", idx, err)
		}
	}

	net := newLoopbackNet(hub, pub, port)
	tr := transport.NewTransport(priv, net, false)

	wallet := newStubWallet(pub, hub.chain)
	updates := make(chan any, 64)

	svc, err := tonpayments.NewService(hub.chain, database, tr, nil, wallet, updates, priv, cfg.ChannelConfig, cfg.Vault, false)
	if err != nil {
		t.Fatalf("node %d: init service failed: %v", idx, err)
	}
	tr.SetService(svc)
	hub.chain.setResolver(svc)
	hub.chain.registerUpdates(updates)
	hub.chain.registerWallet(wallet.WalletAddress())

	return &testNode{
		idx:         idx,
		port:        port,
		key:         priv,
		pub:         pub,
		transport:   tr,
		chain:       hub.chain,
		svc:         svc,
		derivatives: tonpayments.NewDerivativesService(svc),
		db:          database,
		updates:     updates,
		wallet:      wallet,
		done:        make(chan struct{}),
	}
}

func (n *testNode) start() {
	go func() {
		defer close(n.done)
		n.svc.Start()
	}()
}

func (n *testNode) stop(t *testing.T) {
	t.Helper()

	n.svc.Stop()
	if n.scanner != nil {
		n.scanner.Stop()
	}
	n.transport.Stop()

	select {
	case <-n.done:
	case <-time.After(4 * time.Second):
		t.Logf("node %d stop timeout; continuing cleanup", n.idx)
	}

	n.db.Close()
}

type flowRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *flowRecorder) add(entry string) {
	if r == nil || entry == "" {
		return
	}
	r.mu.Lock()
	r.entries = append(r.entries, entry)
	r.mu.Unlock()
}

func (r *flowRecorder) snapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.entries...)
}

type flowRecordingChain struct {
	inner tonpayments.ChainAPI
	flow  *flowRecorder
}

func (c *flowRecordingChain) GetAccount(ctx context.Context, addr *address.Address, blockAfter time.Time) (*chainclient.Account, error) {
	return c.inner.GetAccount(ctx, addr, blockAfter)
}

func (c *flowRecordingChain) GetJettonWalletAddress(ctx context.Context, root, addr *address.Address) (*address.Address, error) {
	return c.inner.GetJettonWalletAddress(ctx, root, addr)
}

func (c *flowRecordingChain) GetJettonBalance(ctx context.Context, root, addr *address.Address, blockAfter time.Time) (*big.Int, error) {
	return c.inner.GetJettonBalance(ctx, root, addr, blockAfter)
}

func (c *flowRecordingChain) GetLastTransaction(ctx context.Context, addr *address.Address, blockAfter time.Time) (*chainclient.Transaction, *chainclient.Account, error) {
	return c.inner.GetLastTransaction(ctx, addr, blockAfter)
}

func (c *flowRecordingChain) GetTransactionByInMsgHash(ctx context.Context, addr *address.Address, msgHash []byte, after time.Time) (*chainclient.Transaction, error) {
	return c.inner.GetTransactionByInMsgHash(ctx, addr, msgHash, after)
}

func classifyExternalFlow(body *cell.Cell) string {
	if body == nil {
		return "external"
	}

	var uncoop payments.UncoopCloseMsg
	if err := tlb.LoadFromCell(&uncoop, body.MustBeginParse()); err == nil {
		return "uncoop-start"
	}

	var settle payments.SettleMsg
	if err := tlb.LoadFromCell(&settle, body.MustBeginParse()); err == nil {
		return "settle"
	}

	var fin payments.FinalizeSettleMsg
	if err := tlb.LoadFromCell(&fin, body.MustBeginParse()); err == nil {
		return "finalize-settle"
	}

	var exec payments.ProxyExecuteActionsMsg
	if err := tlb.LoadFromCell(&exec, body.MustBeginParse()); err == nil {
		return "execute-action"
	}

	var finish payments.FinishUncooperativeClose
	if err := tlb.LoadFromCell(&finish, body.MustBeginParse()); err == nil {
		return "finish-close"
	}

	return "external"
}

func (c *flowRecordingChain) SendWaitExternalMessage(ctx context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	hash, err := c.inner.SendWaitExternalMessage(ctx, to, body)
	if err == nil && c.flow != nil && to != nil {
		c.flow.add(fmt.Sprintf("%s:%s:%s", classifyExternalFlow(body), to.String(), base64.StdEncoding.EncodeToString(hash)))
	}
	return hash, err
}

type recordingWallet struct {
	inner tonpayments.Wallet
	flow  *flowRecorder
}

type mnemonicWallet struct {
	wallet *tonwallet.Wallet
}

func (w *mnemonicWallet) WalletAddress() *address.Address {
	return w.wallet.WalletAddress()
}

func (w *mnemonicWallet) DoTransactionMany(ctx context.Context, _ string, messages []tonpayments.WalletMessage) ([]byte, error) {
	list := make([]*tonwallet.Message, 0, len(messages))
	for _, message := range messages {
		target := message.To
		if message.StateInit != nil {
			stateCell, err := tlb.ToCell(message.StateInit)
			if err != nil {
				return nil, err
			}
			target = address.NewAddress(0, 0, stateCell.Hash())
		}

		msg := tonwallet.SimpleMessage(target, message.Amount, message.Body)
		if message.EC != nil {
			msg.InternalMessage.ExtraCurrencies = cell.NewDict(32)
			for u, coins := range message.EC {
				_ = msg.InternalMessage.ExtraCurrencies.SetIntKey(big.NewInt(int64(u)), cell.BeginCell().MustStoreBigVarUInt(coins.Nano(), 32).EndCell())
			}
		}
		if message.StateInit != nil {
			msg.InternalMessage.Bounce = false
			msg.InternalMessage.StateInit = message.StateInit
		}

		list = append(list, msg)
	}

	hash, err := w.wallet.SendManyWaitTxHash(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("failed to send tx: %w", err)
	}
	return hash, nil
}

func (w *mnemonicWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}

func (w *recordingWallet) WalletAddress() *address.Address {
	return w.inner.WalletAddress()
}

func classifyWalletReason(reason string) string {
	switch {
	case strings.Contains(reason, "Deploy derivative resolver"):
		return "deploy-resolver"
	case strings.Contains(reason, "Commit derivative resolver"):
		return "commit-resolver"
	case strings.Contains(reason, "Proxy derivative settle"):
		return "proxy-settle"
	case strings.Contains(reason, "Channel balance top up"):
		return "topup"
	default:
		return "wallet-tx"
	}
}

func walletTargets(messages []tonpayments.WalletMessage) []string {
	targets := make([]string, 0, len(messages))
	for _, message := range messages {
		target := message.To
		if message.StateInit != nil {
			stateCell, err := tlb.ToCell(message.StateInit)
			if err == nil {
				target = address.NewAddress(0, 0, stateCell.Hash())
			}
		}
		if target != nil {
			targets = append(targets, target.String())
		}
	}
	return targets
}

func (w *recordingWallet) DoTransactionMany(ctx context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	hash, err := w.inner.DoTransactionMany(ctx, reason, messages)
	if err == nil && w.flow != nil {
		targets := walletTargets(messages)
		kind := classifyWalletReason(reason)
		if len(targets) == 0 {
			w.flow.add(fmt.Sprintf("%s::%s", kind, base64.StdEncoding.EncodeToString(hash)))
		} else {
			for _, target := range targets {
				w.flow.add(fmt.Sprintf("%s:%s:%s", kind, target, base64.StdEncoding.EncodeToString(hash)))
			}
		}
	}
	return hash, err
}

func (w *recordingWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}

type liveTestnetEnv struct {
	api    ton.APIClientWrapped
	chain  *flowRecordingChain
	wallet tonpayments.Wallet
	raw    *tonwallet.Wallet
	flow   *flowRecorder
}

func walletPrivateKeyFromEnv() (ed25519.PrivateKey, error) {
	seed := strings.Fields(strings.TrimSpace(os.Getenv("WALLET_SEED")))
	if len(seed) < 12 {
		return nil, fmt.Errorf("WALLET_SEED is required")
	}

	key, err := tonwallet.SeedToPrivateKeyWithOptions(seed)
	if err == nil {
		return key, nil
	}

	return tonwallet.SeedToPrivateKeyWithOptions(seed, tonwallet.WithBIP39(true))
}

func newTestnetLoopbackHub(t *testing.T) *loopbackHub {
	t.Helper()

	seed := strings.Fields(strings.TrimSpace(os.Getenv("WALLET_SEED")))
	if len(seed) < 12 {
		t.Skip("testnet two-node mode requires WALLET_SEED")
	}

	client := liteclient.NewConnectionPool()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var err error
	if err = client.AddConnection(ctx, "109.236.80.69:49913", "AxFZRHVD1qIO9Fyva52P4vC3tRvk8ac1KKOG0c6IVio="); err != nil {
		t.Fatalf("connect to testnet lite server failed: %v", err)
	}

	api := ton.NewAPIClient(client).WithRetry().WithTimeout(10 * time.Second)
	rawWallet, err := tonwallet.FromSeed(api, seed, tonwallet.HighloadV2R2)
	if err != nil {
		rawWallet, err = tonwallet.FromSeed(api, seed, tonwallet.HighloadV2R2, true)
		if err != nil {
			t.Fatalf("init testnet wallet failed: %v", err)
		}
	}
	baseWallet := &mnemonicWallet{wallet: rawWallet}
	acc, err := chainclient.NewTON(api).GetAccount(context.Background(), baseWallet.WalletAddress(), time.Time{})
	if err != nil {
		t.Fatalf("fetch testnet wallet state failed: %v", err)
	}
	if acc == nil || !acc.HasState {
		t.Fatalf("testnet wallet %s is not funded or not deployed; get testnet TON via @testgiver_ton_bot first", baseWallet.WalletAddress().String())
	}

	flow := &flowRecorder{}
	return &loopbackHub{
		byPubKey: map[string]*loopbackNet{},
		live: &liveTestnetEnv{
			api:  api,
			flow: flow,
			chain: &flowRecordingChain{
				inner: chainclient.NewTON(api),
				flow:  flow,
			},
			wallet: &recordingWallet{
				inner: baseWallet,
				flow:  flow,
			},
			raw: rawWallet,
		},
	}
}

func newTestnetNode(t *testing.T, hub *loopbackHub, idx, port int) *testNode {
	return newTestnetNodeWithOptions(t, hub, idx, port, testNodeOptions{acceptingDerivatives: true})
}

func newTestnetNodeWithOptions(t *testing.T, hub *loopbackHub, idx, port int, opts testNodeOptions) *testNode {
	t.Helper()

	if hub.live == nil {
		t.Fatalf("node %d: testnet hub is not initialized", idx)
	}

	cfg, err := cfgpkg.Generate()
	if err != nil {
		t.Fatalf("node %d: generate config failed: %v", idx, err)
	}
	cfg.ChannelConfig.BufferTimeToCommit = 1
	cfg.ChannelConfig.QuarantineDurationSec = 15
	cfg.ChannelConfig.ActionsDuration = 35
	cfg.ChannelConfig.ConditionalCloseDurationSec = 35
	cfg.ChannelConfig.MinSafeVirtualChannelTimeoutSec = 1
	cfg.ChannelConfig.ReplicationMessageAttachAmount = "0.047"
	cfg.ChannelConfig.AcceptingDerivatives = opts.acceptingDerivatives
	cfg.ChannelConfig.DerivativesHedge.WebhookURL = opts.hedgeWebhookURL
	if opts.hedgeWebhookKey != "" {
		cfg.ChannelConfig.DerivativesHedge.WebhookKey = opts.hedgeWebhookKey
	}
	if opts.hedgeWebhookSecret != "" {
		cfg.ChannelConfig.DerivativesHedge.WebhookSignatureHMACSHA256Key = opts.hedgeWebhookSecret
	}
	cfg.ChannelConfig.SupportedCoins.Ton.BalanceControl = nil
	for key, jetton := range cfg.ChannelConfig.SupportedCoins.Jettons {
		jetton.BalanceControl = nil
		cfg.ChannelConfig.SupportedCoins.Jettons[key] = jetton
	}
	for key, ec := range cfg.ChannelConfig.SupportedCoins.ExtraCurrencies {
		ec.BalanceControl = nil
		cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[key] = ec
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("node %d: generate node key failed: %v", idx, err)
	}

	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("node-%d-db", idx))
	storage, fresh, err := leveldb.NewLevelDB(dbPath)
	if err != nil {
		t.Fatalf("node %d: open db failed: %v", idx, err)
	}

	database := dbpkg.NewDB(storage, pub)
	if fresh {
		if err = database.SetMigrationVersion(context.Background(), len(dbpkg.Migrations)); err != nil {
			t.Fatalf("node %d: set migration version failed: %v", idx, err)
		}
	} else {
		if err = dbpkg.RunMigrations(database); err != nil {
			t.Fatalf("node %d: run migrations failed: %v", idx, err)
		}
	}

	net := newLoopbackNet(hub, pub, port)
	tr := transport.NewTransport(priv, net, false)
	updates := make(chan any, 256)
	scanner := chainscan.NewScanner(hub.live.api, 0, zerolog.Nop(), updates)
	database.SetOnChannelUpdated(scanner.OnChannelUpdate)

	svc, err := tonpayments.NewService(hub.live.chain, database, tr, nil, hub.live.wallet, updates, priv, cfg.ChannelConfig, cfg.Vault, false)
	if err != nil {
		t.Fatalf("node %d: init service failed: %v", idx, err)
	}
	tr.SetService(svc)

	return &testNode{
		idx:         idx,
		port:        port,
		key:         priv,
		pub:         pub,
		transport:   tr,
		svc:         svc,
		derivatives: tonpayments.NewDerivativesService(svc),
		db:          database,
		updates:     updates,
		wallet:      hub.live.wallet,
		walletRaw:   hub.live.raw,
		scanner:     scanner,
		done:        make(chan struct{}),
	}
}

type hedgeWebhookEvent struct {
	Event         string `json:"event"`
	OrderID       string `json:"order_id"`
	LinkedOrderID string `json:"linked_order_id"`
	Status        string `json:"status"`
}

const testHedgeMaxRequestBodyBytes = 16 * 1024

type testHedgeServer struct {
	t *testing.T

	server    *httptest.Server
	key       string
	secret    []byte
	rawSecret string

	mu                      sync.Mutex
	rejectOpen              bool
	delay                   time.Duration
	tamperResponseSignature bool
	hedged                  int
	closed                  int
	events                  []hedgeWebhookEvent
}

func newTestHedgeServer(t *testing.T) *testHedgeServer {
	t.Helper()

	key, rawSecret, secret := newTestHedgeAuth(t)
	srv := &testHedgeServer{
		t:         t,
		key:       key,
		rawSecret: rawSecret,
		secret:    secret,
	}
	srv.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}

		srv.mu.Lock()
		delay := srv.delay
		rejectOpen := srv.rejectOpen
		tamperResponseSignature := srv.tamperResponseSignature
		srv.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}

		defer r.Body.Close()

		body, err := io.ReadAll(io.LimitReader(r.Body, testHedgeMaxRequestBodyBytes+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}
		if len(body) > testHedgeMaxRequestBodyBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}

		target := r.URL.EscapedPath()
		if target == "" {
			target = "/"
		}
		meta, _, err := hedgeauth.VerifyRequest(
			r.Header,
			r.Method,
			hedgeauth.CanonicalTarget(target, r.URL.RawQuery),
			body,
			srv.key,
			srv.secret,
			time.Now(),
			hedgeauth.DefaultMaxClockSkew,
		)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}

		var event hedgeWebhookEvent
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&event); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}

		status := http.StatusOK
		respBody := []byte(`{"success":true}`)
		if rejectOpen && event.Event == "open" {
			status = http.StatusConflict
			respBody = []byte(`{"success":false}`)
		}

		srv.mu.Lock()
		srv.events = append(srv.events, event)
		if status == http.StatusOK {
			switch event.Event {
			case "open":
				srv.hedged++
			case "close":
				srv.closed++
			}
		}
		srv.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if err = hedgeauth.ApplySignedResponseHeaders(w.Header(), meta, status, respBody, srv.key, srv.secret, time.Now()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		}
		if tamperResponseSignature {
			w.Header().Set(hedgeauth.HeaderSignature, "tampered")
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))

	t.Cleanup(func() {
		hedged, closed := srv.counts()
		t.Logf("[hedge-server] hedged=%d closed=%d", hedged, closed)
		srv.server.Close()
	})

	return srv
}

func (s *testHedgeServer) url() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.URL
}

func (s *testHedgeServer) auth() (string, string) {
	if s == nil {
		return "", ""
	}
	return s.key, s.rawSecret
}

func (s *testHedgeServer) setRejectOpen(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rejectOpen = v
	s.mu.Unlock()
}

func (s *testHedgeServer) setDelay(delay time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.delay = delay
	s.mu.Unlock()
}

func (s *testHedgeServer) setTamperResponseSignature(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tamperResponseSignature = v
	s.mu.Unlock()
}

func (s *testHedgeServer) counts() (int, int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hedged, s.closed
}

func (s *testHedgeServer) snapshot() []hedgeWebhookEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hedgeWebhookEvent, len(s.events))
	copy(out, s.events)
	return out
}

func newTestHedgeAuth(t *testing.T) (string, string, []byte) {
	t.Helper()

	keyRaw := make([]byte, 12)
	if _, err := rand.Read(keyRaw); err != nil {
		t.Fatalf("generate hedge key failed: %v", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate hedge secret failed: %v", err)
	}
	return base64.StdEncoding.EncodeToString(keyRaw), base64.StdEncoding.EncodeToString(secret), secret
}

func waitHedgeCounts(t *testing.T, srv *testHedgeServer, hedged, closed int) {
	t.Helper()

	waitFor(t, 30*time.Second, 200*time.Millisecond, func() (bool, string) {
		gotHedged, gotClosed := srv.counts()
		if gotHedged == hedged && gotClosed == closed {
			return true, fmt.Sprintf("hedge server counts hedged=%d closed=%d", gotHedged, gotClosed)
		}
		return false, fmt.Sprintf("waiting hedge counts hedged=%d/%d closed=%d/%d", gotHedged, hedged, gotClosed, closed)
	})
}

func waitChannelByPeer(t *testing.T, node *testNode, peerKey ed25519.PublicKey) *dbpkg.Channel {
	t.Helper()

	var out *dbpkg.Channel
	waitFor(t, 20*time.Second, 250*time.Millisecond, func() (bool, string) {
		list, err := node.svc.ListChannels(context.Background(), nil, dbpkg.ChannelStateAny)
		if err != nil {
			return false, fmt.Sprintf("node %d list channels failed: %v", node.idx, err)
		}

		for _, ch := range list {
			if bytes.Equal(ch.Their.OnchainInfo.Key, peerKey) {
				out = ch
				return true, fmt.Sprintf("node %d channel with peer found: %s", node.idx, ch.Our.Address)
			}
		}
		return false, fmt.Sprintf("node %d has no channel with target peer yet", node.idx)
	})
	return out
}

func waitActiveChannelReady(t *testing.T, node *testNode, channelAddr string) {
	t.Helper()

	waitFor(t, 20*time.Second, 250*time.Millisecond, func() (bool, string) {
		_, err := node.svc.GetActiveChannel(context.Background(), channelAddr)
		if err != nil {
			return false, fmt.Sprintf("node %d waiting active channel %s: %v", node.idx, channelAddr, err)
		}
		return true, fmt.Sprintf("node %d active channel %s is ready", node.idx, channelAddr)
	})
}

func openDerivativeForTest(ctx context.Context, node *testNode, channelAddr string, isLong bool, leverage int, amountDecimal string) (string, error) {
	side := "short"
	if isLong {
		side = "long"
	}

	id, err := node.derivatives.OpenPosition(ctx, channelAddr, "BTC", side, leverage, amountDecimal, "market", "")
	if err == nil {
		return id, nil
	}
	if !strings.Contains(err.Error(), "no price resolver") {
		return "", err
	}
	return node.derivatives.OpenPosition(ctx, channelAddr, "BTCUSDT", side, leverage, amountDecimal, "market", "")
}

func seedChannelTONBalance(t *testing.T, node *testNode, channelAddr string, amount *big.Int) {
	t.Helper()

	deadline := time.Now().Add(6 * time.Second)
	tonBalanceID := payments.GetTONBalanceID()

	for {
		ch, err := node.db.GetChannel(context.Background(), channelAddr)
		if err != nil {
			t.Fatalf("node %d get channel %s failed: %v", node.idx, channelAddr, err)
		}

		if ch.Our.OnchainBalances == nil {
			ch.Our.OnchainBalances = map[string]*big.Int{}
		}
		if ch.Their.OnchainBalances == nil {
			ch.Their.OnchainBalances = map[string]*big.Int{}
		}

		ch.Our.ActiveOnchain = true
		ch.Their.ActiveOnchain = true
		ch.Our.OnchainBalances[tonBalanceID] = new(big.Int).Set(amount)
		ch.Their.OnchainBalances[tonBalanceID] = new(big.Int).Set(amount)

		err = node.db.UpdateChannel(context.Background(), ch)
		if err == nil {
			return
		}

		if strings.Contains(err.Error(), "version mismatch") && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		t.Fatalf("node %d update channel %s failed: %v", node.idx, channelAddr, err)
	}
}

func listActiveIncomingDerivativeKeys(node *testNode) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, 4)
	err := node.db.ForEachActiveSpecialMetaKey(context.Background(), func(key ed25519.PublicKey) error {
		keys = append(keys, append(ed25519.PublicKey{}, key...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

type derivativeMonitorStateJSON struct {
	LastCheckedAt int64 `json:"last_checked_at"`
	EntryCrossed  bool  `json:"entry_crossed"`
	Liquidated    bool  `json:"liquidated"`
	HistoryTooOld bool  `json:"history_too_old"`
}

func derivativeMonitor(meta *dbpkg.ConditionalMeta) (derivativeMonitorStateJSON, bool) {
	if meta == nil || meta.SpecialDetails == nil {
		return derivativeMonitorStateJSON{}, false
	}

	raw, err := json.Marshal(meta.SpecialDetails)
	if err != nil {
		return derivativeMonitorStateJSON{}, false
	}

	var state struct {
		Monitor derivativeMonitorStateJSON `json:"monitor"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return derivativeMonitorStateJSON{}, false
	}

	return state.Monitor, true
}

func isDerivativeMonitorLiquidated(meta *dbpkg.ConditionalMeta) bool {
	monitor, ok := derivativeMonitor(meta)
	return ok && monitor.Liquidated
}

func forceDerivativeHistoryTooOldForTest(t *testing.T, node *testNode, incomingKey ed25519.PublicKey) {
	t.Helper()

	meta, err := node.svc.GetVirtualChannelMeta(context.Background(), incomingKey)
	if err != nil {
		t.Fatalf("load incoming derivative meta failed: %v", err)
	}

	meta.CreatedAt = time.Now().UTC().Add(-3 * time.Minute)
	meta.UpdatedAt = time.Now().UTC()

	var packed map[string]any
	raw, err := json.Marshal(meta.SpecialDetails)
	if err != nil {
		t.Fatalf("marshal derivative special details failed: %v", err)
	}
	if err = json.Unmarshal(raw, &packed); err != nil {
		t.Fatalf("unmarshal derivative special details failed: %v", err)
	}
	packed["monitor"] = map[string]any{
		"last_checked_at": meta.CreatedAt.Unix() - 1,
	}
	meta.SpecialDetails = packed

	if err = node.db.UpdateVirtualChannelMeta(context.Background(), meta); err != nil {
		t.Fatalf("update derivative meta for old-history scenario failed: %v", err)
	}
}

type signedMockProvider struct {
	mu    sync.RWMutex
	price *big.Int
	key   ed25519.PrivateKey
}

func newSignedMockProvider(initial *big.Int) *signedMockProvider {
	seed := sha256.Sum256([]byte("testnode-mock-btc-proof-signer"))

	p := &signedMockProvider{
		key: ed25519.NewKeyFromSeed(seed[:]),
	}
	if initial != nil {
		p.price = new(big.Int).Set(initial)
	} else {
		p.price = big.NewInt(0)
	}
	return p
}

func (m *signedMockProvider) Fetch(_ context.Context, at int64) (int64, *big.Int, error) {
	m.mu.RLock()
	price := new(big.Int).Set(m.price)
	m.mu.RUnlock()
	return at, price, nil
}

func (m *signedMockProvider) SetPrice(p *big.Int) {
	if p == nil {
		return
	}

	m.mu.Lock()
	m.price = new(big.Int).Set(p)
	m.mu.Unlock()
}

func (m *signedMockProvider) ProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), m.key.Public().(ed25519.PublicKey)...)
}

func (m *signedMockProvider) SignProofCell(proof *cell.Cell) ([]byte, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cell is nil")
	}
	return proof.Sign(m.key), nil
}

func installMockBTCResolver(t *testing.T, initial string) *oracle.Resolver {
	t.Helper()

	initialCoins, err := tlb.FromDecimal(initial, 9)
	if err != nil {
		t.Fatalf("parse initial mock price %q failed: %v", initial, err)
	}

	resolverID := oracle.GetResolverID("binance", "BTCUSDT")
	prev, hadPrev := oracle.PriceResolvers[resolverID]

	resolver := oracle.NewResolver(newSignedMockProvider(initialCoins.Nano()))
	oracle.PriceResolvers[resolverID] = resolver

	t.Cleanup(func() {
		resolver.Close()
		if hadPrev {
			oracle.PriceResolvers[resolverID] = prev
		} else {
			delete(oracle.PriceResolvers, resolverID)
		}
	})

	return resolver
}

func waitFor(t *testing.T, timeout, interval time.Duration, check func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastMsg string

	for time.Now().Before(deadline) {
		ok, msg := check()
		if msg != "" {
			lastMsg = msg
		}
		if ok {
			if lastMsg != "" {
				t.Log(lastMsg)
			}
			return
		}
		time.Sleep(interval)
	}

	t.Fatalf("timeout: %s", lastMsg)
}

type loopbackHub struct {
	mu       sync.RWMutex
	byPubKey map[string]*loopbackNet
	chain    *testChain
	live     *liveTestnetEnv
}

func newLoopbackHub() *loopbackHub {
	return &loopbackHub{
		byPubKey: map[string]*loopbackNet{},
		chain:    newTestChain(),
	}
}

func (h *loopbackHub) register(net *loopbackNet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byPubKey[string(net.pub)] = net
}

func (h *loopbackHub) get(pub ed25519.PublicKey) *loopbackNet {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byPubKey[string(pub)]
}

type loopbackNet struct {
	hub *loopbackHub
	pub ed25519.PublicKey
	id  [32]byte

	mu                sync.RWMutex
	queryHandler      func(ctx context.Context, peer *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, peer *transport.Peer) error
	afterQuery        func(msg, response any) error
}

func newLoopbackNet(hub *loopbackHub, pub ed25519.PublicKey, port int) *loopbackNet {
	net := &loopbackNet{
		hub: hub,
		pub: append(ed25519.PublicKey{}, pub...),
		id:  sha256.Sum256([]byte(fmt.Sprintf("loopback-%d", port))),
	}
	hub.register(net)
	return net
}

func (n *loopbackNet) GetOurID() []byte {
	return append([]byte{}, n.id[:]...)
}

func (n *loopbackNet) SetHandlers(q func(ctx context.Context, peer *transport.Peer, msg any) (any, error), d func(ctx context.Context, peer *transport.Peer) error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.queryHandler = q
	n.disconnectHandler = d
}

func (n *loopbackNet) Connect(_ context.Context, channelKey ed25519.PublicKey) (*transport.Peer, error) {
	remote := n.hub.get(channelKey)
	if remote == nil {
		return nil, fmt.Errorf("peer %s not found in loopback hub", base64.StdEncoding.EncodeToString(channelKey))
	}

	localPeer := &transport.Peer{ID: remote.GetOurID()}
	remotePeer := &transport.Peer{ID: n.GetOurID()}

	forward := &loopbackConn{
		remote:       remote,
		peerAtRemote: remotePeer,
	}
	backward := &loopbackConn{
		remote:       n,
		peerAtRemote: localPeer,
	}

	localPeer.Conn = forward
	remotePeer.Conn = backward
	return localPeer, nil
}

type loopbackConn struct {
	remote       *loopbackNet
	peerAtRemote *transport.Peer
}

func (c *loopbackConn) Query(ctx context.Context, msg, res tl.Serializable) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.remote.mu.RLock()
	h := c.remote.queryHandler
	afterQuery := c.remote.afterQuery
	c.remote.mu.RUnlock()
	if h == nil {
		return fmt.Errorf("remote loopback query handler is not set")
	}

	out, err := h(ctx, c.peerAtRemote, msg)
	if err != nil {
		return err
	}
	if afterQuery != nil {
		if err := afterQuery(msg, out); err != nil {
			return err
		}
	}

	if res == nil || out == nil {
		return nil
	}

	dst := reflect.ValueOf(res)
	if dst.Kind() != reflect.Ptr || dst.IsNil() {
		return fmt.Errorf("response must be a non-nil pointer")
	}

	src := reflect.ValueOf(out)
	if !src.Type().AssignableTo(dst.Elem().Type()) {
		return fmt.Errorf("response type mismatch: got %T, want %s", out, dst.Elem().Type())
	}
	dst.Elem().Set(src)
	return nil
}

type testChain struct {
	mu sync.RWMutex

	accounts      map[string]*chainclient.Account
	channelStates map[string]*testChainChannelState
	channelPairs  map[string]string
	lastTxByAddr  map[string]*chainclient.Transaction
	txByInMsgHash map[string]*chainclient.Transaction
	updateSinks   []chan any
	knownWallets  map[string]struct{}
	resolver      payments.FullResolver
	nextLT        uint64
	flow          []string
}

type testChainChannelState struct {
	addr              *address.Address
	pairAddr          *address.Address
	storage           payments.ChannelStorageData
	ourActions        *cell.Dictionary
	theirActions      *cell.Dictionary
	theirConditionals *cell.Dictionary
}

func newTestChain() *testChain {
	return &testChain{
		accounts:      map[string]*chainclient.Account{},
		channelStates: map[string]*testChainChannelState{},
		channelPairs:  map[string]string{},
		lastTxByAddr:  map[string]*chainclient.Transaction{},
		txByInMsgHash: map[string]*chainclient.Transaction{},
		knownWallets:  map[string]struct{}{},
		flow:          make([]string, 0, 16),
	}
}

func (c *testChain) setResolver(resolver payments.FullResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolver == nil {
		c.resolver = resolver
	}
}

func (c *testChain) registerUpdates(ch chan any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateSinks = append(c.updateSinks, ch)
}

func (c *testChain) registerWallet(addr *address.Address) {
	if addr == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.knownWallets[addr.String()] = struct{}{}
}

func (c *testChain) flowSnapshot() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.flow))
	copy(out, c.flow)
	return out
}

func (c *testChain) recordFlowLocked(event string) {
	c.flow = append(c.flow, event)
}

func buildTestChannelStorage(ch *dbpkg.Channel) payments.ChannelStorageData {
	isA := ch.WeLeft
	keyA := append(ed25519.PublicKey{}, ch.Our.OnchainInfo.Key...)
	keyB := append(ed25519.PublicKey{}, ch.Their.OnchainInfo.Key...)
	if !isA {
		keyA, keyB = keyB, keyA
	}

	committed := ch.Our.LatestCommitedSeqno
	if ch.Their.LatestCommitedSeqno > committed {
		committed = ch.Their.LatestCommitedSeqno
	}

	return payments.ChannelStorageData{
		IsA:            isA,
		Initialized:    true,
		CommittedSeqno: committed,
		WalletSeqno:    ch.Our.LatestWalletSeqno,
		KeyA:           keyA,
		KeyB:           keyB,
		ChannelID:      ch.ID,
		ClosingConfig: payments.ClosingConfig{
			QuarantineDuration:             2,
			ConditionalCloseDuration:       8,
			ActionsDuration:                8,
			ReplicationMessageAttachAmount: tlb.MustFromTON("0.1"),
		},
		PartyAddress: address.MustParseAddr(ch.Their.Address),
	}
}

func buildTestChannelAccount(storage payments.ChannelStorageData) (*address.Address, *chainclient.Account, error) {
	data, err := tlb.ToCell(storage)
	if err != nil {
		return nil, nil, err
	}

	baseStorage := payments.ChannelStorageData{
		IsA:            storage.IsA,
		Initialized:    false,
		CommittedSeqno: 0,
		WalletSeqno:    0,
		KeyA:           storage.KeyA,
		KeyB:           storage.KeyB,
		ChannelID:      storage.ChannelID,
		ClosingConfig:  storage.ClosingConfig,
	}
	baseData, err := tlb.ToCell(baseStorage)
	if err != nil {
		return nil, nil, err
	}

	stateInit := tlb.StateInit{
		Code: payments.PaymentChannelCodes[0],
		Data: baseData,
	}
	stateCell, err := tlb.ToCell(stateInit)
	if err != nil {
		return nil, nil, err
	}

	addr := address.NewAddress(0, 0, stateCell.Hash())
	return addr, &chainclient.Account{
		Address:         addr,
		Balance:         tlb.MustFromTON("1000"),
		ExtraCurrencies: cell.NewDict(32),
		HasState:        true,
		IsActive:        true,
		Code:            stateInit.Code,
		Data:            data,
	}, nil
}

func (c *testChain) syncChannelPair(t *testing.T, chAB, chBA *dbpkg.Channel) {
	t.Helper()

	stAB := buildTestChannelStorage(chAB)
	stBA := buildTestChannelStorage(chBA)

	addrAB, accAB, err := buildTestChannelAccount(stAB)
	if err != nil {
		t.Fatalf("build test account A failed: %v", err)
	}
	addrBA, accBA, err := buildTestChannelAccount(stBA)
	if err != nil {
		t.Fatalf("build test account B failed: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.accounts[addrAB.String()] = accAB
	c.accounts[addrBA.String()] = accBA
	c.channelPairs[addrAB.String()] = addrBA.String()
	c.channelPairs[addrBA.String()] = addrAB.String()
	c.channelStates[addrAB.String()] = &testChainChannelState{
		addr:              addrAB,
		pairAddr:          addrBA,
		storage:           stAB,
		ourActions:        chAB.Our.Data.ActionStates.Copy(),
		theirActions:      chAB.Their.Data.ActionStates.Copy(),
		theirConditionals: chAB.Their.Data.Conditionals.Copy(),
	}
	c.channelStates[addrBA.String()] = &testChainChannelState{
		addr:              addrBA,
		pairAddr:          addrAB,
		storage:           stBA,
		ourActions:        chBA.Our.Data.ActionStates.Copy(),
		theirActions:      chBA.Their.Data.ActionStates.Copy(),
		theirConditionals: chBA.Their.Data.Conditionals.Copy(),
	}
}

func (s *testChainChannelState) rebuildAccount() (*chainclient.Account, error) {
	data, err := tlb.ToCell(s.storage)
	if err != nil {
		return nil, err
	}

	return &chainclient.Account{
		Address:         s.addr,
		Balance:         tlb.MustFromTON("1000"),
		ExtraCurrencies: cell.NewDict(32),
		HasState:        true,
		IsActive:        true,
		Code:            payments.PaymentChannelCodes[0],
		Data:            data,
	}, nil
}

func copyBytes(src []byte) []byte {
	if src == nil {
		return nil
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

func copyAccount(acc *chainclient.Account) *chainclient.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	if acc.Balance.Nano() != nil {
		out.Balance = tlb.MustFromNano(new(big.Int).Set(acc.Balance.Nano()), 9)
	}
	return &out
}

func txLookupKey(addr *address.Address, msgHash []byte) string {
	if addr == nil {
		return base64.StdEncoding.EncodeToString(msgHash)
	}
	return addr.String() + ":" + base64.StdEncoding.EncodeToString(msgHash)
}

func (c *testChain) nextTransactionLocked(addr *address.Address, typ tlb.MsgType, body *cell.Cell, out []chainclient.MsgInfo) *chainclient.Transaction {
	c.nextLT++

	var prevHash []byte
	var prevLT uint64
	if prev := c.lastTxByAddr[addr.String()]; prev != nil {
		prevHash = append([]byte{}, prev.Hash...)
		prevLT = prev.LT
	}

	msgHash := []byte(nil)
	if body != nil {
		msgHash = append([]byte{}, body.Hash()...)
	}

	hashSeed := append([]byte{}, addr.Data()...)
	hashSeed = append(hashSeed, []byte(string(typ))...)
	hashSeed = append(hashSeed, msgHash...)
	hashSeed = append(hashSeed, byte(c.nextLT))
	hash := sha256.Sum256(hashSeed)

	tx := &chainclient.Transaction{
		Hash:       hash[:],
		PrevTxHash: prevHash,
		LT:         c.nextLT,
		PrevTxLT:   prevLT,
		At:         time.Now().Unix(),
		Success:    true,
		In: chainclient.MsgInfo{
			Type:    typ,
			To:      addr.String(),
			MsgHash: msgHash,
			Body:    body,
		},
		Out: out,
	}

	c.lastTxByAddr[addr.String()] = tx
	if len(msgHash) > 0 {
		c.txByInMsgHash[txLookupKey(addr, msgHash)] = tx
	}
	return tx
}

func (c *testChain) buildChannelUpdateLocked(addr *address.Address, tx *chainclient.Transaction) (*tonpayments.ChannelUpdatedEvent, error) {
	acc := c.accounts[addr.String()]
	if acc == nil {
		return nil, fmt.Errorf("account %s not found", addr.String())
	}
	ch, err := payments.NewPaymentChannelClient(c).ParseChannel(addr, acc.Code, acc.Data, true)
	if err != nil {
		return nil, err
	}
	return &tonpayments.ChannelUpdatedEvent{
		Transaction:   tx,
		LatestChannel: ch,
	}, nil
}

func (c *testChain) publish(updates []*tonpayments.ChannelUpdatedEvent) {
	if len(updates) == 0 {
		return
	}

	c.mu.RLock()
	sinks := append([]chan any(nil), c.updateSinks...)
	c.mu.RUnlock()

	for _, update := range updates {
		for _, sink := range sinks {
			select {
			case sink <- update:
			default:
				go func(ch chan any, upd *tonpayments.ChannelUpdatedEvent) {
					ch <- upd
				}(sink, update)
			}
		}
	}
}

func (c *testChain) GetAccount(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Account, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if acc := c.accounts[addr.String()]; acc != nil {
		return copyAccount(acc), nil
	}
	if _, ok := c.knownWallets[addr.String()]; ok {
		return &chainclient.Account{
			Address:         addr,
			Balance:         tlb.MustFromTON("1000"),
			ExtraCurrencies: cell.NewDict(32),
			HasState:        true,
			IsActive:        true,
		}, nil
	}

	return &chainclient.Account{
		Address:         addr,
		Balance:         tlb.ZeroCoins,
		ExtraCurrencies: cell.NewDict(32),
		HasState:        false,
		IsActive:        false,
	}, nil
}

func (c *testChain) GetJettonWalletAddress(_ context.Context, root, addr *address.Address) (*address.Address, error) {
	seed := append(append([]byte{}, root.Data()...), addr.Data()...)
	h := sha256.Sum256(seed)
	return address.NewAddress(0, 0, h[:]), nil
}

func (c *testChain) GetJettonBalance(_ context.Context, _ *address.Address, _ *address.Address, _ time.Time) (*big.Int, error) {
	return big.NewInt(0), nil
}

func (c *testChain) GetLastTransaction(_ context.Context, addr *address.Address, _ time.Time) (*chainclient.Transaction, *chainclient.Account, error) {
	acc, err := c.GetAccount(context.Background(), addr, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastTxByAddr[addr.String()], acc, nil
}

func (c *testChain) GetTransactionByInMsgHash(_ context.Context, addr *address.Address, msgHash []byte, _ time.Time) (*chainclient.Transaction, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.txByInMsgHash[txLookupKey(addr, msgHash)], nil
}

func stateSideForContract(st *payments.StateBodySigned, isA bool) *payments.StateSide {
	if st == nil {
		return nil
	}
	side := st.Body.B
	if !isA {
		side = st.Body.A
	}
	return &payments.StateSide{
		ConditionalsHash: copyBytes(side.ConditionalsHash),
		ActionStatesHash: copyBytes(side.ActionStatesHash),
	}
}

func (c *testChain) applyUncoopCloseLocked(target *testChainChannelState, body *cell.Cell, msg *payments.UncoopCloseMsg) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	now := uint32(time.Now().Unix())

	if target.storage.Quarantine == nil {
		target.storage.Quarantine = &payments.QuarantinedState{
			Seqno:                msg.Signed.State.Body.Seqno,
			TheirState:           stateSideForContract(msg.Signed.State, target.storage.IsA),
			QuarantineStarts:     now,
			CommittedByOwner:     true,
			ActionsToExecuteHash: make([]byte, 32),
		}
	}
	acc, err := target.rebuildAccount()
	if err != nil {
		return nil, nil, err
	}
	c.accounts[target.addr.String()] = acc

	if pair := c.channelStates[target.pairAddr.String()]; pair != nil && pair.storage.Quarantine == nil {
		pair.storage.Quarantine = &payments.QuarantinedState{
			Seqno:                msg.Signed.State.Body.Seqno,
			TheirState:           stateSideForContract(msg.Signed.State, pair.storage.IsA),
			QuarantineStarts:     now,
			CommittedByOwner:     false,
			ActionsToExecuteHash: make([]byte, 32),
		}
		accPair, err := pair.rebuildAccount()
		if err != nil {
			return nil, nil, err
		}
		c.accounts[pair.addr.String()] = accPair
	}

	tx := c.nextTransactionLocked(target.addr, tlb.MsgTypeExternalIn, body, nil)
	update, err := c.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		return nil, nil, err
	}
	c.recordFlowLocked("uncoop-start:" + target.addr.String())
	return []*tonpayments.ChannelUpdatedEvent{update}, append([]byte{}, tx.Hash...), nil
}

func (c *testChain) applySettleLocked(target *testChainChannelState, body *cell.Cell, msg *payments.SettleMsg, flowLabel string) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	if target.storage.Quarantine == nil || target.storage.Quarantine.TheirState == nil {
		return nil, nil, fmt.Errorf("channel %s is not quarantined", target.addr.String())
	}
	if c.resolver == nil {
		return nil, nil, fmt.Errorf("test chain resolver is not set")
	}

	updatedCond := target.theirConditionals.Copy()
	updatedActions := target.theirActions.Copy()

	kvs, err := msg.Signed.ToSettle.LoadAll()
	if err != nil {
		return nil, nil, err
	}
	condProofBody, err := msg.Signed.ConditionalsProof.PeekRef(0)
	if err != nil {
		return nil, nil, err
	}
	actProofBody, err := msg.Signed.ActionsInputProof.PeekRef(0)
	if err != nil {
		return nil, nil, err
	}

	condProofDict := condProofBody.AsDict(256)
	actProofDict := actProofBody.AsDict(256)
	empty := cell.BeginCell().EndCell()

	for _, kv := range kvs {
		key, err := kv.Key.LoadSlice(256)
		if err != nil {
			return nil, nil, err
		}
		keyCell := cell.BeginCell().MustStoreSlice(key, 256).EndCell()

		condCode, err := condProofDict.LoadValue(keyCell)
		if err != nil {
			return nil, nil, err
		}
		cond, err := payments.CodeToConditional(context.Background(), condCode.MustToCell(), c.resolver)
		if err != nil {
			return nil, nil, err
		}

		actState, err := actProofDict.LoadValue(cond.GetAction().IDCell())
		if err != nil {
			return nil, nil, err
		}

		newActState, err := cond.Execute(actState.MustToCell(), kv.Value.MustToCell(), make(map[string]*payments.LockedDepositInfo))
		if err != nil {
			return nil, nil, err
		}

		if err = updatedCond.Set(keyCell, empty); err != nil {
			return nil, nil, err
		}
		if err = updatedActions.Set(cond.GetAction().IDCell(), newActState); err != nil {
			return nil, nil, err
		}
	}

	target.theirConditionals = updatedCond
	target.theirActions = updatedActions
	target.storage.WalletSeqno = msg.Signed.WalletSeqno + 1
	target.storage.Quarantine.TheirState.ConditionalsHash = updatedCond.AsCell().Hash()
	target.storage.Quarantine.TheirState.ActionStatesHash = updatedActions.AsCell().Hash()

	if pair := c.channelStates[target.pairAddr.String()]; pair != nil {
		pair.ourActions = updatedActions.Copy()
	}

	acc, err := target.rebuildAccount()
	if err != nil {
		return nil, nil, err
	}
	c.accounts[target.addr.String()] = acc

	evBody, err := tlb.ToCell(payments.ConditionalsSettledEvent{
		NewConditionalsHash: updatedCond.AsCell().Hash(),
		NewActionsHash:      updatedActions.AsCell().Hash(),
	})
	if err != nil {
		return nil, nil, err
	}

	tx := c.nextTransactionLocked(target.addr, tlb.MsgTypeExternalIn, body, []chainclient.MsgInfo{{
		Type:    tlb.MsgTypeExternalOut,
		From:    target.addr.String(),
		To:      target.addr.String(),
		MsgHash: evBody.Hash(),
		Body:    evBody,
	}})
	update, err := c.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		return nil, nil, err
	}
	c.recordFlowLocked(flowLabel + ":" + target.addr.String())
	return []*tonpayments.ChannelUpdatedEvent{update}, append([]byte{}, tx.Hash...), nil
}

func (c *testChain) applyFinalizeLocked(target *testChainChannelState, body *cell.Cell, msg *payments.FinalizeSettleMsg) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	if target.storage.Quarantine == nil {
		return nil, nil, fmt.Errorf("channel %s is not quarantined", target.addr.String())
	}

	target.storage.WalletSeqno = msg.Signed.WalletSeqno + 1
	target.storage.Quarantine.OurSettlementFinalized = true

	if pair := c.channelStates[target.pairAddr.String()]; pair != nil && pair.storage.Quarantine != nil {
		pair.storage.Quarantine.ActionsToExecuteHash = copyBytes(msg.Signed.ActionsInputHash)
		accPair, err := pair.rebuildAccount()
		if err != nil {
			return nil, nil, err
		}
		c.accounts[pair.addr.String()] = accPair
	}

	acc, err := target.rebuildAccount()
	if err != nil {
		return nil, nil, err
	}
	c.accounts[target.addr.String()] = acc

	tx := c.nextTransactionLocked(target.addr, tlb.MsgTypeExternalIn, body, nil)
	update, err := c.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		return nil, nil, err
	}
	c.recordFlowLocked("finalize-settle:" + target.addr.String())
	return []*tonpayments.ChannelUpdatedEvent{update}, append([]byte{}, tx.Hash...), nil
}

func (c *testChain) applyProxyExecuteLocked(target *testChainChannelState, body *cell.Cell, msg *payments.ProxyExecuteActionsMsg) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	if c.resolver == nil {
		return nil, nil, fmt.Errorf("test chain resolver is not set")
	}

	target.storage.WalletSeqno = msg.Signed.WalletSeqno + 1

	updates := make([]*tonpayments.ChannelUpdatedEvent, 0, 2)
	if pair := c.channelStates[target.pairAddr.String()]; pair != nil {
		act, err := payments.CodeToAction(context.Background(), msg.Signed.Msg.Signed.Action, c.resolver)
		if err != nil {
			return nil, nil, err
		}
		if err = pair.ourActions.Set(act.IDCell(), cell.BeginCell().EndCell()); err != nil {
			return nil, nil, err
		}
		if pair.storage.Quarantine != nil {
			pair.storage.Quarantine.ActionsToExecuteHash = pair.ourActions.AsCell().Hash()
		}
		accPair, err := pair.rebuildAccount()
		if err != nil {
			return nil, nil, err
		}
		c.accounts[pair.addr.String()] = accPair

		repTx := c.nextTransactionLocked(pair.addr, tlb.MsgTypeInternal, body, nil)
		repUpdate, err := c.buildChannelUpdateLocked(pair.addr, repTx)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, repUpdate)
	}

	acc, err := target.rebuildAccount()
	if err != nil {
		return nil, nil, err
	}
	c.accounts[target.addr.String()] = acc

	tx := c.nextTransactionLocked(target.addr, tlb.MsgTypeExternalIn, body, nil)
	update, err := c.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		return nil, nil, err
	}
	updates = append(updates, update)
	c.recordFlowLocked("execute-action:" + target.addr.String())
	return updates, append([]byte{}, tx.Hash...), nil
}

func (c *testChain) applyFinishCloseLocked(target *testChainChannelState, body *cell.Cell) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	target.storage.Initialized = false
	target.storage.Quarantine = nil
	acc, err := target.rebuildAccount()
	if err != nil {
		return nil, nil, err
	}
	c.accounts[target.addr.String()] = acc

	tx := c.nextTransactionLocked(target.addr, tlb.MsgTypeExternalIn, body, nil)
	update, err := c.buildChannelUpdateLocked(target.addr, tx)
	if err != nil {
		return nil, nil, err
	}

	updates := []*tonpayments.ChannelUpdatedEvent{update}
	if pair := c.channelStates[target.pairAddr.String()]; pair != nil {
		pair.storage.Initialized = false
		pair.storage.Quarantine = nil
		accPair, err := pair.rebuildAccount()
		if err != nil {
			return nil, nil, err
		}
		c.accounts[pair.addr.String()] = accPair

		repTx := c.nextTransactionLocked(pair.addr, tlb.MsgTypeInternal, body, nil)
		repUpdate, err := c.buildChannelUpdateLocked(pair.addr, repTx)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, repUpdate)
	}

	c.recordFlowLocked("finish-close:" + target.addr.String())
	return updates, append([]byte{}, tx.Hash...), nil
}

func (c *testChain) handleWalletMessagesLocked(reason string, messages []tonpayments.WalletMessage) ([]*tonpayments.ChannelUpdatedEvent, []byte, error) {
	hash := sha256.New()
	hash.Write([]byte(reason))

	var updates []*tonpayments.ChannelUpdatedEvent
	for _, msg := range messages {
		if msg.To != nil {
			hash.Write(msg.To.Data())
		}
		hash.Write(msg.Amount.Nano().Bytes())

		if msg.StateInit != nil {
			stCell, err := tlb.ToCell(*msg.StateInit)
			if err != nil {
				return nil, nil, err
			}
			addr := address.NewAddress(0, 0, stCell.Hash())
			if _, exists := c.accounts[addr.String()]; !exists {
				c.accounts[addr.String()] = &chainclient.Account{
					Address:         addr,
					Balance:         tlb.MustFromTON("1"),
					ExtraCurrencies: cell.NewDict(32),
					HasState:        true,
					IsActive:        true,
					Code:            msg.StateInit.Code,
					Data:            msg.StateInit.Data,
				}
				c.recordFlowLocked("deploy-resolver:" + addr.String())
			}
			continue
		}

		if msg.To == nil || msg.Body == nil {
			continue
		}

		acc := c.accounts[msg.To.String()]
		if acc == nil || acc.Code == nil {
			continue
		}

		if bytes.Equal(acc.Code.Hash(), condcontracts.Codes[0].Hash()) {
			var commit condcontracts.Commit
			if err := tlb.LoadFromCell(&commit, msg.Body.MustBeginParse()); err == nil {
				storage, err := condcontracts.LoadDerivativeStorage(acc.Data)
				if err != nil {
					return nil, nil, err
				}

				var entry, exit condcontracts.PriceProof
				if err = tlb.LoadFromCell(&entry, commit.Entry.SignedBody.MustBeginParse()); err != nil {
					return nil, nil, err
				}
				if err = tlb.LoadFromCell(&exit, commit.Exit.SignedBody.MustBeginParse()); err != nil {
					return nil, nil, err
				}

				storage.EntryAt = entry.At
				storage.ExitAt = exit.At
				storage.ExitPrice = exit.Price
				if storage.QuarantineTill < exit.At {
					storage.QuarantineTill = exit.At
				}

				data, err := tlb.ToCell(storage)
				if err != nil {
					return nil, nil, err
				}
				stateInit, err := condcontracts.BuildDerivativeStateInit(data)
				if err != nil {
					return nil, nil, err
				}
				c.accounts[msg.To.String()] = &chainclient.Account{
					Address:         msg.To,
					Balance:         tlb.MustFromTON("1"),
					ExtraCurrencies: cell.NewDict(32),
					HasState:        true,
					IsActive:        true,
					Code:            stateInit.Code,
					Data:            stateInit.Data,
				}
				c.recordFlowLocked("commit-resolver:" + msg.To.String())
				continue
			}

			var proxy condcontracts.ProxySettle
			if err := tlb.LoadFromCell(&proxy, msg.Body.MustBeginParse()); err == nil {
				storage, err := condcontracts.LoadDerivativeStorage(acc.Data)
				if err != nil {
					return nil, nil, err
				}

				target := storage.Config.AddressB
				if proxy.ToA {
					target = storage.Config.AddressA
				}
				targetState := c.channelStates[target.String()]
				if targetState == nil {
					return nil, nil, fmt.Errorf("proxy settle target %s is not registered", target.String())
				}

				var settle payments.SettleMsg
				if err = tlb.LoadFromCell(&settle, proxy.Msg.MustBeginParse()); err != nil {
					return nil, nil, err
				}
				upd, _, err := c.applySettleLocked(targetState, proxy.Msg, &settle, "proxy-settle")
				if err != nil {
					return nil, nil, err
				}
				updates = append(updates, upd...)
			}
		}
	}

	sum := hash.Sum(nil)
	return updates, append([]byte{}, sum[:32]...), nil
}

func (c *testChain) SendWaitExternalMessage(_ context.Context, to *address.Address, body *cell.Cell) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	target := c.channelStates[to.String()]
	if target == nil {
		payload := append([]byte{}, to.Data()...)
		if body != nil {
			payload = append(payload, body.Hash()...)
		}
		h := sha256.Sum256(payload)
		return h[:], nil
	}

	var (
		updates []*tonpayments.ChannelUpdatedEvent
		hash    []byte
		err     error
	)

	var uncoop payments.UncoopCloseMsg
	if err = tlb.LoadFromCell(&uncoop, body.MustBeginParse()); err == nil {
		updates, hash, err = c.applyUncoopCloseLocked(target, body, &uncoop)
	} else {
		var settle payments.SettleMsg
		if err = tlb.LoadFromCell(&settle, body.MustBeginParse()); err == nil {
			updates, hash, err = c.applySettleLocked(target, body, &settle, "settle")
		} else {
			var fin payments.FinalizeSettleMsg
			if err = tlb.LoadFromCell(&fin, body.MustBeginParse()); err == nil {
				updates, hash, err = c.applyFinalizeLocked(target, body, &fin)
			} else {
				var exec payments.ProxyExecuteActionsMsg
				if err = tlb.LoadFromCell(&exec, body.MustBeginParse()); err == nil {
					updates, hash, err = c.applyProxyExecuteLocked(target, body, &exec)
				} else {
					var finish payments.FinishUncooperativeClose
					if err = tlb.LoadFromCell(&finish, body.MustBeginParse()); err == nil {
						updates, hash, err = c.applyFinishCloseLocked(target, body)
					} else {
						payload := append([]byte{}, to.Data()...)
						if body != nil {
							payload = append(payload, body.Hash()...)
						}
						h := sha256.Sum256(payload)
						return h[:], nil
					}
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}

	go c.publish(updates)
	return hash, nil
}

type stubWallet struct {
	addr  *address.Address
	chain *testChain
}

func newStubWallet(seed ed25519.PublicKey, chain *testChain) *stubWallet {
	h := sha256.Sum256(seed)
	return &stubWallet{addr: address.NewAddress(0, 0, h[:]), chain: chain}
}

func (w *stubWallet) WalletAddress() *address.Address {
	return w.addr
}

func (w *stubWallet) DoTransactionMany(_ context.Context, reason string, messages []tonpayments.WalletMessage) ([]byte, error) {
	if w.chain == nil {
		hash := sha256.New()
		hash.Write([]byte(reason))
		for _, msg := range messages {
			if msg.To != nil {
				hash.Write(msg.To.Data())
			}
			hash.Write(msg.Amount.Nano().Bytes())
		}
		sum := hash.Sum(nil)
		return append([]byte{}, sum[:32]...), nil
	}

	w.chain.mu.Lock()
	updates, hash, err := w.chain.handleWalletMessagesLocked(reason, messages)
	w.chain.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go w.chain.publish(updates)
	return hash, nil
}

func (w *stubWallet) DoTransaction(ctx context.Context, reason string, to *address.Address, amt tlb.Coins, body *cell.Cell) ([]byte, error) {
	return w.DoTransactionMany(ctx, reason, []tonpayments.WalletMessage{{
		To:     to,
		Amount: amt,
		Body:   body,
	}})
}
