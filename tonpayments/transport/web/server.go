//go:build !(js && wasm)

package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"
)

type PeerConnection struct {
	transport *transport.Peer

	queries   map[string]chan tl.Serializable
	pushQueue chan *Event
	mx        sync.RWMutex
}

type HTTP struct {
	key               ed25519.PrivateKey
	peers             map[string]*PeerConnection
	queryHandler      func(ctx context.Context, from *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, from *transport.Peer) error

	ton *client.TON

	mx sync.RWMutex
}

func NewHTTP(ton *client.TON, key ed25519.PrivateKey) *HTTP {
	// id := sha256.Sum256(append([]byte("http-web-server"), key...))
	return &HTTP{
		key:   key,
		ton:   ton,
		peers: make(map[string]*PeerConnection),
	}
}

func (h *HTTP) GetOurID() []byte {
	return h.key.Public().(ed25519.PublicKey)
}

func (h *HTTP) Connect(ctx context.Context, channelKey ed25519.PublicKey) (*transport.Peer, error) {
	return nil, fmt.Errorf("cannot connect to web peer from server")
}

func (h *HTTP) SetHandlers(q func(ctx context.Context, peer *transport.Peer, msg any) (any, error), d func(ctx context.Context, peer *transport.Peer) error) {
	h.queryHandler = q
	h.disconnectHandler = d
}

func (h *HTTP) StartServer(addr string) error {
	m := http.NewServeMux()
	m.HandleFunc("/web-channel/api/v1/push", h.pushHandler)
	m.HandleFunc("/web-channel/api/v1/subscribe", h.sseHandler)
	m.HandleFunc("/web-channel/api/v1/subscribe/auth", h.sseAuthHandler)

	m.HandleFunc("/web-channel/api/v1/ton/account", h.getAccountHandler)
	m.HandleFunc("/web-channel/api/v1/ton/transaction/last", h.getLastTxHandler)
	m.HandleFunc("/web-channel/api/v1/ton/transaction/list", h.getListTxHandler)
	m.HandleFunc("/web-channel/api/v1/ton/jetton/wallet", h.getJettonWalletAddrHandler)
	m.HandleFunc("/web-channel/api/v1/ton/jetton/balance", h.getJettonWalletBalanceHandler)

	return http.ListenAndServe(addr, m)
}

func (h *HTTP) getAccountHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	acc, err := h.ton.GetAccount(r.Context(), addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(acc)
}

func (h *HTTP) getLastTxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	tx, acc, err := h.ton.GetLastTransaction(r.Context(), addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"Account":     acc,
		"Transaction": tx,
	})
}

func (h *HTTP) getListTxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	lt, err := strconv.ParseUint(r.URL.Query().Get("lt"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hash, err := base64.URLEncoding.DecodeString(r.URL.Query().Get("hash"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(hash) != 32 {
		http.Error(w, "hash len incorrect", http.StatusBadRequest)
		return
	}

	txs, err := h.ton.GetTransactionsList(r.Context(), addr, lt, hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"Transactions": txs,
	})
}

func (h *HTTP) getJettonWalletAddrHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jet, err := address.ParseAddr(r.URL.Query().Get("jetton"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	res, err := h.ton.GetJettonWalletAddress(r.Context(), jet, addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"Address": res.String(),
	})
}

func (h *HTTP) getJettonWalletBalanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jet, err := address.ParseAddr(r.URL.Query().Get("jetton"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	res, err := h.ton.GetJettonBalance(r.Context(), jet, addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"Balance": res.String(),
	})
}

func (h *HTTP) pushHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var e Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(e.Key) != 32 {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	h.mx.RLock()
	pk := h.peers[string(e.Key)]
	h.mx.RUnlock()
	if pk == nil {
		http.Error(w, "peer not found, subscribe first", http.StatusUnauthorized)
		return
	}

	var req tl.Serializable
	if _, err := tl.Parse(&req, e.Data, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if e.QueryID != "" {
		pk.mx.Lock()
		defer pk.mx.Unlock()

		if resp, ok := pk.queries[e.QueryID]; ok {
			resp <- req

			// to protect from repeat write to chan
			delete(pk.queries, e.QueryID)

			_ = json.NewEncoder(w).Encode(QueryResponseAccepted{true})
			return
		}

		http.Error(w, "query not found", http.StatusBadRequest)
		return
	}

	res, err := h.queryHandler(r.Context(), pk.transport, req)
	if err != nil {
		log.Debug().Err(err).Msg("failed to handle query")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := tl.Serialize(res, true)
	if err != nil {
		log.Error().Err(err).Msg("failed to serialize response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	_ = json.NewEncoder(w).Encode(Event{Data: data})
}

func (h *HTTP) sseAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sub SubscribeAuth
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if time.Now().UTC().Unix()-sub.Timestamp > 30 || time.Now().UTC().Unix()-sub.Timestamp < -30 {
		http.Error(w, "timestamp expired or future", http.StatusBadRequest)
		return
	}
	if !ed25519.Verify(sub.PeerKey, []byte(fmt.Sprintf("web:%d", sub.Timestamp)), sub.Signature) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	_ = json.NewEncoder(w).Encode(SubscribeAuthResult{
		Token: base64.URLEncoding.EncodeToString(ed25519.Sign(h.key,
			[]byte(fmt.Sprintf("subscribe:%d:%s", sub.Timestamp, base64.URLEncoding.EncodeToString(sub.PeerKey))))),
	})
}

func (h *HTTP) sseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ts, err := strconv.ParseUint(r.URL.Query().Get("timestamp"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	token, err := base64.URLEncoding.DecodeString(r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	keyBytes, err := base64.URLEncoding.DecodeString(r.URL.Query().Get("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if time.Now().UTC().Unix()-int64(ts) > 45 {
		http.Error(w, "timestamp expired", http.StatusBadRequest)
		return
	}

	d := []byte(fmt.Sprintf("subscribe:%d:%s", ts, base64.URLEncoding.EncodeToString(keyBytes)))
	if !ed25519.Verify(h.key.Public().(ed25519.PublicKey), d, token) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	key := string(keyBytes)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	t := &transport.Peer{
		ID: keyBytes,
	}
	oc := &PeerConnection{
		transport: t,
		queries:   make(map[string]chan tl.Serializable),
		pushQueue: make(chan *Event, 4),
	}
	t.Conn = oc

	h.mx.Lock()
	if c := h.peers[key]; c != nil {
		close(c.pushQueue) // close prev subscription
		_ = h.disconnectHandler(context.Background(), c.transport)
	}
	h.peers[key] = oc
	h.mx.Unlock()

	defer func() {
		h.mx.Lock()
		defer h.mx.Unlock()

		if v := h.peers[key]; v == oc {
			delete(h.peers, key)
			_ = h.disconnectHandler(context.Background(), oc.transport)
		}
	}()

	oc.pushQueue <- &Event{Key: oc.transport.ID}
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-oc.pushQueue:
			if msg == nil {
				return
			}

			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}

			if _, err = fmt.Fprintf(w, "data: %s\n\n", string(data)); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

func (p *PeerConnection) Query(ctx context.Context, msg, res tl.Serializable) error {
	data, err := tl.Serialize(msg, true)
	if err != nil {
		return err
	}

	resp := make(chan tl.Serializable, 1)

	qidData := make([]byte, 8)
	if _, err = rand.Read(qidData); err != nil {
		return err
	}

	qid := base64.StdEncoding.EncodeToString(qidData)

	p.mx.Lock()
	p.queries[qid] = resp
	p.mx.Unlock()

	p.pushQueue <- &Event{
		Key:     p.transport.ID,
		QueryID: qid,
		Data:    data,
	}

	select {
	case <-ctx.Done():
		p.mx.Lock()
		delete(p.queries, qid)
		p.mx.Unlock()

		return ctx.Err()
	case val := <-resp:
		// query removed from map at writer level to protect from repeat

		reflect.ValueOf(res).Elem().Set(reflect.ValueOf(val))
	}
	return nil
}
