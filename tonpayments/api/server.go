package api

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"net/http"
	"time"
)

type Queue interface {
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
	OpenVirtualChannel(ctx context.Context, with, instructionKey ed25519.PublicKey, private ed25519.PrivateKey, chain []transport.OpenVirtualInstruction, vch payments.VirtualChannel) error
	DeployChannelWithNode(ctx context.Context, capacity tlb.Coins, nodeKey ed25519.PublicKey) (*address.Address, error)
}

type Success struct {
	Success bool
}

type Error struct {
	Error string `json:"error"`
}

type Server struct {
	svc            Service
	queue          Queue
	webhook        string
	webhookSignal  chan bool
	srv            http.Server
	sender         http.Client
	apiCredentials *Credentials
}

type Credentials struct {
	Login    string
	Password string
}

func NewServer(addr, webhook string, svc Service, queue Queue, credentials *Credentials) *Server {
	s := &Server{
		svc:     svc,
		queue:   queue,
		webhook: webhook,
		sender: http.Client{
			Timeout: 10 * time.Second,
		},
		apiCredentials: credentials,
	}

	mx := http.NewServeMux()
	mx.HandleFunc("/api/v1/channel/onchain/open", s.checkCredentials(s.handleChannelOpen))
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
	k, err := hex.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("incorrect key format, should be in hex: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("incorrect key length, should be 32")
	}
	return ed25519.PublicKey(k), nil
}
