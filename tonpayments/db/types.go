package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
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

type VirtualChannelMeta struct {
	Key                 []byte
	Active              bool
	FromChannelAddress  string
	ToChannelAddress    string
	LastKnownResolve    []byte
	ReadyToReleaseCoins bool
	CreatedAt           time.Time
}

type Channel struct {
	ID                     []byte
	Address                string
	Status                 ChannelStatus
	WeLeft                 bool
	OurOnchain             OnchainState
	TheirOnchain           OnchainState
	SafeOnchainClosePeriod int64

	AcceptingActions bool

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

var ErrNewerStateIsKnown = errors.New("newer state is already known")

func NewSide(channelId []byte, seqno, counterpartySeqno uint64) Side {
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
				CounterpartyData: &payments.SemiChannelBody{
					Seqno:        counterpartySeqno,
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

func (s *Side) Copy() *Side {
	sd := &Side{
		SignedSemiChannel: payments.SignedSemiChannel{
			Signature: payments.Signature{
				Value: append([]byte{}, s.Signature.Value...),
			},
			State: payments.SemiChannel{
				ChannelID: append([]byte{}, s.State.ChannelID...),
				Data: payments.SemiChannelBody{
					Seqno:        s.State.Data.Seqno,
					Sent:         s.State.Data.Sent,
					Conditionals: cell.NewDict(32),
				},
			},
		},
	}

	for _, kv := range s.State.Data.Conditionals.All() {
		_ = sd.State.Data.Conditionals.Set(kv.Key, kv.Value)
	}

	if s.State.CounterpartyData != nil {
		sd.State.CounterpartyData = &payments.SemiChannelBody{
			Seqno:        s.State.CounterpartyData.Seqno,
			Sent:         s.State.CounterpartyData.Sent,
			Conditionals: cell.NewDict(32),
		}

		for _, kv := range s.State.CounterpartyData.Conditionals.All() {
			_ = sd.State.CounterpartyData.Conditionals.Set(kv.Key, kv.Value)
		}
	}

	return sd
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

func (ch *VirtualChannelMeta) GetKnownResolve(key ed25519.PublicKey) *payments.VirtualChannelState {
	if ch.LastKnownResolve == nil {
		return nil
	}

	cll, err := cell.FromBOC(ch.LastKnownResolve)
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

func (ch *VirtualChannelMeta) AddKnownResolve(key ed25519.PublicKey, state *payments.VirtualChannelState) error {
	if !state.Verify(key) {
		return fmt.Errorf("incorrect signature")
	}

	if ch.LastKnownResolve != nil {
		cl, err := cell.FromBOC(ch.LastKnownResolve)
		if err != nil {
			return err
		}

		var oldState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&oldState, cl.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse old start: %w", err)
		}

		if oldState.Amount.Nano().Cmp(state.Amount.Nano()) == 1 {
			return ErrNewerStateIsKnown
		}
	}

	cl, err := tlb.ToCell(state)
	if err != nil {
		return err
	}

	ch.LastKnownResolve = cl.ToBOC()
	return nil
}
