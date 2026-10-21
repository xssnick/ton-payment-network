package db

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type VirtualChannelEventType string

const (
	VirtualChannelEventTypeOpen   VirtualChannelEventType = "open"
	VirtualChannelEventTypeClose  VirtualChannelEventType = "close"
	VirtualChannelEventTypeRemove VirtualChannelEventType = "remove"
)

type VirtualChannelEvent struct {
	EventType      VirtualChannelEventType `json:"event_type"`
	VirtualChannel any                     `json:"virtual_channel"`
}

type ChannelHistoryActionTransferInData struct {
	Amounts map[string]string
	From    []byte
}

type ChannelHistoryActionTransferOutData struct {
	Amounts map[string]string
	To      []byte
}

type ChannelHistoryActionAmountData struct {
	IsTheir bool
	Amounts map[string]string
}

type ChannelHistoryActionRentCapData struct {
	BalanceID string
	Amount    string
	Fee       string
	Till      int64
}

type ChannelHistoryActionTxRequest struct {
	Fees map[string]string
}

type ChannelStatus uint8
type ConditionalStatus uint8
type ChannelHistoryEventType uint8

const (
	ChannelStateInactive ChannelStatus = iota
	ChannelStateActive
	ChannelStateClosing
	ChannelStateAny ChannelStatus = 100
)

const (
	ChannelHistoryActionBalanceChanged ChannelHistoryEventType = iota + 1
	ChannelHistoryActionTransferIn
	ChannelHistoryActionTransferOut
	ChannelHistoryActionUncooperativeCloseStarted
	ChannelHistoryActionClosed
	ChannelHistoryActionTheirCapacityRented
	ChannelHistoryActionOurCapacityRented
	ChannelHistoryActionWithdrawTransactionRequest
)

const (
	ConditionalStateActive ConditionalStatus = iota + 1
	ConditionalStateWantClose
	ConditionalStateClosed
	ConditionalStateWantRemove
	ConditionalStateRemoved
	ConditionalStatePending
)

var ErrAlreadyExists = errors.New("already exists")
var ErrNotFound = errors.New("not found")
var ErrChannelBusy = fmt.Errorf("channel is busy")

type ConditionalMetaSide struct {
	ChannelAddress        string
	Conditional           *cell.Cell
	UncooperativeDeadline time.Time
	SafeDeadline          time.Time
	SenderKey             ed25519.PublicKey
	LinkedKey             ed25519.PublicKey
}

type ConditionalMeta struct {
	Key              []byte
	Status           ConditionalStatus
	Incoming         *ConditionalMetaSide
	Outgoing         *ConditionalMetaSide
	LastKnownResolve *cell.Cell
	FinalDestination ed25519.PublicKey // known only to the first initiator
	SpecialDetails   any

	CreatedAt time.Time
	UpdatedAt time.Time
}

type ChannelHistoryItem struct {
	At     time.Time `json:"-"`
	Action ChannelHistoryEventType
	Data   json.RawMessage
}

type PendingChannelState struct {
	Seqno     uint64
	OurData   AgreedData
	TheirData AgreedData
}

type Side struct {
	Address                 string
	OnchainBalances         map[string]*big.Int
	OnchainInfo             OnchainState
	Data                    AgreedData
	LockedDeposits          map[string]*payments.LockedDepositInfo
	PendingOnchainTransfers map[string]*payments.PendingMessageInfo
	LatestProcessedLT       uint64
	LatestWalletSeqno       uint32
	LatestCommitedSeqno     uint64
	LastProcessedTxAt       time.Time
	ActiveOnchain           bool
	IsSettlementFinalized   bool
}

type PendingCommit struct {
	Seqno   uint64
	Message *cell.Cell
}

type PendingDeposit struct {
	Amount     *big.Int
	ExpectFrom *address.Address
}

type Channel struct {
	ID                     []byte
	Status                 ChannelStatus
	WeLeft                 bool
	SafeOnchainClosePeriod int64

	AcceptingActions   bool
	WebPeer            bool
	UrgentForUs        bool
	UncoopCloseStarted bool

	SignedState *cell.Cell
	Our         Side
	Their       Side

	// InitAt - initialization or reinitialization time
	InitAt    time.Time
	CreatedAt time.Time

	CodeHash        []byte
	InitMessageBody *cell.Cell
	InitialData     *cell.Cell
	PendingCommit   *PendingCommit

	DBVersion int64

	mx sync.RWMutex
}

type OnchainState struct {
	Key ed25519.PublicKey
}

type AgreedData struct {
	Conditionals *cell.Dictionary
	ActionStates *cell.Dictionary
}

func NewAgreedData() AgreedData {
	return AgreedData{
		Conditionals: cell.NewDict(256),
		ActionStates: cell.NewDict(256),
	}
}

func (s *AgreedData) UnmarshalJSON(bytes []byte) error {
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

	sl := cl.MustBeginParse()

	s.Conditionals, err = sl.LoadDict(256)
	if err != nil {
		return err
	}

	s.ActionStates, err = sl.LoadDict(256)
	if err != nil {
		return err
	}

	return nil
}

func (s AgreedData) MarshalJSON() ([]byte, error) {
	c := cell.BeginCell().
		MustStoreDict(s.Conditionals).
		MustStoreDict(s.ActionStates).
		EndCell()

	return []byte(strconv.Quote(base64.StdEncoding.EncodeToString(c.ToBOC()))), nil
}

func (ch *Channel) SideA() *Side {
	if ch.WeLeft {
		return &ch.Our
	}
	return &ch.Their
}

func (ch *Channel) SideB() *Side {
	if ch.WeLeft {
		return &ch.Their
	}
	return &ch.Our
}

func (ch *Channel) LoadSignedState() *payments.StateBodySigned {
	if ch.SignedState == nil {
		return &payments.StateBodySigned{
			SignatureA: payments.Signature{
				Value: make([]byte, 64),
			},
			SignatureB: payments.Signature{
				Value: make([]byte, 64),
			},
			Body: payments.StateBody{
				ChannelID: ch.ID,
				Seqno:     ch.Our.LatestCommitedSeqno,
				A: payments.StateSide{
					ConditionalsHash: make([]byte, 32),
					ActionStatesHash: make([]byte, 32),
				},
				B: payments.StateSide{
					ConditionalsHash: make([]byte, 32),
					ActionStatesHash: make([]byte, 32),
				},
			},
		}
	}

	var state payments.StateBodySigned
	if err := payments.LoadState(&state, ch.SignedState); err != nil {
		panic("corrupted state:" + err.Error())
	}

	return &state
}

func (ch *Channel) CalcBalance(ctx context.Context, isTheir bool, resolver payments.FullResolver) (map[string]*payments.BalanceInfo, error) {
	ch.mx.RLock()
	defer ch.mx.RUnlock()

	s1, s2 := ch.Our, ch.Their
	if isTheir {
		s1, s2 = s2, s1
	}

	// TODO: cache and precalc
	balances := make(map[string]*payments.BalanceInfo)
	for _, config := range resolver.GetKnownBalanceTypes() {
		balances[config.BalanceID] = payments.NewBalanceInfo(config)
	}

	if s1.ActiveOnchain {
		for id, b := range s1.OnchainBalances {
			balances[id].Onchain = new(big.Int).Set(b)
		}
	}

	s1Actions, err := s1.Data.ActionStates.LoadAll()
	if err != nil {
		return nil, err
	}

	s1Conditionals, err := s1.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, err
	}

	s2Actions, err := s2.Data.ActionStates.LoadAll()
	if err != nil {
		return nil, err
	}

	s2Conditionals, err := s2.Data.Conditionals.LoadAll()
	if err != nil {
		return nil, err
	}

	for _, tr := range s1.PendingOnchainTransfers {
		for bid, amt := range tr.Amounts {
			b := balances[bid]
			b.OnHold.Add(b.OnHold, amt)
		}
	}

	for _, v := range s1Actions {
		actId := v.Key.MustLoadSlice(256)
		act, err := resolver.ResolveAction(ctx, actId)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve s1 action %s: %w", hex.EncodeToString(actId), err)
		}

		if err = act.EmulateBalance(v.Value.MustToCell(), balances, true); err != nil {
			return nil, err
		}
	}

	for _, v := range s2Actions {
		actId := v.Key.MustLoadSlice(256)
		act, err := resolver.ResolveAction(ctx, actId)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve s2 action %s: %w", hex.EncodeToString(actId), err)
		}

		if err = act.EmulateBalance(v.Value.MustToCell(), balances, false); err != nil {
			return nil, err
		}
	}

	for _, v := range s1Conditionals {
		c, err := payments.CodeToConditional(ctx, v.Value.MustToCell(), resolver)
		if err != nil {
			return nil, err
		}

		if err = c.EmulateBalance(balances, true); err != nil {
			return nil, err
		}
	}

	for _, v := range s2Conditionals {
		c, err := payments.CodeToConditional(ctx, v.Value.MustToCell(), resolver)
		if err != nil {
			return nil, err
		}

		if err = c.EmulateBalance(balances, false); err != nil {
			return nil, err
		}
	}

	return balances, nil
}

func (ch *Channel) CalcDepositFee(cc *payments.CoinConfig, newAmount *big.Int, till time.Time, isTheir bool) *big.Int {
	const periodSec = 30 * 24 * 60 * 60 // 30 days in seconds

	now := time.Now()
	totalSec := int64(till.Sub(now) / time.Second)
	if totalSec <= 0 {
		return big.NewInt(0)
	}

	// select whose deposit info to use
	dep := ch.Their.LockedDeposits
	if !isTheir {
		dep = ch.Our.LockedDeposits
	}
	ld := dep[cc.BalanceID]

	// determine the old locked amount and its "used" and "until"
	oldAmount := big.NewInt(0)
	oldUsed := big.NewInt(0)
	oldUntil := now
	if ld != nil && ld.Till.After(now) {
		oldAmount.Set(ld.Amount)
		oldUsed.Set(ld.Used)
		oldUntil = ld.Till
	}

	delta := new(big.Int).Sub(newAmount, oldAmount)
	if delta.Sign() <= 0 {
		return big.NewInt(0)
	}

	// fee percent per 30 days
	percentRat := new(big.Rat).SetFloat64(cc.VirtualTunnelConfig.CapacityFeePercentPer30Days / 100.0)
	totalFeeRat := new(big.Rat)

	// extension fee for the existing free part (oldAmount - oldUsed)
	free := new(big.Int).Sub(oldAmount, oldUsed)
	if free.Sign() > 0 && till.After(oldUntil) {
		extSec := int64(till.Sub(oldUntil) / time.Second)
		extRat := new(big.Rat).SetFrac(
			big.NewInt(extSec),
			big.NewInt(periodSec),
		)
		freeRat := new(big.Rat).SetInt(free)

		// fee = free * percent * extensionPeriod
		extFeeRat := new(big.Rat).Mul(freeRat, percentRat)
		extFeeRat.Mul(extFeeRat, extRat)
		totalFeeRat.Add(totalFeeRat, extFeeRat)
	}

	// fee for the delta over the full period from now to till
	if delta.Sign() > 0 {
		totalRat := new(big.Rat).SetFrac(
			big.NewInt(totalSec),
			big.NewInt(periodSec),
		)
		deltaRat := new(big.Rat).SetInt(delta)

		// fee = delta * percent * totalPeriod
		deltaFeeRat := new(big.Rat).Mul(deltaRat, percentRat)
		deltaFeeRat.Mul(deltaFeeRat, totalRat)
		totalFeeRat.Add(totalFeeRat, deltaFeeRat)
	}

	// convert rational fee to integer with ceiling (round up)
	num := totalFeeRat.Num()
	den := totalFeeRat.Denom()
	// ceil(num/den) = (num + den - 1) / den
	numAdd := new(big.Int).Add(num, new(big.Int).Sub(den, big.NewInt(1)))
	capFee := new(big.Int).Quo(numAdd, den)

	// add fixed deposit fee
	result := new(big.Int).Add(capFee, cc.VirtualTunnelConfig.CapacityDepositFee.Nano())
	return result
}

func (ch *ConditionalMeta) AddKnownResolve(ctx context.Context, cond payments.Conditional, state *cell.Cell, errOnOlderState bool) error {
	if cond.GetDeadline().Before(time.Now()) {
		return fmt.Errorf("conditional has expired")
	}

	if err := cond.ValidateState(ctx, ch.LastKnownResolve, state); err != nil {
		if !errOnOlderState && errors.Is(err, payments.ErrNewerConditionalStateIsKnown) {
			return nil
		}
		return fmt.Errorf("failed to validate add state: %w", err)
	}

	ch.LastKnownResolve = state
	return nil
}

func (h *ChannelHistoryItem) ParseData() any {
	var dst any

	switch h.Action {
	case ChannelHistoryActionBalanceChanged:
		dst = &ChannelHistoryActionAmountData{}
	case ChannelHistoryActionTransferIn:
		dst = &ChannelHistoryActionTransferInData{}
	case ChannelHistoryActionTransferOut:
		dst = &ChannelHistoryActionTransferOutData{}
	default:
		return nil
	}

	if err := json.Unmarshal(h.Data, dst); err != nil {
		return nil
	}
	return dst
}
