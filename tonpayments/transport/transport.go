package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"time"
)

type NetworkProvider interface {
	GetOurID() []byte
	Connect(ctx context.Context, channelKey ed25519.PublicKey) (*Peer, error)
	SetHandlers(q func(ctx context.Context, peer *Peer, msg any) (any, error), d func(ctx context.Context, peer *Peer) error)
}

type PeerConnection interface {
	Query(ctx context.Context, msg, res tl.Serializable) error
}

type Peer struct {
	ID      []byte
	AuthKey ed25519.PublicKey
	Conn    PeerConnection

	mx sync.Mutex
}

type Service interface {
	ReviewChannelConfig(prop ProposeChannelConfig) (*address.Address, config.CoinConfig, error)
	ProcessAction(ctx context.Context, key ed25519.PublicKey, lockId int64, channelAddr *address.Address, signedState payments.SignedSemiChannel, action Action, updateProof *cell.Cell, fromWeb bool) (*payments.SignedSemiChannel, error)
	ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action Action) ([]byte, error)
	ProcessExternalChannelLock(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64, lock bool) error
	ProcessIsChannelLocked(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64) error
}

type Transport struct {
	web        bool
	svc        Service
	net        NetworkProvider
	channelKey ed25519.PrivateKey

	peersByKey map[string]*Peer

	urgentPeers map[string]func()

	closeCtx context.Context
	closer   func()

	mx sync.RWMutex
}

func NewTransport(channelKey ed25519.PrivateKey, net NetworkProvider, web bool) *Transport {
	s := &Transport{
		web:         web,
		net:         net,
		channelKey:  channelKey,
		peersByKey:  map[string]*Peer{},
		urgentPeers: map[string]func(){},
	}
	s.closeCtx, s.closer = context.WithCancel(context.Background())
	net.SetHandlers(s.handleQuery, s.handleDisconnect)
	return s
}

func (t *Transport) SetService(svc Service) {
	t.svc = svc
}

func (t *Transport) handleDisconnect(ctx context.Context, p *Peer) error {
	t.mx.Lock()
	defer t.mx.Unlock()

	if t.peersByKey[string(p.AuthKey)] == p {
		delete(t.peersByKey, string(p.AuthKey))
		log.Info().Str("key", base64.StdEncoding.EncodeToString(p.AuthKey)).Msg("peer disconnected")
	}

	return nil
}

func (t *Transport) handleQuery(ctx context.Context, peer *Peer, msg any) (any, error) {
	switch q := msg.(type) {
	case Ping:
		return Pong{Value: q.Value}, nil
	case Authenticate:
		if q.Timestamp < time.Now().Add(-30*time.Second).Unix() || q.Timestamp > time.Now().UTC().Unix() {
			return nil, fmt.Errorf("outdated auth data")
		}

		// check signature with both adnl addresses, to protect from MITM attack
		authData, err := tl.Hash(AuthenticateToSign{
			A:         peer.ID,
			B:         t.net.GetOurID(),
			Timestamp: q.Timestamp,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to hash their auth data: %w", err)
		}

		if !ed25519.Verify(q.Key, authData, q.Signature) {
			return nil, fmt.Errorf("incorrect signature")
		}

		if !bytes.Equal(q.Key, peer.AuthKey) {
			log.Info().Str("key", base64.StdEncoding.EncodeToString(q.Key)).Msg("connected with payment node peer")

			t.mx.Lock()
			if peer.AuthKey != nil {
				// when authenticated with new key, delete old record
				delete(t.peersByKey, string(peer.AuthKey))
			}
			peer.AuthKey = append([]byte{}, q.Key...)
			t.peersByKey[string(peer.AuthKey)] = peer
			t.mx.Unlock()
		}

		// reverse A and B, and sign, so party can verify us too
		authData, err = tl.Hash(AuthenticateToSign{
			A:         t.net.GetOurID(),
			B:         peer.ID,
			Timestamp: q.Timestamp,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to hash our auth data: %w", err)
		}

		return Authenticate{
			Key:       t.channelKey.Public().(ed25519.PublicKey),
			Timestamp: q.Timestamp,
			Signature: ed25519.Sign(t.channelKey, authData),
		}, nil
	case RequestChannelLock:
		if peer.AuthKey == nil {
			return nil, fmt.Errorf("not authorized")
		}

		var reason string
		err := t.svc.ProcessExternalChannelLock(ctx, peer.AuthKey, address.NewAddress(0, 0, q.ChannelAddr), q.LockID, q.Lock)
		if err != nil {
			reason = err.Error()
		}

		return Decision{Agreed: reason == "", Reason: reason}, nil
	case IsChannelUnlocked:
		if peer.AuthKey == nil {
			return nil, fmt.Errorf("not authorized")
		}

		var reason string
		err := t.svc.ProcessIsChannelLocked(ctx, peer.AuthKey, address.NewAddress(0, 0, q.ChannelAddr), q.LockID)
		if err != nil {
			reason = err.Error()
		}

		return Decision{Agreed: reason == "", Reason: reason}, nil
	case ProposeChannelConfig:
		var res ChannelConfigDecision
		if addr, cc, err := t.svc.ReviewChannelConfig(q); err == nil {
			res.WalletAddr = addr.Data()

			res.ProxyAllowed = cc.VirtualTunnelConfig.AllowTunneling
			if res.ProxyAllowed {
				res.ProxyMaxCap = tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMaxCapacity, int(cc.Decimals)).Nano().Bytes()
				res.ProxyMinFee = tlb.MustFromDecimal(cc.VirtualTunnelConfig.ProxyMinFee, int(cc.Decimals)).Nano().Bytes()
				res.ProxyPercentFeeFloat = math.Float64bits(cc.VirtualTunnelConfig.ProxyFeePercent)
			}

			res.Ok = true
		} else {
			res.Reason = err.Error()
		}

		return res, nil
	case ProposeAction:
		if peer.AuthKey == nil {
			return nil, fmt.Errorf("not authorized")
		}

		var state payments.SignedSemiChannel
		if err := tlb.LoadFromCell(&state, q.SignedState.BeginParse()); err != nil {
			return nil, fmt.Errorf("failed to parse channel state")
		}

		var updCell *cell.Cell
		ok := true
		reason := ""
		updateProof, err := t.svc.ProcessAction(ctx, peer.AuthKey, q.LockID,
			address.NewAddress(0, 0, q.ChannelAddr), state, q.Action, q.UpdateProof, t.web)
		if err != nil {
			reason = err.Error()
			ok = false
			log.Debug().Str("reason", reason).Msg("failed to process action")
		} else {
			if updCell, err = tlb.ToCell(updateProof); err != nil {
				return nil, fmt.Errorf("failed to serialize state cell: %w", err)
			}

			sk := cell.CreateProofSkeleton()
			if updateProof.State.CounterpartyData != nil {
				// include counterparty to proof (last ref)
				sk.ProofRef(int(updCell.RefsNum() - 1))
			}
			// prune conditionals, leave only hashes for optimization
			if updCell, err = updCell.CreateProof(sk); err != nil {
				return nil, fmt.Errorf("failed to create proof from state cell: %w", err)
			}
			updCell = updCell.MustPeekRef(0)
		}

		return ProposalDecision{Agreed: ok, Reason: reason, SignedState: updCell}, nil
	case RequestAction:
		if peer.AuthKey == nil {
			return nil, fmt.Errorf("not authorized")
		}

		ok := true
		reason := ""
		sign, err := t.svc.ProcessActionRequest(ctx, peer.AuthKey,
			address.NewAddress(0, 0, q.ChannelAddr), q.Action)
		if err != nil {
			reason = err.Error()
			ok = false
		}

		return Decision{Agreed: ok, Reason: reason, Signature: sign}, nil
	}

	return nil, fmt.Errorf("unknown query")
}

func (t *Transport) AddUrgentPeer(channelKey ed25519.PublicKey) {
	t.mx.Lock()
	defer t.mx.Unlock()

	if t.urgentPeers[string(channelKey)] != nil {
		// already urgent
		return
	}

	peerCtx, cancel := context.WithCancel(t.closeCtx)
	t.urgentPeers[string(channelKey)] = cancel

	go func() {
		var wait time.Duration = 0
		var timeout = 15 * time.Second
		for {
			select {
			case <-peerCtx.Done():
				log.Debug().Str("key", base64.StdEncoding.EncodeToString(channelKey)).Msg("closing urgent peer")
				return
			case <-time.After(wait):
			}

			start := time.Now()

			log.Debug().Str("key", base64.StdEncoding.EncodeToString(channelKey)).Msg("pinging urgent peer...")

			var pong Pong
			ctx, cancel := context.WithTimeout(peerCtx, timeout)
			err := t.doQuery(ctx, channelKey, Ping{Value: rand.Int63()}, &pong, true)
			cancel()
			if err != nil {
				timeout = 10 * time.Second
				wait = 3 * time.Second
				log.Warn().Err(err).Str("key", base64.StdEncoding.EncodeToString(channelKey)).Msg("failed to ping urgent peer, retrying in 3s")
				continue
			}

			timeout = 7 * time.Second
			wait = 10 * time.Second
			log.Debug().Str("key", base64.StdEncoding.EncodeToString(channelKey)).Dur("ping_ms", time.Since(start).Round(time.Millisecond)).Msg("urgent peer successfully pinged")
		}
	}()
}

func (t *Transport) Stop() {
	t.closer()
}

func (t *Transport) RemoveUrgentPeer(channelKey ed25519.PublicKey) {
	t.mx.Lock()
	defer t.mx.Unlock()

	if fc := t.urgentPeers[string(channelKey)]; fc != nil {
		delete(t.urgentPeers, string(channelKey))
		// cancel peer connector
		fc()
		return
	}
}

func (t *Transport) auth(ctx context.Context, peer *Peer) error {
	ts := time.Now().UTC().Unix()
	authData, err := tl.Hash(AuthenticateToSign{
		A:         t.net.GetOurID(),
		B:         peer.ID,
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash our auth data: %w", err)
	}

	var res Authenticate
	err = peer.Conn.Query(ctx, Authenticate{
		Key:       t.channelKey.Public().(ed25519.PublicKey),
		Timestamp: ts,
		Signature: ed25519.Sign(t.channelKey, authData),
	}, &res)
	if err != nil {
		return fmt.Errorf("failed to request auth: %w", err)
	}

	authData, err = tl.Hash(AuthenticateToSign{
		A:         peer.ID,
		B:         t.net.GetOurID(),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash their auth data: %w", err)
	}

	if !ed25519.Verify(res.Key, authData, res.Signature) {
		return fmt.Errorf("incorrect response signature")
	}

	t.mx.Lock()
	if peer.AuthKey != nil {
		// when authenticated with new key, delete old record
		delete(t.peersByKey, string(peer.AuthKey))
	}
	peer.AuthKey = append([]byte{}, res.Key...)
	t.peersByKey[string(peer.AuthKey)] = peer
	t.mx.Unlock()
	log.Info().Str("key", base64.StdEncoding.EncodeToString(peer.AuthKey)).Msg("connected with payment node peer")

	return nil
}

func (t *Transport) preparePeer(ctx context.Context, key []byte, connect bool) (peer *Peer, err error) {
	if bytes.Equal(key, t.channelKey.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("cannot connect to ourself")
	}

	t.mx.RLock()
	peer = t.peersByKey[string(key)]
	t.mx.RUnlock()

	if peer == nil {
		if !connect {
			return nil, ErrNotConnected
		}

		if peer, err = t.net.Connect(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to connect to peer: %w", err)
		}
	}

	peer.mx.Lock()
	defer peer.mx.Unlock()

	if peer.AuthKey == nil {
		if err = t.auth(ctx, peer); err != nil {
			return nil, fmt.Errorf("failed to auth peer: %w", err)
		}
	}

	return peer, nil
}

func (t *Transport) ProposeChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey, prop ProposeChannelConfig) (*address.Address, VirtualConfigResponse, error) {
	var res ChannelConfigDecision
	err := t.doQuery(ctx, theirChannelKey, prop, &res, true)
	if err != nil {
		return nil, VirtualConfigResponse{}, fmt.Errorf("failed to make request: %w", err)
	}

	if !res.Ok {
		return nil, VirtualConfigResponse{}, fmt.Errorf("rejected: %s", res.Reason)
	}

	cfg := VirtualConfigResponse{
		ProxyFeePercent: math.Float64frombits(res.ProxyPercentFeeFloat),
		AllowTunneling:  res.ProxyAllowed,
	}

	if res.ProxyAllowed {
		if len(res.ProxyMinFee) > 32 || len(res.ProxyMaxCap) > 32 {
			return nil, VirtualConfigResponse{}, fmt.Errorf("invalid proxy config")
		}
		
		cfg.ProxyMinFee = new(big.Int).SetBytes(res.ProxyMinFee)
		cfg.ProxyMaxCapacity = new(big.Int).SetBytes(res.ProxyMaxCap)
	}

	// TODO: check proxy fees and config
	return address.NewAddress(0, 0, res.WalletAddr), cfg, nil
}

func (t *Transport) RequestChannelLock(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64, lock bool) (*Decision, error) {
	var res Decision
	err := t.doQuery(ctx, theirChannelKey, RequestChannelLock{
		LockID:      id,
		ChannelAddr: channel.Data(),
		Lock:        lock,
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to request lock: %w", err)
	}
	return &res, nil
}

func (t *Transport) IsChannelUnlocked(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64) (*Decision, error) {
	var res Decision
	err := t.doQuery(ctx, theirChannelKey, IsChannelUnlocked{
		LockID:      id,
		ChannelAddr: channel.Data(),
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to request lock status: %w", err)
	}
	return &res, nil
}

func (t *Transport) ProposeAction(ctx context.Context, lockId int64, channelAddr *address.Address, theirChannelKey []byte, state, updateProof *cell.Cell, action Action) (*ProposalDecision, error) {
	var res ProposalDecision
	err := t.doQuery(ctx, theirChannelKey, ProposeAction{
		LockID:      lockId,
		ChannelAddr: channelAddr.Data(),
		Action:      action,
		SignedState: state,
		UpdateProof: updateProof,
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (t *Transport) RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action Action) (*Decision, error) {
	var res Decision
	err := t.doQuery(ctx, theirChannelKey, RequestAction{
		ChannelAddr: channelAddr.Data(),
		Action:      action,
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (t *Transport) doQuery(ctx context.Context, theirKey []byte, req, resp tl.Serializable, connect bool) error {
	maxWait := 7 * time.Second
	if connect {
		maxWait = 30 * time.Second
	}

	if dl, ok := ctx.Deadline(); !ok || dl.After(time.Now().Add(maxWait)) {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, maxWait)
		defer cancel()
	}

	peer, err := t.preparePeer(ctx, theirKey, connect)
	if err != nil {
		return fmt.Errorf("failed to prepare peer %s: %w", base64.StdEncoding.EncodeToString(theirKey), err)
	}

	err = peer.Conn.Query(ctx, req, resp)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	return nil
}
