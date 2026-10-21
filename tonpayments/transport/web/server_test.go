//go:build !(js && wasm)

package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tl"
)

func testPeerConnection(t *testing.T, key ed25519.PublicKey) *PeerConnection {
	t.Helper()
	p := &PeerConnection{
		transport: &transport.Peer{ID: key},
		queries:   make(map[string]chan tl.Serializable),
		pushQueue: make(chan *Event, 4),
		done:      make(chan struct{}),
	}
	p.transport.Conn = p
	t.Cleanup(p.close)
	return p
}

func assertNoPendingQueries(t *testing.T, p *PeerConnection) {
	t.Helper()
	p.mx.RLock()
	defer p.mx.RUnlock()
	if len(p.queries) != 0 {
		t.Fatalf("connection has %d pending queries", len(p.queries))
	}
}

func pushSignedReply(t *testing.T, h *HTTP, key ed25519.PrivateKey, queryID string, reply tl.Serializable) *httptest.ResponseRecorder {
	t.Helper()
	data, err := tl.Serialize(reply, true)
	if err != nil {
		t.Fatal(err)
	}
	event := Event{Key: key.Public().(ed25519.PublicKey), QueryID: queryID, Data: data}
	event.Sign(key)
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.pushHandler(w, httptest.NewRequest(http.MethodPost, "/web-channel/api/v1/push", bytes.NewReader(body)))
	return w
}

func TestPeerConnectionQuerySignedResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   tl.Serializable
		wantErr bool
	}{
		{name: "matching response", reply: transport.Pong{Value: 42}},
		{name: "wrong response type", reply: transport.Decision{Agreed: true}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key, private, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			p := testPeerConnection(t, key)
			h := &HTTP{peers: map[string]*PeerConnection{string(key): p}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var result transport.Pong
			finished := make(chan error, 1)
			go func() { finished <- p.Query(ctx, transport.Ping{Value: 42}, &result) }()

			var query *Event
			select {
			case query = <-p.pushQueue:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			w := pushSignedReply(t, h, private, query.QueryID, tc.reply)
			if w.Code != http.StatusOK {
				t.Fatalf("reply status %d: %s", w.Code, w.Body)
			}
			err = <-finished
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unexpected web peer response type") {
					t.Fatalf("wrong response type returned %v", err)
				}
			} else if err != nil || result.Value != 42 {
				t.Fatalf("query returned %+v, %v", result, err)
			}
			assertNoPendingQueries(t, p)
			if w = pushSignedReply(t, h, private, query.QueryID, tc.reply); w.Code != http.StatusBadRequest {
				t.Fatalf("duplicate reply status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestPeerConnectionQueryCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fullQueue bool
		deadline  bool
	}{
		{name: "cancel while queue full", fullQueue: true},
		{name: "deadline while queue full", fullQueue: true, deadline: true},
		{name: "cancel while waiting for reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := testPeerConnection(t, nil)
				if tc.fullQueue {
					for range cap(p.pushQueue) {
						p.pushQueue <- &Event{}
					}
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if tc.deadline {
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				finished := make(chan error, 1)
				go func() {
					var result transport.Pong
					finished <- p.Query(ctx, transport.Ping{}, &result)
				}()
				synctest.Wait()
				if tc.deadline {
					time.Sleep(time.Second)
				} else {
					cancel()
				}
				wantErr := context.Canceled
				if tc.deadline {
					wantErr = context.DeadlineExceeded
				}
				if err := <-finished; !errors.Is(err, wantErr) {
					t.Fatalf("query error %v, want %v", err, wantErr)
				}
				assertNoPendingQueries(t, p)
			})
		})
	}
}

func TestPeerConnectionCloseCompletesConcurrentQueries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testPeerConnection(t, nil)
		const count = 32
		finished := make(chan error, count)
		for range count {
			go func() {
				var result transport.Pong
				finished <- p.Query(t.Context(), transport.Ping{}, &result)
			}()
		}
		synctest.Wait()
		p.close()
		p.close()
		for range count {
			if err := <-finished; !errors.Is(err, errPeerClosed) {
				t.Fatalf("query error %v, want %v", err, errPeerClosed)
			}
		}
		assertNoPendingQueries(t, p)
		var result transport.Pong
		if err := p.Query(t.Context(), transport.Ping{}, &result); !errors.Is(err, errPeerClosed) {
			t.Fatalf("closed connection accepted query: %v", err)
		}
	})
}

type eventResponseWriter struct {
	header http.Header
	events chan []byte
}

func (w *eventResponseWriter) Header() http.Header { return w.header }
func (w *eventResponseWriter) WriteHeader(int)     {}
func (w *eventResponseWriter) Flush()              {}
func (w *eventResponseWriter) Write(data []byte) (int, error) {
	w.events <- bytes.Clone(data)
	return len(data), nil
}

func startTestSubscription(t *testing.T, h *HTTP, key ed25519.PublicKey) (*eventResponseWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	timestamp := time.Now().Unix()
	token := ed25519.Sign(h.key, []byte(fmt.Sprintf("subscribe:%d:%s", timestamp, base64.URLEncoding.EncodeToString(key))))
	params := url.Values{
		"timestamp": {fmt.Sprint(timestamp)},
		"token":     {base64.URLEncoding.EncodeToString(token)},
		"key":       {base64.URLEncoding.EncodeToString(key)},
	}
	ctx, cancel := context.WithCancel(t.Context())
	w := &eventResponseWriter{header: make(http.Header), events: make(chan []byte, 8)}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.sseHandler(w, httptest.NewRequest(http.MethodGet, "/web-channel/api/v1/subscribe?"+params.Encode(), nil).WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("SSE handler did not stop")
		}
	})
	return w, cancel, finished
}

func readTestEvent(t *testing.T, w *eventResponseWriter) Event {
	t.Helper()
	select {
	case data := <-w.events:
		var event Event
		if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data: "))), &event); err != nil {
			t.Fatal(err)
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("SSE event was not delivered")
		return Event{}
	}
}

func TestSSEReconnectAndDisconnectCompleteQueries(t *testing.T) {
	t.Parallel()
	_, serverKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTP(nil, serverKey)
	disconnected := make(chan *transport.Peer, 2)
	h.SetHandlers(nil, func(_ context.Context, peer *transport.Peer) error {
		// Callbacks must also be able to inspect the connection registry.
		h.mx.RLock()
		h.mx.RUnlock()
		disconnected <- peer
		return nil
	})
	w, _, stopped := startTestSubscription(t, h, key)
	if event := readTestEvent(t, w); !bytes.Equal(event.Key, key) || event.QueryID != "" {
		t.Fatalf("unexpected initial event %+v", event)
	}
	h.mx.RLock()
	first := h.peers[string(key)]
	h.mx.RUnlock()
	finished := make(chan error, 1)
	go func() {
		var result transport.Pong
		finished <- first.Query(t.Context(), transport.Ping{}, &result)
	}()
	oldQuery := readTestEvent(t, w)

	w, cancel, _ := startTestSubscription(t, h, key)
	readTestEvent(t, w)
	if err := <-finished; !errors.Is(err, errPeerClosed) {
		t.Fatalf("replaced connection query error %v", err)
	}
	<-stopped
	assertNoPendingQueries(t, first)
	if peer := <-disconnected; peer != first.transport {
		t.Fatal("replacement disconnected the wrong peer")
	}
	if reply := pushSignedReply(t, h, private, oldQuery.QueryID, transport.Pong{}); reply.Code != http.StatusBadRequest {
		t.Fatalf("old connection reply was accepted: %d", reply.Code)
	}

	h.mx.RLock()
	second := h.peers[string(key)]
	h.mx.RUnlock()
	if second == first || second.transport.Conn != second {
		t.Fatal("replacement peer was not initialized and published")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var result transport.Pong
		finished <- second.Query(t.Context(), transport.Ping{}, &result)
	}()
	query := readTestEvent(t, w)
	if reply := pushSignedReply(t, h, private, query.QueryID, transport.Pong{Value: 9}); reply.Code != http.StatusOK {
		t.Fatalf("replacement reply status %d", reply.Code)
	}
	if err := <-finished; err != nil {
		t.Fatalf("replacement query failed: %v", err)
	}
	wg.Wait()
	assertNoPendingQueries(t, second)
	go func() {
		var result transport.Pong
		finished <- second.Query(t.Context(), transport.Ping{}, &result)
	}()
	readTestEvent(t, w)
	cancel()
	if err := <-finished; !errors.Is(err, errPeerClosed) {
		t.Fatalf("disconnected connection query error %v", err)
	}
	assertNoPendingQueries(t, second)
	if peer := <-disconnected; peer != second.transport {
		t.Fatal("request cancellation disconnected the wrong peer")
	}
	h.mx.RLock()
	_, exists := h.peers[string(key)]
	h.mx.RUnlock()
	if exists {
		t.Fatal("disconnected peer remains in the registry")
	}
}
