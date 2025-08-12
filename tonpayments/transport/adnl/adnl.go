//go:build !(js && wasm)

package adnl

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"sync"
	"sync/atomic"
	"time"
)

const _ChunkSize = 1 << 17
const _RLDPMaxAnswerSize = 2*_ChunkSize + 1024

type PeerConnection struct {
	rldp      *rldp.RLDP
	adnl      adnl.Peer
	transport *transport.Peer
	lastQuery int64

	mx sync.Mutex
}

type Service interface {
	ReviewChannelConfig(prop transport.ProposeChannelConfig) (*address.Address, config.CoinConfig, error)
	ProcessAction(ctx context.Context, key ed25519.PublicKey, lockId int64, channelAddr *address.Address, signedState payments.SignedSemiChannel, action transport.Action, updateProof *cell.Cell, fromWeb bool) (*payments.SignedSemiChannel, error)
	ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action transport.Action) ([]byte, error)
	ProcessExternalChannelLock(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64, lock bool) error
	ProcessIsChannelLocked(ctx context.Context, key ed25519.PublicKey, addr *address.Address, id int64) error
}

type Server struct {
	key               ed25519.PrivateKey
	channelKey        ed25519.PrivateKey
	dht               *dht.Client
	gate              *adnl.Gateway
	closeCtx          context.Context
	queryHandler      func(ctx context.Context, from *transport.Peer, msg any) (any, error)
	disconnectHandler func(ctx context.Context, from *transport.Peer) error

	peers map[string]*PeerConnection
	mx    sync.RWMutex

	closer func()
}

func NewServer(dht *dht.Client, gate *adnl.Gateway, key, channelKey ed25519.PrivateKey, serverMode bool) *Server {
	s := &Server{
		channelKey: channelKey,
		key:        key,
		dht:        dht,
		gate:       gate,
		peers:      map[string]*PeerConnection{},
	}
	s.closeCtx, s.closer = context.WithCancel(context.Background())
	s.gate.SetConnectionHandler(s.bootstrapPeerWrap)

	go func() {
		for {
			select {
			case <-s.closeCtx.Done():
				log.Info().Str("source", "server").Msg("stopped peers cleaner")
				return
			case <-time.After(5 * time.Second):
				s.mx.Lock()
				for id, p := range s.peers {
					if time.Since(time.Unix(p.lastQuery, 0)) > 5*time.Minute {
						delete(s.peers, id)
						p.adnl.Close()

						log.Debug().Str("source", "server").
							Str("peer", base64.StdEncoding.EncodeToString(p.adnl.GetID())).
							Msg("peer was not queried for 10 minutes, will be disconnected")
					}
				}
				s.mx.Unlock()
			}
		}
	}()

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

func (s *Server) GetOurID() []byte {
	return s.gate.GetID()
}

func (s *Server) SetHandlers(q func(ctx context.Context, from *transport.Peer, msg any) (any, error), d func(ctx context.Context, from *transport.Peer) error) {
	s.queryHandler = q
	s.disconnectHandler = d
}

func (s *Server) updateDHT(ctx context.Context) error {
	addr := s.gate.GetAddressList()

	ctxStore, cancel := context.WithTimeout(ctx, 120*time.Second)
	stored, id, err := s.dht.StoreAddress(ctxStore, addr, 30*time.Minute, s.key, 3)
	cancel()
	if err != nil && stored == 0 {
		return err
	}

	chanKey := keys.PublicKeyED25519{Key: s.channelKey.Public().(ed25519.PublicKey)}
	dhtVal, err := tl.Serialize(transport.NodeAddress{
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

func (s *Server) bootstrapPeer(client adnl.Peer) *transport.Peer {
	s.mx.Lock()
	defer s.mx.Unlock()

	client.Reinit()

	if rl := s.peers[string(client.GetID())]; rl != nil {
		return rl.transport
	}

	rl := rldp.NewClientV2(client)
	p := &PeerConnection{
		rldp:      rl,
		adnl:      client,
		lastQuery: time.Now().Unix(),
	}
	p.transport = &transport.Peer{
		ID:   client.GetID(),
		Conn: p,
	}

	client.SetQueryHandler(s.handleADNLQuery(p))
	rl.SetOnQuery(s.handleRLDPQuery(p))

	client.SetDisconnectHandler(func(_ string, _ ed25519.PublicKey) {
		s.mx.Lock()
		delete(s.peers, string(p.adnl.GetID()))
		s.mx.Unlock()

		_ = s.disconnectHandler(s.closeCtx, p.transport)
	})

	s.peers[string(client.GetID())] = p

	return p.transport
}

func (s *Server) handleADNLQuery(peer *PeerConnection) func(query *adnl.MessageQuery) error {
	return func(query *adnl.MessageQuery) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		atomic.StoreInt64(&peer.lastQuery, time.Now().Unix())

		res, err := s.queryHandler(ctx, peer.transport, query.Data)
		if err != nil {
			return fmt.Errorf("failed to handle query: %w", err)
		}

		return peer.adnl.Answer(ctx, query.ID, res)
	}
}

func (s *Server) handleRLDPQuery(peer *PeerConnection) func(transfer []byte, query *rldp.Query) error {
	return func(transfer []byte, query *rldp.Query) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		atomic.StoreInt64(&peer.lastQuery, time.Now().Unix())

		res, err := s.queryHandler(ctx, peer.transport, query.Data)
		if err != nil {
			return fmt.Errorf("failed to handle query: %w", err)
		}

		return peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.Timeout, query.ID, transfer, res)
	}
}

func (s *Server) Stop() {
	s.closer()
}

func (s *Server) Connect(ctx context.Context, channelKey ed25519.PublicKey) (*transport.Peer, error) {
	channelKeyId, err := tl.Hash(keys.PublicKeyED25519{Key: channelKey})
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

	var nodeAddr transport.NodeAddress
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

func (p *PeerConnection) Query(ctx context.Context, req, resp tl.Serializable) error {
	tm := time.Now()

	switch req.(type) {
	case transport.ProposeAction, transport.RequestAction, transport.ProposeChannelConfig:
		if err := p.rldp.DoQuery(ctx, _RLDPMaxAnswerSize, req, resp); err != nil {
			// TODO: check other network cases too
			if time.Since(tm) > 8*time.Second {
				// drop peer to reconnect
				p.adnl.Close()
			}
			return fmt.Errorf("failed to make rldp request: %w", err)
		}
	default:
		if err := p.adnl.Query(ctx, req, resp); err != nil {
			// TODO: check other network cases too
			if time.Since(tm) > 3*time.Second {
				// drop peer to reconnect
				p.adnl.Close()
			}
			return fmt.Errorf("failed to make adnl request: %w", err)
		}
	}
	atomic.StoreInt64(&p.lastQuery, time.Now().Unix())

	return nil
}
