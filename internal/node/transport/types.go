package transport

import (
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func init() {
	tl.Register(Decision{}, "payments.decision agreed:Bool reason:string = payments.Decision")
	tl.Register(ChannelConfig{}, "payments.channelConfig excessFee:bytes walletAddr:int256 quarantineDuration:int misbehaviorFine:bytes conditionalCloseDuration:int = payments.ChannelConfig")
	tl.Register(AuthenticateToSign{}, "payments.authenticateToSign a:int256 b:int256 timestamp:long = payments.AuthenticateToSign")
	tl.Register(NodeAddress{}, "payments.nodeAddress adnl_addr:int256 = payments.NodeAddress")

	tl.Register(ConfirmCloseAction{}, "payments.confirmCloseAction key:int256 state:bytes = payments.Action")
	tl.Register(UnlockExpiredAction{}, "payments.unlockExpiredAction key:int256 = payments.Action")
	tl.Register(OpenVirtualAction{}, "payments.openVirtualAction target:int256 key:int256 = payments.Action")
	tl.Register(CloseVirtualAction{}, "payments.closeVirtualAction key:int256 state:bytes = payments.Action")
	tl.Register(CooperativeCloseAction{}, "payments.cooperativeCloseAction signedCloseRequest:bytes = payments.Action")
	tl.Register(IncrementStatesAction{}, "payments.incrementStatesAction response:Bool = payments.Action")

	tl.Register(GetChannelConfig{}, "payments.getChannelConfig = payments.Request")
	tl.Register(RequestAction{}, "payments.requestAction channelAddr:int256 action:payments.Action = payments.Request")
	tl.Register(ProposeAction{}, "payments.proposeAction channelAddr:int256 action:payments.Action state:bytes = payments.Request")
	tl.Register(RequestInboundChannel{}, "payments.requestInboundChannel key:int256 wallet:int256 capacity:bytes = payments.Request")
	tl.Register(Authenticate{}, "payments.authenticate key:int256 timestamp:long signature:bytes = payments.Authenticate")
}

type Action any

// NodeAddress - DHT record value which stores adnl addr related to node's public key used for channels
type NodeAddress struct {
	ADNLAddr []byte `tl:"int256"`
}

// Authenticate - auth with both sides adnl ids signature, to establish connection
type Authenticate struct {
	Key       []byte `tl:"int256"`
	Timestamp int64  `tl:"long"`
	// It should be the signature of AuthenticateToSign, signed by node channel key
	Signature []byte `tl:"bytes"`
}

// AuthenticateToSign - payload to sign for auth, A and B are adnl addresses of parties
type AuthenticateToSign struct {
	A         []byte `tl:"int256"`
	B         []byte `tl:"int256"`
	Timestamp int64  `tl:"long"`
}

// RequestInboundChannel - request party to deploy channel with us,
// and initialize it with Capacity amount, to send us coins
type RequestInboundChannel struct {
	Key      []byte `tl:"int256"`
	Wallet   []byte `tl:"int256"`
	Capacity []byte `tl:"bytes"`
}

// ProposeAction - request party to update state with action,
// for example open virtual channel and add conditional payment
type ProposeAction struct {
	ChannelAddr []byte     `tl:"int256"`
	Action      any        `tl:"struct boxed [payments.openVirtualAction,payments.closeVirtualAction,payments.confirmCloseAction,payments.unlockExpiredAction,payments.syncStateAction,payments.incrementStatesAction]"`
	SignedState *cell.Cell `tl:"cell"`
}

// RequestAction - request party to propose some action
type RequestAction struct {
	ChannelAddr []byte `tl:"int256"`
	Action      any    `tl:"struct boxed [payments.closeVirtualAction,payments.confirmCloseAction,payments.unlockExpiredAction,payments.syncStateAction,payments.cooperativeCloseAction]"`
}

// Decision - response for actions proposals and request, Reason is filled when not agreed
type Decision struct {
	Agreed bool   `tl:"bool"`
	Reason string `tl:"string"`
}

// OpenVirtualAction - request party to open virtual channel (tunnel) with specified target
type OpenVirtualAction struct {
	Target []byte `tl:"int256"`
	Key    []byte `tl:"int256"`
}

// CloseVirtualAction - request party to close virtual channel,
// must be accepted only from virtual channel receiver side.
type CloseVirtualAction struct {
	Key   []byte     `tl:"int256"`
	State *cell.Cell `tl:"cell"`
}

// CooperativeCloseAction - request party to close onchain channel
type CooperativeCloseAction struct {
	SignedCloseRequest *cell.Cell `tl:"cell"`
}

// UnlockExpiredAction - request party to remove expired condition
// related to virtual channel, to unlock funds
type UnlockExpiredAction struct {
	Key []byte `tl:"int256"`
}

// ConfirmCloseAction - request party to remove closed condition
// and increase unconditional amount
type ConfirmCloseAction struct {
	Key   []byte     `tl:"int256"`
	State *cell.Cell `tl:"cell"`
}

// IncrementStatesAction - send our state with incremented seqno to party,
// and expect same from them too. Can be used to confirm rollback or for the first state exchange
type IncrementStatesAction struct {
	Response bool `tl:"bool"`
}

// GetChannelConfig - request channel params supported by party,
// to deploy contract and initialize communication
type GetChannelConfig struct{}

// ChannelConfig - response of GetChannelConfig
type ChannelConfig struct {
	ExcessFee                []byte `tl:"bytes"`
	WalletAddr               []byte `tl:"int256"`
	QuarantineDuration       uint32 `tl:"int"`
	MisbehaviorFine          []byte `tl:"bytes"`
	ConditionalCloseDuration uint32 `tl:"int"`
}
