package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/rand"
	"sync"
	"time"
)

const _ChunkSize = 1 << 17
const _RLDPMaxAnswerSize = 2*_ChunkSize + 1024

type PeerConnection struct {
	rldp    *rldp.RLDP
	adnl    adnl.Peer
	authKey ed25519.PublicKey

	mx sync.Mutex
}

var ErrNotConnected = fmt.Errorf("not connected with peer")

type Service interface {
	ReviewChannelConfig(prop ProposeChannelConfig) (*address.Address, error)
	ProcessAction(ctx context.Context, key ed25519.PublicKey, lockId int64, channelAddr *address.Address, signedState payments.SignedSemiChannel, action Action, updateProof *cell.Cell) (*payments.SignedSemiChannel, error)
	ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action Action) ([]byte, error)
	ProcessExternalChannelLock(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64, lock bool) error
	ProcessIsChannelLocked(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64) error
}

type Server struct {
	svc        Service
	channelKey ed25519.PrivateKey
	key        ed25519.PrivateKey
	dht        *dht.Client
	gate       *adnl.Gateway
	closeCtx   context.Context

	peersByKey map[string]*PeerConnection
	peers      map[string]*PeerConnection
	mx         sync.RWMutex

	urgentPeers map[string]func()

	closer func()
}

func NewServer(dht *dht.Client, gate *adnl.Gateway, key, channelKey ed25519.PrivateKey, serverMode bool) *Server {
	s := &Server{
		channelKey:  channelKey,
		key:         key,
		dht:         dht,
		gate:        gate,
		peersByKey:  map[string]*PeerConnection{},
		peers:       map[string]*PeerConnection{},
		urgentPeers: map[string]func(){},
	}
	s.closeCtx, s.closer = context.WithCancel(context.Background())
	s.gate.SetConnectionHandler(s.bootstrapPeerWrap)

	if serverMode {
		go func() {
			updateFailed := false
			wait := 1 * time.Second
			// refresh dht records
			for {
				select {
				case <-s.closeCtx.Done():
					log.Info().Str("source", "server").Msg("stopped dht updater")
					return
				case <-time.After(wait):
				}

				log.Debug().Str("source", "server").Msg("updating our dht record")

				ctx, cancel := context.WithTimeout(s.closeCtx, 240*time.Second)
				err := s.updateDHT(ctx)
				cancel()

				if err != nil {
					updateFailed = true
					log.Warn().Err(err).Str("source", "server").Msg("failed to update our dht record, will retry in 5 sec")

					// on err, retry sooner
					wait = 5 * time.Second
					continue
				} else if updateFailed {
					updateFailed = false
					log.Info().Str("source", "server").Msg("dht record was successfully updated after retry")
				} else {
					log.Debug().Str("source", "server").Msg("dht record was successfully updated")
				}
				wait = 3 * time.Minute
			}
		}()
	}
	return s
}

func (s *Server) SetService(svc Service) {
	s.svc = svc
}

func (s *Server) updateDHT(ctx context.Context) error {
	addr := s.gate.GetAddressList()

	ctxStore, cancel := context.WithTimeout(ctx, 120*time.Second)
	stored, id, err := s.dht.StoreAddress(ctxStore, addr, 30*time.Minute, s.key, 0)
	cancel()
	if err != nil && stored == 0 {
		return err
	}

	chanKey := adnl.PublicKeyED25519{Key: s.channelKey.Public().(ed25519.PublicKey)}
	dhtVal, err := tl.Serialize(NodeAddress{
		ADNLAddr: id,
	}, true)
	if err != nil {
		return err
	}

	stored, _, err = s.dht.Store(ctx, chanKey, []byte("payment-node"), 0,
		dhtVal, dht.UpdateRuleSignature{}, 30*time.Minute, s.channelKey, 0)
	if err != nil {
		return fmt.Errorf("failed to store node payment-node value in dht: %w", err)
	}
	log.Debug().Str("source", "server").Int("copies", stored).Msg("our payment-node adnl address was updated in dht")

	return nil
}

func (s *Server) bootstrapPeerWrap(client adnl.Peer) error {
	s.bootstrapPeer(client)
	return nil
}

func (s *Server) bootstrapPeer(client adnl.Peer) *PeerConnection {
	s.mx.Lock()
	defer s.mx.Unlock()

	client.Reinit()

	if rl := s.peers[string(client.GetID())]; rl != nil {
		return rl
	}

	rl := rldp.NewClientV2(client)
	p := &PeerConnection{
		rldp: rl,
		adnl: client,
	}

	client.SetQueryHandler(s.handleADNLQuery(p))
	rl.SetOnQuery(s.handleRLDPQuery(p))

	client.SetDisconnectHandler(func(_ string, _ ed25519.PublicKey) {
		s.mx.Lock()
		if p.authKey != nil {
			log.Info().Str("key", base64.StdEncoding.EncodeToString(p.authKey)).Msg("peer disconnected")

			delete(s.peersByKey, string(p.authKey))
		}
		delete(s.peers, string(p.adnl.GetID()))
		s.mx.Unlock()
	})

	s.peers[string(client.GetID())] = p

	return p
}

func (s *Server) handleADNLQuery(peer *PeerConnection) func(query *adnl.MessageQuery) error {
	return func(query *adnl.MessageQuery) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		switch q := query.Data.(type) {
		case Ping:
			if err := peer.adnl.Answer(ctx, query.ID, Pong{Value: q.Value}); err != nil {
				return err
			}
		case Authenticate:
			if q.Timestamp < time.Now().Add(-30*time.Second).Unix() || q.Timestamp > time.Now().Unix() {
				return fmt.Errorf("outdated auth data")
			}

			// check signature with both adnl addresses, to protect from MITM attack
			authData, err := tl.Hash(AuthenticateToSign{
				A:         peer.adnl.GetID(),
				B:         s.gate.GetID(),
				Timestamp: q.Timestamp,
			})
			if err != nil {
				return fmt.Errorf("failed to hash their auth data: %w", err)
			}

			if !ed25519.Verify(q.Key, authData, q.Signature) {
				return fmt.Errorf("incorrect signature")
			}

			s.mx.Lock()
			if peer.authKey != nil {
				// when authenticated with new key, delete old record
				delete(s.peersByKey, string(peer.authKey))
			}
			peer.authKey = append([]byte{}, q.Key...)
			s.peersByKey[string(peer.authKey)] = peer
			s.mx.Unlock()
			log.Info().Str("key", base64.StdEncoding.EncodeToString(peer.authKey)).Msg("connected with payment node peer")

			// reverse A and B, and sign, so party can verify us too
			authData, err = tl.Hash(AuthenticateToSign{
				A:         s.gate.GetID(),
				B:         peer.adnl.GetID(),
				Timestamp: q.Timestamp,
			})
			if err != nil {
				return fmt.Errorf("failed to hash our auth data: %w", err)
			}

			if err = peer.adnl.Answer(ctx, query.ID, Authenticate{
				Key:       s.channelKey.Public().(ed25519.PublicKey),
				Timestamp: q.Timestamp,
				Signature: ed25519.Sign(s.channelKey, authData),
			}); err != nil {
				return err
			}
		case RequestChannelLock:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			var reason string
			err := s.svc.ProcessExternalChannelLock(ctx, peer.authKey, address.NewAddress(0, 0, q.ChannelAddr), q.LockID, q.Lock)
			if err != nil {
				reason = err.Error()
			}

			if err = peer.adnl.Answer(ctx, query.ID, Decision{Agreed: reason == "", Reason: reason}); err != nil {
				return err
			}
		case IsChannelUnlocked:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			var reason string
			err := s.svc.ProcessIsChannelLocked(ctx, peer.authKey, address.NewAddress(0, 0, q.ChannelAddr), q.LockID)
			if err != nil {
				reason = err.Error()
			}

			if err = peer.adnl.Answer(ctx, query.ID, Decision{Agreed: reason == "", Reason: reason}); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Server) handleRLDPQuery(peer *PeerConnection) func(transfer []byte, query *rldp.Query) error {
	return func(transfer []byte, query *rldp.Query) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		switch q := query.Data.(type) {
		case ProposeChannelConfig:
			var res ChannelConfigDecision
			if addr, err := s.svc.ReviewChannelConfig(q); err == nil {
				res.WalletAddr = addr.Data()
				res.Ok = true
			} else {
				res.Reason = err.Error()
			}

			if err := peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.Timeout, query.ID, transfer, res); err != nil {
				return err
			}
		case Ping:
			if err := peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.Timeout, query.ID, transfer, Pong{Value: q.Value}); err != nil {
				return err
			}
		case ProposeAction:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			var state payments.SignedSemiChannel
			if err := tlb.LoadFromCell(&state, q.SignedState.BeginParse()); err != nil {
				return fmt.Errorf("failed to parse channel state")
			}

			var updCell *cell.Cell
			ok := true
			reason := ""
			updateProof, err := s.svc.ProcessAction(ctx, peer.authKey, q.LockID,
				address.NewAddress(0, 0, q.ChannelAddr), state, q.Action, q.UpdateProof)
			if err != nil {
				reason = err.Error()
				ok = false
			} else {
				if updCell, err = tlb.ToCell(updateProof); err != nil {
					return fmt.Errorf("failed to serialize state cell: %w", err)
				}

				sk := cell.CreateProofSkeleton()
				if updateProof.State.CounterpartyData != nil {
					// include counterparty to proof (last ref)
					sk.ProofRef(int(updCell.RefsNum() - 1))
				}
				// prune conditionals, leave only hashes for optimization
				if updCell, err = updCell.CreateProof(sk); err != nil {
					return fmt.Errorf("failed to create proof from state cell: %w", err)
				}
				updCell = updCell.MustPeekRef(0)
			}

			if err = peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.Timeout, query.ID, transfer, ProposalDecision{Agreed: ok, Reason: reason, SignedState: updCell}); err != nil {
				return err
			}
		case RequestAction:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			ok := true
			reason := ""
			sign, err := s.svc.ProcessActionRequest(ctx, peer.authKey,
				address.NewAddress(0, 0, q.ChannelAddr), q.Action)
			if err != nil {
				reason = err.Error()
				ok = false
			}

			if err = peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.Timeout, query.ID, transfer, Decision{Agreed: ok, Reason: reason, Signature: sign}); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Server) AddUrgentPeer(channelKey ed25519.PublicKey) {
	s.mx.Lock()
	defer s.mx.Unlock()

	if s.urgentPeers[string(channelKey)] != nil {
		// already urgent
		return
	}

	peerCtx, cancel := context.WithCancel(s.closeCtx)
	s.urgentPeers[string(channelKey)] = cancel

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
			err := s.doADNLQuery(ctx, channelKey, Ping{Value: rand.Int63()}, &pong, true)
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

func (s *Server) RemoveUrgentPeer(channelKey ed25519.PublicKey) {
	s.mx.Lock()
	defer s.mx.Unlock()

	if fc := s.urgentPeers[string(channelKey)]; fc != nil {
		delete(s.urgentPeers, string(channelKey))
		// cancel peer connector
		fc()
		return
	}
}

func (s *Server) connect(ctx context.Context, channelKey ed25519.PublicKey) (*PeerConnection, error) {
	channelKeyId, err := tl.Hash(adnl.PublicKeyED25519{Key: channelKey})
	if err != nil {
		return nil, fmt.Errorf("failed to calc hash of channel key %s: %w", base64.StdEncoding.EncodeToString(channelKey), err)
	}

	dhtVal, _, err := s.dht.FindValue(ctx, &dht.Key{
		ID:    channelKeyId,
		Name:  []byte("payment-node"),
		Index: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find payment-node in dht of %s: %w", base64.StdEncoding.EncodeToString(channelKey), err)
	}

	var nodeAddr NodeAddress
	if _, err = tl.Parse(&nodeAddr, dhtVal.Data, true); err != nil {
		return nil, fmt.Errorf("failed to parse node dht value of %s: %w", base64.StdEncoding.EncodeToString(channelKey), err)
	}

	list, key, err := s.dht.FindAddresses(ctx, nodeAddr.ADNLAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to find address in dht of %s: %w", base64.StdEncoding.EncodeToString(channelKey), err)
	}

	if len(list.Addresses) == 0 {
		return nil, fmt.Errorf("no addresses for %s", base64.StdEncoding.EncodeToString(channelKey))
	}
	addr := fmt.Sprintf("%s:%d", list.Addresses[0].IP.String(), list.Addresses[0].Port)

	peer, err := s.gate.RegisterClient(addr, key)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to peer of %s at %s: %w", base64.StdEncoding.EncodeToString(channelKey), addr, err)
	}
	return s.bootstrapPeer(peer), nil
}

func (s *Server) auth(ctx context.Context, peer *PeerConnection) error {
	ts := time.Now().Unix()
	authData, err := tl.Hash(AuthenticateToSign{
		A:         s.gate.GetID(),
		B:         peer.adnl.GetID(),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash our auth data: %w", err)
	}

	var res Authenticate
	err = peer.adnl.Query(ctx, Authenticate{
		Key:       s.channelKey.Public().(ed25519.PublicKey),
		Timestamp: ts,
		Signature: ed25519.Sign(s.channelKey, authData),
	}, &res)
	if err != nil {
		return fmt.Errorf("failed to request auth: %w", err)
	}

	authData, err = tl.Hash(AuthenticateToSign{
		A:         peer.adnl.GetID(),
		B:         s.gate.GetID(),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash their auth data: %w", err)
	}

	if !ed25519.Verify(res.Key, authData, res.Signature) {
		return fmt.Errorf("incorrect response signature")
	}

	s.mx.Lock()
	if peer.authKey != nil {
		// when authenticated with new key, delete old record
		delete(s.peersByKey, string(peer.authKey))
	}
	peer.authKey = append([]byte{}, res.Key...)
	s.peersByKey[string(peer.authKey)] = peer
	s.mx.Unlock()
	log.Info().Str("key", base64.StdEncoding.EncodeToString(peer.authKey)).Msg("connected with payment node peer")

	return nil
}

func (s *Server) preparePeer(ctx context.Context, key []byte, connect bool) (peer *PeerConnection, err error) {
	if bytes.Equal(key, s.channelKey.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("cannot connect to ourself")
	}

	s.mx.RLock()
	peer = s.peersByKey[string(key)]
	s.mx.RUnlock()

	if peer == nil {
		if !connect {
			return nil, ErrNotConnected
		}

		if peer, err = s.connect(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to connect to peer: %w", err)
		}
	}

	peer.mx.Lock()
	defer peer.mx.Unlock()

	if peer.authKey == nil {
		if err = s.auth(ctx, peer); err != nil {
			return nil, fmt.Errorf("failed to auth peer: %w", err)
		}
	}

	return peer, nil
}

func (s *Server) ProposeChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey, prop ProposeChannelConfig) (*address.Address, error) {
	var res ChannelConfigDecision
	err := s.doRLDPQuery(ctx, theirChannelKey, prop, &res, true)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}

	if !res.Ok {
		return nil, fmt.Errorf("rejected: %s", res.Reason)
	}
	return address.NewAddress(0, 0, res.WalletAddr), nil
}

func (s *Server) RequestChannelLock(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64, lock bool) (*Decision, error) {
	var res Decision
	err := s.doADNLQuery(ctx, theirChannelKey, RequestChannelLock{
		LockID:      id,
		ChannelAddr: channel.Data(),
		Lock:        lock,
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to request lock: %w", err)
	}
	return &res, nil
}

func (s *Server) IsChannelUnlocked(ctx context.Context, theirChannelKey ed25519.PublicKey, channel *address.Address, id int64) (*Decision, error) {
	var res Decision
	err := s.doADNLQuery(ctx, theirChannelKey, IsChannelUnlocked{
		LockID:      id,
		ChannelAddr: channel.Data(),
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to request lock status: %w", err)
	}
	return &res, nil
}

func (s *Server) ProposeAction(ctx context.Context, lockId int64, channelAddr *address.Address, theirChannelKey []byte, state, updateProof *cell.Cell, action Action) (*ProposalDecision, error) {
	var res ProposalDecision
	err := s.doRLDPQuery(ctx, theirChannelKey, ProposeAction{
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

func (s *Server) RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action Action) (*Decision, error) {
	var res Decision
	err := s.doRLDPQuery(ctx, theirChannelKey, RequestAction{
		ChannelAddr: channelAddr.Data(),
		Action:      action,
	}, &res, false)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (s *Server) doRLDPQuery(ctx context.Context, theirKey []byte, req, resp tl.Serializable, connect bool) error {
	peer, err := s.preparePeer(ctx, theirKey, connect)
	if err != nil {
		return fmt.Errorf("failed to prepare peer %s: %w", hex.EncodeToString(theirKey), err)
	}

	var cancel func()
	dl, ok := ctx.Deadline()
	if !ok || dl.After(time.Now().Add(7*time.Second)) {
		ctx, cancel = context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
	}

	tm := time.Now()
	err = peer.rldp.DoQuery(ctx, _RLDPMaxAnswerSize, req, resp)
	if err != nil {
		// TODO: check other network cases too
		if time.Since(tm) > 3*time.Second {
			// drop peer to reconnect
			peer.adnl.Close()
		}
		return fmt.Errorf("failed to make rldp request: %w", err)
	}
	return nil
}

func (s *Server) doADNLQuery(ctx context.Context, theirKey []byte, req, resp tl.Serializable, connect bool) error {
	peer, err := s.preparePeer(ctx, theirKey, connect)
	if err != nil {
		return fmt.Errorf("failed to prepare peer: %w", err)
	}

	var cancel func()
	dl, ok := ctx.Deadline()
	if !ok || dl.After(time.Now().Add(7*time.Second)) {
		ctx, cancel = context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
	}

	tm := time.Now()
	err = peer.adnl.Query(ctx, req, resp)
	if err != nil {
		// TODO: check other network cases too
		if time.Since(tm) > 3*time.Second {
			// drop peer to reconnect
			peer.adnl.Close()
		}
		return fmt.Errorf("failed to make adnl request: %w", err)
	}
	return nil
}
