//go:build !(js && wasm)

package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/derivatives"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type PeerConnection struct {
	transport *transport.Peer

	queries   map[string]chan tl.Serializable
	pushQueue chan *Event
	done      chan struct{}
	mx        sync.RWMutex
}

var errPeerClosed = errors.New("web peer connection closed")

func (p *PeerConnection) close() {
	p.mx.Lock()
	defer p.mx.Unlock()

	select {
	case <-p.done:
		return
	default:
		close(p.done)
		clear(p.queries)
	}
}

type HTTP struct {
	key               ed25519.PrivateKey
	peers             map[string]*PeerConnection
	queryHandler      func(ctx context.Context, from *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, from *transport.Peer) error

	ton           *client.TON
	derivProvider derivatives.CandleProvider

	mx sync.RWMutex
}

func NewHTTP(ton *client.TON, key ed25519.PrivateKey) *HTTP {
	// id := sha256.Sum256(append([]byte("http-web-server"), key...))
	return &HTTP{
		key:           key,
		ton:           ton,
		peers:         make(map[string]*PeerConnection),
		derivProvider: derivatives.NewBinanceFuturesProvider(),
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

	m.HandleFunc("/web-channel/api/v1/ton/external", h.sendExternalHandler)
	m.HandleFunc("/web-channel/api/v1/ton/account", h.getAccountHandler)
	m.HandleFunc("/web-channel/api/v1/ton/transaction/last", h.getLastTxHandler)
	m.HandleFunc("/web-channel/api/v1/ton/transaction/by_in_msg_hash", h.getTxByInMsgHashHandler)
	m.HandleFunc("/web-channel/api/v1/ton/transaction/list", h.getListTxHandler)
	m.HandleFunc("/web-channel/api/v1/ton/jetton/wallet", h.getJettonWalletAddrHandler)
	m.HandleFunc("/web-channel/api/v1/ton/jetton/balance", h.getJettonWalletBalanceHandler)
	m.HandleFunc("/web-channel/api/v1/derivatives/price", h.getDerivativePriceHandler)
	m.HandleFunc("/web-channel/api/v1/derivatives/prices", h.getDerivativePricesRangeHandler)
	m.HandleFunc("/web-channel/api/v1/derivatives/book_volume", h.getDerivativeBookVolumeHandler)

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
		return
	}

	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	acc, err := h.ton.GetAccount(r.Context(), addr, time.Unix(after, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(acc)
}

func (h *HTTP) sendExternalHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	type request struct {
		Body *cell.Cell `json:"body"`
	}

	var req request
	if err = json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	msgHash, err := h.ton.SendWaitExternalMessage(r.Context(), addr, req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"MsgHash": msgHash,
	})
}

func (h *HTTP) getLastTxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, acc, err := h.ton.GetLastTransaction(r.Context(), addr, time.Unix(after, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"Account":     acc,
		"Transaction": tx,
	})
}

func (h *HTTP) getTxByInMsgHashHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	addr, err := address.ParseAddr(r.URL.Query().Get("address"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
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

	tx, err := h.ton.GetTransactionByInMsgHash(r.Context(), addr, hash, time.Unix(after, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
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
		return
	}

	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	res, err := h.ton.GetJettonBalance(r.Context(), jet, addr, time.Unix(after, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"Balance": res.String(),
	})
}

func derivativeResolverCandidates(symbol string) []uint32 {
	switch strings.ToUpper(strings.TrimSpace(symbol)) {
	case "BTC", "BTCUSDT":
		return []uint32{
			oracle.GetResolverID("binance", "BTCUSDT"),
			2, // backward compatibility with legacy hardcoded asset id
		}
	case "TON", "TONUSDT":
		return []uint32{
			oracle.GetResolverID("binance", "TONUSDT"),
			1, // backward compatibility with legacy hardcoded asset id
		}
	default:
		return nil
	}
}

func (h *HTTP) getDerivativePriceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	candidates := derivativeResolverCandidates(symbol)
	if len(candidates) == 0 {
		http.Error(w, "unsupported symbol", http.StatusBadRequest)
		return
	}

	var resolver *oracle.Resolver
	for _, id := range candidates {
		resolver = oracle.PriceResolvers[id]
		if resolver != nil {
			break
		}
	}
	if resolver == nil {
		http.Error(w, "price resolver is not configured", http.StatusNotFound)
		return
	}

	atRaw := strings.TrimSpace(r.URL.Query().Get("at"))

	cells := resolver.GetSignedPricesSince(0)
	if len(cells) == 0 {
		http.Error(w, "price is unavailable", http.StatusServiceUnavailable)
		return
	}

	var found *cell.Cell
	if atRaw == "" {
		// Get the latest
		found = cells[len(cells)-1]
	} else {
		at, err := strconv.ParseInt(atRaw, 10, 64)
		if err != nil {
			http.Error(w, "invalid at", http.StatusBadRequest)
			return
		}
		for _, c := range cells {
			pAt, _, pErr := oracle.ParsePriceProof(c)
			if pErr == nil && pAt == at {
				found = c
				break
			}
		}
	}

	if found == nil {
		http.Error(w, "price is unavailable", http.StatusServiceUnavailable)
		return
	}

	// Return at, price, and boc for backward compatibility
	pAt, pPrice, _ := oracle.ParsePriceProof(found)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"symbol": symbol,
		"at":     pAt,
		"price":  pPrice.String(),
		"boc":    base64.StdEncoding.EncodeToString(found.ToBOC()),
	})
}

func (h *HTTP) getDerivativePricesRangeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	candidates := derivativeResolverCandidates(symbol)
	if len(candidates) == 0 {
		http.Error(w, "unsupported symbol", http.StatusBadRequest)
		return
	}

	var resolver *oracle.Resolver
	for _, id := range candidates {
		resolver = oracle.PriceResolvers[id]
		if resolver != nil {
			break
		}
	}
	if resolver == nil {
		http.Error(w, "price resolver is not configured", http.StatusNotFound)
		return
	}

	sinceRaw := strings.TrimSpace(r.URL.Query().Get("since"))
	var since int64
	if sinceRaw != "" {
		var err error
		since, err = strconv.ParseInt(sinceRaw, 10, 64)
		if err != nil {
			http.Error(w, "invalid since", http.StatusBadRequest)
			return
		}
	}

	cells := resolver.GetSignedPricesSince(since)

	// Return array of base64-encoded BOC strings
	bocs := make([]string, 0, len(cells))
	for _, c := range cells {
		bocs = append(bocs, base64.StdEncoding.EncodeToString(c.ToBOC()))
	}

	_ = json.NewEncoder(w).Encode(bocs)
}

func (h *HTTP) getDerivativeBookVolumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		http.Error(w, "symbol is required", http.StatusBadRequest)
		return
	}

	depth := 20
	if rawDepth := strings.TrimSpace(r.URL.Query().Get("depth")); rawDepth != "" {
		parsed, err := strconv.Atoi(rawDepth)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid depth", http.StatusBadRequest)
			return
		}
		depth = parsed
	}

	volumeLimit := 120
	if rawVolumeLimit := strings.TrimSpace(r.URL.Query().Get("volume_limit")); rawVolumeLimit != "" {
		parsed, err := strconv.Atoi(rawVolumeLimit)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid volume_limit", http.StatusBadRequest)
			return
		}
		volumeLimit = parsed
	}

	provider := h.derivProvider
	if provider == nil {
		provider = derivatives.NewBinanceFuturesProvider()
	}

	data, err := provider.FetchOrderBookAndVolume(r.Context(), symbol, depth, volumeLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	_ = json.NewEncoder(w).Encode(data)
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

	if !e.Verify(e.Key) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	select {
	case <-pk.done:
		http.Error(w, errPeerClosed.Error(), http.StatusUnauthorized)
		return
	default:
	}

	var req tl.Serializable
	if _, err := tl.Parse(&req, e.Data, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if e.QueryID != "" {
		pk.mx.Lock()
		defer pk.mx.Unlock()
		select {
		case <-pk.done:
			http.Error(w, errPeerClosed.Error(), http.StatusUnauthorized)
			return
		default:
		}

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
		return
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
		done:      make(chan struct{}),
	}
	t.Conn = oc

	h.mx.Lock()
	previous := h.peers[key]
	if previous != nil {
		previous.close()
	}
	h.peers[key] = oc
	h.mx.Unlock()
	if previous != nil {
		_ = h.disconnectHandler(context.Background(), previous.transport)
	}

	defer func() {
		oc.close()
		h.mx.Lock()
		current := h.peers[key] == oc
		if current {
			delete(h.peers, key)
		}
		h.mx.Unlock()
		if current {
			_ = h.disconnectHandler(context.Background(), oc.transport)
		}
	}()

	writeEvent := func(msg *Event) error {
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := writeEvent(&Event{Key: oc.transport.ID}); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-oc.done:
			return
		case msg := <-oc.pushQueue:
			select {
			case <-oc.done:
				return
			default:
			}
			if err := writeEvent(msg); err != nil {
				return
			}
		}
	}
}

func (p *PeerConnection) Query(ctx context.Context, msg, res tl.Serializable) error {
	result := reflect.ValueOf(res)
	if result.Kind() != reflect.Pointer || result.IsNil() {
		return fmt.Errorf("web query result must be a non-nil pointer")
	}
	result = result.Elem()
	if err := ctx.Err(); err != nil {
		return err
	}

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
	select {
	case <-p.done:
		p.mx.Unlock()
		return errPeerClosed
	default:
	}
	p.queries[qid] = resp
	p.mx.Unlock()
	defer func() {
		p.mx.Lock()
		delete(p.queries, qid)
		p.mx.Unlock()
	}()

	event := &Event{
		Key:     p.transport.ID,
		QueryID: qid,
		Data:    data,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errPeerClosed
	case p.pushQueue <- event:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errPeerClosed
	case val := <-resp:
		select {
		case <-p.done:
			return errPeerClosed
		default:
		}
		value := reflect.ValueOf(val)
		if !value.IsValid() || !value.Type().AssignableTo(result.Type()) {
			return fmt.Errorf("unexpected web peer response type %T, expected %s", val, result.Type())
		}
		result.Set(value)
	}
	return nil
}
