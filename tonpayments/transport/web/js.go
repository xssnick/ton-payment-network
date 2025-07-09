//go:build js && wasm

package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/tl"
	"reflect"
	"sync"
	"syscall/js"
	"time"
)

type PeerConnection struct {
	serverKey ed25519.PublicKey
	key       ed25519.PrivateKey
	transport *transport.Peer
	close     func()

	mx sync.RWMutex
}

type HTTP struct {
	key               ed25519.PrivateKey
	serverChannelKey  ed25519.PublicKey
	serverPeerKey     ed25519.PublicKey
	queryHandler      func(ctx context.Context, from *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, from *transport.Peer) error

	peer *PeerConnection
	ton  *client.TON

	mx sync.RWMutex
}

func NewHTTP(ton *client.TON, key ed25519.PrivateKey, serverChannelKey, serverPeerKey ed25519.PublicKey) *HTTP {
	return &HTTP{
		key:              key,
		serverPeerKey:    serverPeerKey,
		serverChannelKey: serverChannelKey,
		ton:              ton,
	}
}

func (h *HTTP) GetOurID() []byte {
	return h.key.Public().(ed25519.PublicKey)
}

func (h *HTTP) Connect(ctx context.Context, channelKey ed25519.PublicKey) (*transport.Peer, error) {
	if !bytes.Equal(channelKey, h.serverChannelKey) {
		return nil, fmt.Errorf("incorrect server channel key, from web you can connect only to single server peer")
	}

	h.mx.Lock()
	defer h.mx.Unlock()

	if h.peer != nil {
		return h.peer.transport, nil
	}

	t := &transport.Peer{
		ID: h.serverPeerKey,
	}
	peer := &PeerConnection{
		serverKey: h.serverPeerKey,
		key:       h.key,
		transport: t,
	}
	t.Conn = peer

	ts, token, err := peer.authSSE(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to authenticate sse: %w", err)
	}
	// TODO: verify server signature?

	connCh := make(chan struct{})
	var connected bool
	url := fmt.Sprintf("/web-channel/api/v1/subscribe?timestamp=%d&token=%s&key=%s", ts, token, base64.URLEncoding.EncodeToString(h.GetOurID()))
	stop, err := h.subscribeSSE(ctx, url, func(msg []byte) {
		var e Event
		if err = json.Unmarshal(msg, &e); err != nil {
			log.Error().Err(err).Msg("event json parse err")
			return
		}

		if len(e.Data) == 0 {
			if !connected {
				h.peer = peer
				connected = true
				close(connCh)
			}
			return
		}

		var val tl.Serializable
		if _, err = tl.Parse(&val, e.Data, true); err != nil {
			log.Error().Err(err).Msg("event tl parse err")
			return
		}

		res, err := h.queryHandler(ctx, peer.transport, val)
		if err != nil {
			log.Error().Err(err).Msg("query handler err")
			return
		}

		resp, err := tl.Serialize(res, true)
		if err != nil {
			log.Error().Err(err).Msg("tl serialize err")
			return
		}

		_, err = peer.pushJSON(ctx, "/web-channel/api/v1/push", Event{Key: h.GetOurID(), Data: resp, QueryID: e.QueryID})
		if err != nil {
			log.Error().Err(err).Msg("push response err")
			return
		}
	}, func() {
		if connected {
			h.peer = nil
			_ = h.disconnectHandler(context.Background(), peer.transport)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe: %w", err)
	}
	peer.close = stop

	select {
	case <-connCh:
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	}

	return peer.transport, nil
}

func (h *HTTP) SetHandlers(q func(ctx context.Context, peer *transport.Peer, msg any) (any, error), d func(ctx context.Context, peer *transport.Peer) error) {
	h.queryHandler = q
	h.disconnectHandler = d
}

func (p *PeerConnection) Query(ctx context.Context, msg, res tl.Serializable) error {
	req, err := tl.Serialize(msg, true)
	if err != nil {
		return err
	}

	resBytes, err := p.pushJSON(ctx, "/web-channel/api/v1/push", Event{Key: p.key.Public().(ed25519.PublicKey), Data: req})
	if err != nil {
		return err
	}

	var ev Event
	if err = json.Unmarshal(resBytes, &ev); err != nil {
		return err
	}

	var val tl.Serializable
	if _, err = tl.Parse(&val, ev.Data, true); err != nil {
		return err
	}

	reflect.ValueOf(res).Elem().Set(reflect.ValueOf(val))
	return nil
}

func (p *PeerConnection) authSSE(ctx context.Context) (int64, string, error) {
	ts := time.Now().UTC().Unix()
	sig := ed25519.Sign(p.key, []byte(fmt.Sprintf("web:%d", ts)))

	resBytes, err := p.pushJSON(ctx, "/web-channel/api/v1/subscribe/auth", SubscribeAuth{PeerKey: p.key.Public().(ed25519.PublicKey), Timestamp: ts, Signature: sig})
	if err != nil {
		return 0, "", err
	}

	var sub SubscribeAuthResult
	if err = json.Unmarshal(resBytes, &sub); err != nil {
		return 0, "", err
	}

	token, err := base64.URLEncoding.DecodeString(sub.Token)
	if err != nil {
		return 0, "", err
	}

	d := []byte(fmt.Sprintf("subscribe:%d:%s", ts, base64.URLEncoding.EncodeToString(p.key.Public().(ed25519.PublicKey))))
	if !ed25519.Verify(p.serverKey, d, token) {
		return 0, "", fmt.Errorf("invalid server signature")
	}

	return ts, sub.Token, nil
}

func await(p js.Value) (js.Value, error) {
	ch := make(chan struct{})
	var res js.Value
	var err error

	then := js.FuncOf(func(this js.Value, args []js.Value) any {
		res = args[0]
		close(ch)
		return nil
	})
	defer then.Release()

	catch := js.FuncOf(func(this js.Value, args []js.Value) any {
		err = errors.New(args[0].String())
		close(ch)
		return nil
	})
	defer catch.Release()

	p.Call("then", then).Call("catch", catch)
	<-ch

	return res, err
}

func (p *PeerConnection) pushJSON(ctx context.Context, url string, body any) ([]byte, error) {
	reqData, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	fetch := js.Global().Get("fetch")
	if fetch.IsUndefined() {
		return nil, fmt.Errorf("fetch not supported")
	}

	headers := js.Global().Get("Object").New()
	headers.Set("Content-Type", "application/json")

	opts := map[string]any{
		"method":  "POST",
		"body":    string(reqData),
		"headers": headers,
	}

	fetchReq := fetch.Invoke(url, opts)
	resp, err := await(fetchReq)
	if err != nil {
		return nil, err
	}

	if !resp.Get("ok").Bool() {
		txt, _ := await(resp.Call("text"))
		return nil, fmt.Errorf("http %d: %s", resp.Get("status").Int(), txt.String())
	}

	jsData, err := await(resp.Call("json"))
	if err != nil {
		return nil, fmt.Errorf("json parse: %w", err)
	}

	str := js.Global().Get("JSON").Call("stringify", jsData).String()
	return []byte(str), nil
}

func (h *HTTP) subscribeSSE(ctx context.Context, url string, onMsg func(msg []byte), onDisconnect func()) (func(), error) {
	eventSourceConstructor := js.Global().Get("EventSource")
	if eventSourceConstructor.IsUndefined() {
		return nil, fmt.Errorf("EventSource not supported")
	}
	eventSource := eventSourceConstructor.New(url)

	onMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		event := args[0]
		data := event.Get("data").String()
		onMsg([]byte(data))
		return nil
	})

	eventSource.Call("addEventListener", "message", onMessage)

	// TODO: better and close peer
	onError := js.FuncOf(func(this js.Value, args []js.Value) any {
		js.Global().Get("console").Call("log", args[0])

		log.Error().Msg("sse err")
		return nil
	})
	eventSource.Call("addEventListener", "error", onError)

	cancel := func() {
		onMessage.Release()
		onError.Release()
		eventSource.Call("close")
		onDisconnect()
	}

	return cancel, nil
}
