package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"net/http"
	"time"
)

type Queue interface {
	CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context, poolName string) (*db.Task, error)
	RetryTask(ctx context.Context, task *db.Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, poolName string, task *db.Task) error
}

type Service interface {
	GetChannel(ctx context.Context, addr string) (*db.Channel, error)
	ListChannels(ctx context.Context, key ed25519.PublicKey, status db.ChannelStatus) ([]*db.Channel, error)

	GetVirtualChannelMeta(ctx context.Context, key ed25519.PublicKey) (*db.VirtualChannelMeta, error)

	RequestCooperativeClose(ctx context.Context, channelAddr string) error
	RequestUncooperativeClose(ctx context.Context, addr string) error
	CloseVirtualChannel(ctx context.Context, virtualKey ed25519.PublicKey) error
	AddVirtualChannelResolve(ctx context.Context, virtualKey ed25519.PublicKey, state payments.VirtualChannelState) error
	OpenVirtualChannel(ctx context.Context, with, instructionKey, finalDest ed25519.PublicKey, private ed25519.PrivateKey, chain []transport.OpenVirtualInstruction, vch payments.VirtualChannel, jettonMaster *address.Address, ecID uint32) error
	DeployChannelWithNode(ctx context.Context, nodeKey ed25519.PublicKey, jettonMaster *address.Address, ecID uint32) (*address.Address, error)
	TopupChannel(ctx context.Context, addr *address.Address, amount tlb.Coins) error
	RequestWithdraw(ctx context.Context, addr *address.Address, amount tlb.Coins) error
	ResolveCoinConfig(jetton string, ecID uint32, onlyEnabled bool) (*config.CoinConfig, error)
}

type Success struct {
	Success bool `json:"success"`
}

type Error struct {
	Error string `json:"error"`
}

type Server struct {
	svc            Service
	queue          Queue
	webhook        string
	webhookKey     string
	webhookSignal  chan bool
	srv            http.Server
	sender         http.Client
	apiCredentials *Credentials
}

type Credentials struct {
	Login    string
	Password string
}

func NewServer(addr, webhook, webhookKey string, svc Service, queue Queue, credentials *Credentials) *Server {
	s := &Server{
		svc:        svc,
		queue:      queue,
		webhook:    webhook,
		webhookKey: webhookKey,
		sender: http.Client{
			Timeout: 10 * time.Second,
		},
		apiCredentials: credentials,
	}

	mx := http.NewServeMux()
	mx.HandleFunc("/api/v1/channel/onchain/open", s.checkCredentials(s.handleChannelOpen))
	mx.HandleFunc("/api/v1/channel/onchain/topup", s.checkCredentials(s.handleTopup))
	mx.HandleFunc("/api/v1/channel/onchain/withdraw", s.checkCredentials(s.handleWithdraw))
	mx.HandleFunc("/api/v1/channel/onchain/close", s.checkCredentials(s.handleChannelClose))
	mx.HandleFunc("/api/v1/channel/onchain/list", s.checkCredentials(s.handleChannelsList))
	mx.HandleFunc("/api/v1/channel/onchain", s.checkCredentials(s.handleChannelGet))

	mx.HandleFunc("/api/v1/channel/virtual/open", s.checkCredentials(s.handleVirtualOpen))
	mx.HandleFunc("/api/v1/channel/virtual/close", s.checkCredentials(s.handleVirtualClose))
	mx.HandleFunc("/api/v1/channel/virtual/transfer", s.checkCredentials(s.handleVirtualTransfer))
	mx.HandleFunc("/api/v1/channel/virtual/state", s.checkCredentials(s.handleVirtualState))
	mx.HandleFunc("/api/v1/channel/virtual/list", s.checkCredentials(s.handleVirtualList))
	mx.HandleFunc("/api/v1/channel/virtual", s.checkCredentials(s.handleVirtualGet))

	s.srv = http.Server{
		Addr:    addr,
		Handler: mx,
	}

	return s
}

func (s *Server) Start() error {
	if s.webhook != "" {
		go s.startWebhooksSender()
	}
	return s.srv.ListenAndServe()
}

func (s *Server) checkCredentials(handler func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiCredentials != nil {
			login, password, ok := r.BasicAuth()
			if !ok {
				writeErr(w, 401, "unauthorized")
				return
			}

			if s.apiCredentials.Password != password || s.apiCredentials.Login != login {
				writeErr(w, 401, "unauthorized")
				return
			}
		}

		handler(w, r)
	}
}

func writeErr(w http.ResponseWriter, code int, text string) {
	data, _ := json.Marshal(Error{text})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func writeResp(w http.ResponseWriter, obj any) {
	data, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func writeSuccess(w http.ResponseWriter) {
	writeResp(w, Success{true})
}

func parseKey(key string) (ed25519.PublicKey, error) {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("incorrect key format, should be in base64: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("incorrect key length, should be 32")
	}
	return ed25519.PublicKey(k), nil
}

func parseState(state string, key ed25519.PublicKey) (payments.VirtualChannelState, error) {
	s, err := base64.StdEncoding.DecodeString(state)
	if err != nil {
		return payments.VirtualChannelState{}, fmt.Errorf("failed to decode state from hex: %w", err)
	}

	cll, err := cell.FromBOC(s)
	if err != nil {
		return payments.VirtualChannelState{}, fmt.Errorf("failed to parse state: %w", err)
	}

	var st payments.VirtualChannelState
	if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
		return payments.VirtualChannelState{}, fmt.Errorf("failed to parse last known resolve state: %w", err)
	}

	if !st.Verify(key) {
		return payments.VirtualChannelState{}, fmt.Errorf("failed to verify last known resolve state: %w", err)
	}
	return st, nil
}
