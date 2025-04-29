package transport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math"
	"math/big"
	mRand "math/rand"
	"time"
)

func init() {
	tl.Register(Ping{}, "payments.ping value:long = payments.Ping")
	tl.Register(Pong{}, "payments.pong value:long = payments.Pong")

	tl.Register(Decision{}, "payments.decision agreed:Bool reason:string signature:bytes = payments.Decision")
	tl.Register(ProposalDecision{}, "payments.proposalDecision agreed:Bool reason:string signedState:bytes = payments.ProposalDecision")
	tl.Register(ChannelConfigDecision{}, "payments.channelConfig ok:Bool walletAddr:int256 reason:string = payments.ChannelConfigDecision")
	tl.Register(AuthenticateToSign{}, "payments.authenticateToSign a:int256 b:int256 timestamp:long = payments.AuthenticateToSign")
	tl.Register(NodeAddress{}, "payments.nodeAddress adnl_addr:int256 = payments.NodeAddress")

	tl.Register(ConfirmCloseAction{}, "payments.confirmCloseAction key:int256 state:bytes = payments.Action")
	tl.Register(RemoveVirtualAction{}, "payments.removeVirtualAction key:int256 = payments.Action")
	tl.Register(RequestRemoveVirtualAction{}, "payments.requestRemoveVirtualAction key:int256 = payments.Action")
	tl.Register(OpenVirtualAction{}, "payments.openVirtualAction channel_key:int256 instruction_key:int256 instructions:payments.instructionsToSign signature:bytes = payments.Action")
	tl.Register(CommitVirtualAction{}, "payments.commitVirtualAction key:int256 prepayAmount:bytes = payments.Action")
	tl.Register(CloseVirtualAction{}, "payments.closeVirtualAction key:int256 state:bytes = payments.Action")
	tl.Register(CooperativeCloseAction{}, "payments.cooperativeCloseAction signedCloseRequest:bytes = payments.Action")
	tl.Register(CooperativeCommitAction{}, "payments.cooperativeCommitAction signedCommitRequest:bytes = payments.Action")
	tl.Register(IncrementStatesAction{}, "payments.incrementStatesAction wantResponse:Bool = payments.Action")

	tl.Register(ProposeChannelConfig{}, "payments.proposeChannelConfig jettonAddr:int256 ec_id:int excessFee:bytes quarantineDuration:int misbehaviorFine:bytes conditionalCloseDuration:int = payments.Request")
	tl.Register(RequestAction{}, "payments.requestAction channelAddr:int256 action:payments.Action = payments.Request")
	tl.Register(ProposeAction{}, "payments.proposeAction lockId:long channelAddr:int256 action:payments.Action state:bytes conditionals:bytes = payments.Request")
	tl.Register(Authenticate{}, "payments.authenticate key:int256 timestamp:long signature:bytes = payments.Authenticate")

	tl.Register(InstructionContainer{}, "payments.instructionContainer hash:int256 data:bytes = payments.InstructionContainer")
	tl.Register(InstructionsToSign{}, "payments.instructionsToSign list:(vector payments.instructionContainer) = payments.InstructionsToSign")
	tl.Register(OpenVirtualInstruction{}, "payments.openVirtualInstruction target:int256 expectedFee:bytes expectedCapacity:bytes expectedDeadline:long nextTarget:int256 nextFee:bytes nextCapacity:bytes nextDeadline:long finalState:bytes = payments.OpenVirtualInstruction")

	tl.Register(RequestChannelLock{}, "payments.requestChannelLock lockId:long channel:int256 lock:Bool = payments.RequestChannelLock")
	tl.Register(IsChannelUnlocked{}, "payments.isChannelUnlocked lockId:long channel:int256 = payments.IsChannelUnlocked")
}

type Action any

// NodeAddress - DHT record value which stores adnl addr related to node's public key used for channels
type NodeAddress struct {
	ADNLAddr []byte `tl:"int256"`
}

// Ping - check connection is alive and delay
type Ping struct {
	Value int64 `tl:"long"`
}

// RequestChannelLock - lock/unlock channel to propose actions
type RequestChannelLock struct {
	LockID      int64  `tl:"long"`
	ChannelAddr []byte `tl:"int256"`
	Lock        bool   `tl:"bool"`
}

// IsChannelUnlocked - check is channel still locked with specific id
type IsChannelUnlocked struct {
	LockID      int64  `tl:"long"`
	ChannelAddr []byte `tl:"int256"`
}

// Pong - response on check connection is alive
type Pong struct {
	Value int64 `tl:"long"`
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

// ProposeAction - request party to update state with action,
// for example open virtual channel and add conditional payment
type ProposeAction struct {
	LockID      int64      `tl:"long"`
	ChannelAddr []byte     `tl:"int256"`
	Action      any        `tl:"struct boxed [payments.openVirtualAction,payments.closeVirtualAction,payments.confirmCloseAction,payments.removeVirtualAction,payments.syncStateAction,payments.incrementStatesAction,payments.commitVirtualAction]"`
	SignedState *cell.Cell `tl:"cell"`
	UpdateProof *cell.Cell `tl:"cell optional"`
}

// RequestAction - request party to propose some action
type RequestAction struct {
	ChannelAddr []byte `tl:"int256"`
	Action      any    `tl:"struct boxed [payments.closeVirtualAction,payments.confirmCloseAction,payments.removeVirtualAction,payments.syncStateAction,payments.cooperativeCloseAction,payments.cooperativeCommitAction,payments.requestRemoveVirtualAction]"`
}

// Decision - response for actions request, Reason is filled when not agreed
type Decision struct {
	Agreed    bool   `tl:"bool"`
	Reason    string `tl:"string"`
	Signature []byte `tl:"bytes"`
}

// ProposalDecision - response for actions proposals, Reason is filled when not agreed
type ProposalDecision struct {
	Agreed      bool       `tl:"bool"`
	Reason      string     `tl:"string"`
	SignedState *cell.Cell `tl:"cell optional"`
}

// CommitVirtualAction - prepay virtual channel for amount, can be used for graceful shutdown,
// to not trigger uncooperative close when you offline, by other party virtual closure attempt
type CommitVirtualAction struct {
	Key          []byte `tl:"int256"`
	PrepayAmount []byte `tl:"bytes"`
}

// OpenVirtualAction - request party to open virtual channel (tunnel) with specified target
type OpenVirtualAction struct {
	ChannelKey []byte `tl:"int256"`
	// We use instruction keys to guarantee instructions execution order
	// next node can know instruction key only from previous node, then shared key can be calculated
	InstructionKey []byte             `tl:"int256"`
	Instructions   InstructionsToSign `tl:"struct"`
	Signature      []byte             `tl:"bytes"`
}

type InstructionsToSign struct {
	// ED25519 Montgomery encrypted slice of InstructionContainer.Data + stub,
	// private of Key is used + public of next target from prev instruction.
	// Garlic-like virtual channels
	List []InstructionContainer `tl:"vector struct"`
}

type InstructionContainer struct {
	Hash []byte `tl:"int256"`
	Data []byte `tl:"bytes"`
}

type OpenVirtualInstruction struct {
	Target             []byte `tl:"int256"`
	NextInstructionKey []byte `tl:"int256"`

	ExpectedFee      []byte `tl:"bytes"`
	ExpectedCapacity []byte `tl:"bytes"`
	ExpectedDeadline int64  `tl:"long"`

	NextTarget []byte `tl:"int256"`
	NextFee    []byte `tl:"bytes"`
	// Should be <= ExpectedCapacity
	NextCapacity []byte `tl:"bytes"`
	NextDeadline int64  `tl:"long"`

	// can be set for the final receiver, so virtual channel will be closed immediately,
	// can be used for simple transfers with immediate delivery
	FinalState *cell.Cell `tl:"cell optional"`

	instructionPrivateKey ed25519.PrivateKey `tl:"-"`
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

// CooperativeCommitAction - request party to commit onchain channel state
type CooperativeCommitAction struct {
	SignedCommitRequest *cell.Cell `tl:"cell"`
}

// RemoveVirtualAction - request party to remove expired condition
// related to virtual channel, to unlock funds
type RemoveVirtualAction struct {
	Key []byte `tl:"int256"`
}

// RequestRemoveVirtualAction - request party to close virtual channel to us,
// without state, because something went wrong
type RequestRemoveVirtualAction struct {
	Key []byte `tl:"int256"`
}

// ConfirmCloseAction - request party to remove closed condition
// and increase unconditional amount
type ConfirmCloseAction struct {
	Key   []byte     `tl:"int256"`
	State *cell.Cell `tl:"cell"`
}

// IncrementStatesAction - send our state with incremented seqno to party,
// and expect same from them too when WantResponse = true. Can be used to confirm rollback or for the first state exchange
type IncrementStatesAction struct {
	WantResponse bool `tl:"bool"`
}

// ProposeChannelConfig - request channel params supported by party,
// to deploy contract and initialize communication
type ProposeChannelConfig struct {
	JettonAddr      []byte `tl:"int256"`
	ExtraCurrencyID uint32 `tl:"int"`

	ExcessFee                []byte `tl:"bytes"`
	QuarantineDuration       uint32 `tl:"int"`
	MisbehaviorFine          []byte `tl:"bytes"`
	ConditionalCloseDuration uint32 `tl:"int"`
}

// ChannelConfigDecision - response for ProposeChannelConfig
type ChannelConfigDecision struct {
	Ok         bool   `tl:"bool"`
	WalletAddr []byte `tl:"int256"`
	Reason     string `tl:"string"`
}

func (a *OpenVirtualAction) SetInstructions(actions []OpenVirtualInstruction, key ed25519.PrivateKey) error {
	a.Instructions = InstructionsToSign{}

	maxLen := 0
	serializedActions := make([][]byte, len(actions))
	for i := 0; i < len(actions); i++ {
		data, err := tl.Serialize(actions[i], true)
		if err != nil {
			return fmt.Errorf("failed to serialize action data: %w", err)
		}
		serializedActions[i] = data
		if len(data) > maxLen {
			maxLen = len(data)
		}
	}

	// randomly increase len to complicate external analysis for instructions count
	fuzz, err := rand.Int(rand.Reader, big.NewInt(512))
	if err != nil {
		return err
	}
	maxLen += int(fuzz.Int64())

	for i := 0; i < len(actions); i++ {
		lenDiff := maxLen - len(serializedActions[i])
		// add random stub data to hide real size
		data := append(serializedActions[i], make([]byte, lenDiff)...)
		// fill padding with random bytes to avoid potential zero padding attacks
		_, _ = rand.Read(data[len(serializedActions[i]):])

		sharedKey, err := adnl.SharedKey(actions[i].instructionPrivateKey, actions[i].Target)
		if err != nil {
			return fmt.Errorf("failed to calc shared key: %w", err)
		}

		hash := sha256.Sum256(data)
		stream, err := adnl.BuildSharedCipher(sharedKey, hash[:])
		if err != nil {
			return fmt.Errorf("failed to init cipher: %w", err)
		}
		stream.XORKeyStream(data, data)

		a.Instructions.List = append(a.Instructions.List, InstructionContainer{
			Hash: hash[:],
			Data: data,
		})
	}

	data, err := tl.Serialize(a.Instructions, true)
	if err != nil {
		return fmt.Errorf("failed to serialize instructions data: %w", err)
	}
	a.Signature = ed25519.Sign(key, data)

	return nil
}

func (a *OpenVirtualAction) DecryptOurInstruction(key ed25519.PrivateKey, instructionKey ed25519.PublicKey) (*OpenVirtualInstruction, error) {
	verifyData, err := tl.Serialize(a.Instructions, true)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize verify data: %w", err)
	}

	if !ed25519.Verify(a.ChannelKey, verifyData, a.Signature) {
		return nil, fmt.Errorf("incorrect signature")
	}

	sharedKey, err := adnl.SharedKey(key, instructionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to calc shared key: %w", err)
	}

	for _, instruction := range a.Instructions.List {
		stream, err := adnl.BuildSharedCipher(sharedKey, instruction.Hash)
		if err != nil {
			return nil, fmt.Errorf("failed to init cipher: %w", err)
		}

		payload := make([]byte, len(instruction.Data))
		stream.XORKeyStream(payload, instruction.Data)

		hash := sha256.Sum256(payload)
		if !bytes.Equal(hash[:], instruction.Hash) {
			// not our
			continue
		}

		var value OpenVirtualInstruction
		if _, err = tl.Parse(&value, payload, true); err != nil {
			return nil, fmt.Errorf("incorrect OpenVirtualInstruction: %w", err)
		}
		return &value, nil
	}
	return nil, fmt.Errorf("not found")
}

type TunnelChainPart struct {
	Target   ed25519.PublicKey
	Capacity *big.Int
	Fee      *big.Int
	Deadline time.Time
}

func GenerateTunnel(key ed25519.PrivateKey, chain []TunnelChainPart, stubSize uint8, withFinalState bool) (payments.VirtualChannel, ed25519.PublicKey, []OpenVirtualInstruction, error) {
	if len(chain) == 0 {
		return payments.VirtualChannel{}, nil, nil, fmt.Errorf("chain is empty")
	}

	vc := payments.VirtualChannel{
		Key:      key.Public().(ed25519.PublicKey),
		Capacity: chain[0].Capacity,
		Fee:      chain[0].Fee,
		Prepay:   big.NewInt(0),
		Deadline: chain[0].Deadline.UTC().Unix(),
	}

	var firstInstructionKey ed25519.PublicKey
	var list []OpenVirtualInstruction
	for i := 0; i < len(chain); i++ {
		inst := OpenVirtualInstruction{
			ExpectedFee:      chain[i].Fee.Bytes(),
			ExpectedCapacity: chain[i].Capacity.Bytes(),
			ExpectedDeadline: chain[i].Deadline.UTC().Unix(),
			Target:           chain[i].Target,
		}

		pub, private, err := ed25519.GenerateKey(nil)
		if err != nil {
			return payments.VirtualChannel{}, nil, nil, err
		}

		inst.instructionPrivateKey = private
		if i > 0 {
			list[i-1].NextInstructionKey = pub
		} else {
			firstInstructionKey = pub
		}

		if i < len(chain)-1 {
			inst.NextTarget = chain[i+1].Target
			inst.NextFee = chain[i+1].Fee.Bytes()
			inst.NextCapacity = chain[i+1].Capacity.Bytes()
			inst.NextDeadline = chain[i+1].Deadline.UTC().Unix()
		} else {
			inst.NextTarget = chain[i].Target
			inst.NextFee = chain[i].Fee.Bytes()
			inst.NextCapacity = chain[i].Capacity.Bytes()
			inst.NextDeadline = chain[i].Deadline.UTC().Unix()
			if withFinalState {
				state := payments.VirtualChannelState{Amount: chain[i].Capacity}
				state.Sign(key)

				fs, err := tlb.ToCell(state)
				if err != nil {
					return payments.VirtualChannel{}, nil, nil, fmt.Errorf("failed to serialize final state: %w", err)
				}
				inst.FinalState = fs
			}
		}

		list = append(list, inst)
	}

	// generate seed with cryptographic random
	seed, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return payments.VirtualChannel{}, nil, nil, err
	}
	mRnd := mRand.New(mRand.NewSource(seed.Int64()))

	for i := 0; i < int(stubSize); i++ {
		// all those actions will be encrypted, we use similar amounts
		// just to keep bytes length similar to original for stronger security

		nextFee := randAmount(chain[len(chain)-1].Fee, chain[0].Fee)
		nextCap := randAmount(chain[len(chain)-1].Capacity, chain[0].Capacity)

		randKey := make([]byte, 32)
		_, _ = rand.Read(randKey)
		randKey2, _, _ := ed25519.GenerateKey(nil)
		randKey3, randKey3prv, _ := ed25519.GenerateKey(nil)

		// spread deadlines to look similar to original
		dlDiff := (chain[0].Deadline.UTC().Unix() - chain[len(chain)-1].Deadline.UTC().Unix()) * 2
		if dlDiff < 3600 {
			dlDiff = 3600
		}

		nextDl := (chain[len(chain)-1].Deadline.UTC().Unix() - dlDiff/2) + mRnd.Int63n(dlDiff)
		expDl := nextDl + mRnd.Int63n(dlDiff)

		list = append(list, OpenVirtualInstruction{
			Target:                randKey2,
			ExpectedFee:           randAmount(chain[len(chain)-1].Fee, nextFee).Bytes(),
			ExpectedCapacity:      randAmount(chain[len(chain)-1].Capacity, nextCap).Bytes(),
			ExpectedDeadline:      expDl,
			NextTarget:            randKey,
			NextInstructionKey:    randKey3,
			NextFee:               nextFee.Bytes(),
			NextDeadline:          nextDl,
			NextCapacity:          nextCap.Bytes(),
			instructionPrivateKey: randKey3prv,
		})
	}

	mRnd.Shuffle(len(list), func(i, j int) {
		list[i], list[j] = list[j], list[i]
	})

	return vc, firstInstructionKey, list, nil
}

func randAmount(from, to *big.Int) *big.Int {
	diff := new(big.Int).Sub(to, from)
	if diff.Sign() != 1 {
		return from
	}

	n, err := rand.Int(rand.Reader, diff)
	if err != nil {
		// practically impossible
		return from
	}
	return n.Add(n, from)
}
