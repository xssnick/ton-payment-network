package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"strconv"
	"sync"
	"time"
)

type ChannelStatus uint8

const (
	ChannelStateInactive ChannelStatus = iota
	ChannelStateActive
	ChannelStateClosing
)

var ErrAlreadyExists = errors.New("already exists")
var ErrNotFound = errors.New("not found")

type Channel struct {
	ID           []byte
	Address      string
	Status       ChannelStatus
	WeLeft       bool
	OurOnchain   OnchainState
	TheirOnchain OnchainState

	KnownResolves map[string][]byte

	Our   Side
	Their Side

	// InitAt - initialization or reinitialization time
	InitAt    time.Time
	UpdatedAt time.Time
	CreatedAt time.Time

	mx sync.RWMutex
}

type OnchainState struct {
	Key            ed25519.PublicKey
	CommittedSeqno uint32
	WalletAddress  string
	Deposited      *big.Int
}

type Side struct {
	payments.SignedSemiChannel
}

func NewSide(channelId []byte, seqno uint64) Side {
	return Side{
		SignedSemiChannel: payments.SignedSemiChannel{
			Signature: payments.Signature{
				Value: make([]byte, 64),
			},
			State: payments.SemiChannel{
				ChannelID: channelId,
				Data: payments.SemiChannelBody{
					Seqno:        seqno,
					Sent:         tlb.ZeroCoins,
					Conditionals: cell.NewDict(32),
				},
			},
		},
	}
}

func (s *Side) IsReady() bool {
	return !bytes.Equal(s.Signature.Value, make([]byte, 64))
}

func (s *Side) UnmarshalJSON(bytes []byte) error {
	str, err := strconv.Unquote(string(bytes))
	if err != nil {
		return err
	}

	data, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return err
	}

	cl, err := cell.FromBOC(data)
	if err != nil {
		return err
	}

	return tlb.LoadFromCell(&s.SignedSemiChannel, cl.BeginParse())
}

func (s *Side) MarshalJSON() ([]byte, error) {
	bts, err := tlb.ToCell(s.SignedSemiChannel)
	if err != nil {
		return nil, err
	}
	return []byte(strconv.Quote(base64.StdEncoding.EncodeToString(bts.ToBOC()))), nil
}

func (ch *Channel) CalcBalance(isTheir bool) (*big.Int, error) {
	ch.mx.RLock()
	defer ch.mx.RUnlock()

	s1, s1chain, s2, s2chain := ch.Our, ch.OurOnchain, ch.Their, ch.TheirOnchain
	if isTheir {
		s1, s2 = s2, s1
		s1chain, s2chain = s2chain, s1chain
	}

	balance := new(big.Int).Add(s2.State.Data.Sent.Nano(), s1chain.Deposited)
	balance = balance.Sub(balance, s1.State.Data.Sent.Nano())

	for _, kv := range s1.State.Data.Conditionals.All() {
		// TODO: support other types of conditions
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to parse condition %d", kv.Key.BeginParse().MustLoadUInt(32))
		}
		balance = balance.Sub(balance, new(big.Int).Add(vch.Capacity, vch.Fee))
	}
	return balance, nil
}

func (ch *Channel) GetKnownResolve(key ed25519.PublicKey) *payments.VirtualChannelState {
	ch.mx.RLock()
	defer ch.mx.RUnlock()

	if ch.KnownResolves == nil {
		return nil
	}

	if val := ch.KnownResolves[hex.EncodeToString(key)]; val != nil {
		cll, err := cell.FromBOC(val)
		if err != nil {
			return nil
		}

		var st payments.VirtualChannelState
		if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
			return nil
		}

		if !st.Verify(key) {
			return nil
		}
		return &st
	}
	return nil
}

func (ch *Channel) RemoveKnownResolve(key ed25519.PublicKey) {
	ch.mx.Lock()
	defer ch.mx.Unlock()

	if ch.KnownResolves == nil {
		return
	}
	delete(ch.KnownResolves, hex.EncodeToString(key))
}

func (ch *Channel) AddKnownResolve(key ed25519.PublicKey, state *payments.VirtualChannelState) error {
	ch.mx.Lock()
	defer ch.mx.Unlock()

	if ch.KnownResolves == nil {
		ch.KnownResolves = map[string][]byte{}
	}

	if !state.Verify(key) {
		return fmt.Errorf("incorrect signature")
	}

	// TODO: check old value is newer

	cl, err := tlb.ToCell(state)
	if err != nil {
		return err
	}

	ch.KnownResolves[hex.EncodeToString(key)] = cl.ToBOC()
	return nil
}
