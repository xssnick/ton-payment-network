package payments

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

type VirtualChannel struct {
	Key      ed25519.PublicKey
	Capacity *big.Int
	Fee      *big.Int
	Prepay   *big.Int
	Deadline int64
}

type VirtualChannelUniversal struct {
	ActionHash []byte
	Key        ed25519.PublicKey
	Capacity   *big.Int
	Fee        *big.Int
	Prepay     *big.Int
	Deadline   int64
}

type ActionSendUniversal struct {
	AddressA *address.Address
	AddressB *address.Address
	Body     *cell.Cell
}

type VirtualChannelState struct {
	Signature []byte
	Amount    *big.Int
}

func SignState(amount tlb.Coins, signKey ed25519.PrivateKey, to ed25519.PublicKey) (res VirtualChannelState, encrypted []byte, err error) {
	st := VirtualChannelState{
		Amount: amount.Nano(),
	}
	st.Sign(signKey)

	cll, err := st.ToCell()
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to serialize cell: %w", err)
	}
	data := cll.ToBOC()

	sharedKey, err := keys.SharedKey(signKey, to)
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to generate shared key: %w", err)
	}
	pub := signKey.Public().(ed25519.PublicKey)

	stream, err := keys.BuildSharedCipher(sharedKey, pub)
	if err != nil {
		return VirtualChannelState{}, nil, fmt.Errorf("failed to init cipher: %w", err)
	}
	// we encrypt state to be sure no one can hijack it and use in the middle of the chain
	stream.XORKeyStream(data, data)

	return st, append(pub, data...), nil
}

func ParseState(data []byte, to ed25519.PrivateKey) (ed25519.PublicKey, VirtualChannelState, error) {
	if len(data) <= 32+64 || len(data) > 32+64+64 {
		return nil, VirtualChannelState{}, fmt.Errorf("incorrect len of state")
	}

	sharedKey, err := keys.SharedKey(to, data[:32])
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to generate shared key: %w", err)
	}

	var payload = data[32:]
	stream, err := keys.BuildSharedCipher(sharedKey, data[:32])
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to init cipher: %w", err)
	}
	stream.XORKeyStream(payload, payload)

	cll, err := cell.FromBOC(payload)
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to parse cell: %w", err)
	}

	var res VirtualChannelState
	if err = tlb.LoadFromCell(&res, cll.BeginParse()); err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to parse state: %w", err)
	}

	if !res.Verify(data[:32]) {
		return nil, VirtualChannelState{}, fmt.Errorf("incorrect signature")
	}

	var key [32]byte
	copy(key[:], data[:32])

	return key[:], res, nil
}

var virtualChannelStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		fun cond(input: slice, fee: int, capacity: int, prepaid: int, deadline: int, key: int) {
			var sign: slice = input.loadBits(512);
			assert(isSliceSignatureValid(input, sign, key) & (deadline >= now()), 24);

			var amount: int = input.loadCoins();
			assert((amount <= capacity) & (prepaid <= amount), 26);
			return (amount - prepaid) + fee;
		}
	*/

	data, err := hex.DecodeString("b5ee9c72010101010023000042058308d7186607f91101f823beb0f29803fa00305202bb5331bbb0f29a58a101a0")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()

var virtualChannelUniversalStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		fun conditional_coins(targetActionsInput: dict, condInput: slice, actionHash: int, fee: int, capacity: int, prepaid: int, deadline: int, key: int): dict {
		    var (actInput, ok) = targetActionsInput.uDictGet(256, actionHash);
		    if (actInput == null || !ok) {
		        // we must always have action to execute condition
		        return targetActionsInput;
		    }

		    var sign: slice = condInput.loadBits(512);
		    assert(isSliceSignatureValid(condInput, sign, key) & (deadline >= blockchain.now())) throw 24;
		    var amount: int = condInput.loadCoins();
		    assert((amount <= capacity) & (prepaid <= amount)) throw 26;

		    var v = actInput.loadAny<FeeActionInput>();
		    v.amount += (amount - prepaid) + fee;

		    targetActionsInput.uDictSet(256, actionHash, v.toCell().beginParse());

		    return targetActionsInput;
		}
	*/

	data, err := hex.DecodeString("b5ee9c7241010101004900008e53578307f40e6fa1216e92307f93b3c300e2925f08e0078308d7186603f91102f823be12b0f298fa00305203bb5312bbb0f29a04fa003004a1a012a0c801fa02c9d0028307f4164f3d04bc")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()

var actionCoinsUniversalStaticCode = func() *cell.Cell {
	// compiled using code:
	/*
		struct FeeActionInput {
			amount: coins
		}

		fun action_coins(actOur: slice, actTheir: slice, isA: bool, addressA: address, addressB: address, body: cell): void {
			var our = actOur.loadAny<FeeActionInput>();
			var their = actTheir.loadAny<FeeActionInput>();
			var amt = our.amount - their.amount;

			if (amt <= 0) {
				return;
			}

			createMessage({
				bounce: false,
				dest: isA ? addressA : addressB,
				value: amt,
				body: body,
			}).send(SEND_MODE_REGULAR | SEND_MODE_IGNORE_ERRORS);
		}
	*/

	data, err := hex.DecodeString("b5ee9c7241010101002700004a05fa003004fa003014a120c101925f05e003e304c8cf8508ce01fa0271cf0b6accc972fb00bcc148bd")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()

func (c *VirtualChannelUniversal) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(pushIntOP(new(big.Int).SetBytes(c.ActionHash))).
		MustStoreBuilder(pushIntOP(c.Fee)).
		MustStoreBuilder(pushIntOP(c.Capacity)).
		MustStoreBuilder(pushIntOP(c.Prepay)).
		MustStoreBuilder(pushIntOP(big.NewInt(c.Deadline))).
		MustStoreBuilder(pushIntOP(new(big.Int).SetBytes(c.Key))).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(virtualChannelUniversalStaticCode). // implicit jump
		EndCell()
}

func (c *ActionSendUniversal) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(pushSliceRef(cell.BeginCell().MustStoreAddr(c.AddressA).ToSlice())).
		MustStoreBuilder(pushSliceRef(cell.BeginCell().MustStoreAddr(c.AddressB).ToSlice())).
		MustStoreBuilder(pushRef(c.Body)).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(actionCoinsUniversalStaticCode). // implicit jump
		EndCell()
}

func (c *VirtualChannel) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(pushIntOP(c.Fee)).
		MustStoreBuilder(pushIntOP(c.Capacity)).
		MustStoreBuilder(pushIntOP(c.Prepay)).
		MustStoreBuilder(pushIntOP(big.NewInt(c.Deadline))).
		MustStoreBuilder(pushIntOP(new(big.Int).SetBytes(c.Key))).
		// we pack immutable part of code to ref for better BoC compression and cheaper transactions
		MustStoreRef(virtualChannelStaticCode). // implicit jump
		EndCell()
}

func ParseVirtualChannelCond(s *cell.Slice) (*VirtualChannel, error) {
	fee, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse fee: %w", err)
	}
	if fee.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse fee: incorrect bits len")
	}
	if fee.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse fee: cannot be negative")
	}

	capacity, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse capacity: %w", err)
	}
	if capacity.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse capacity: incorrect bits len")
	}
	if capacity.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse capacity: cannot be negative")
	}

	prepay, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse prepay: %w", err)
	}
	if prepay.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse prepay: incorrect bits len")
	}
	if prepay.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse prepay: cannot be negative")
	}

	deadline, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse deadline: %w", err)
	}
	if deadline.BitLen() > 32 {
		return nil, fmt.Errorf("failed to parse deadline: incorrect bits len")
	}
	if deadline.Sign() <= 0 {
		return nil, fmt.Errorf("failed to parse deadline: cannot be negative or zero")
	}

	keyInt, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key: %w", err)
	}

	key := keyInt.Bytes()
	if len(key) > 32 {
		return nil, fmt.Errorf("too big key size")
	}

	if len(key) < 32 {
		// prepend it with zeroes
		key = append(make([]byte, 32-len(key)), key...)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return nil, fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), virtualChannelStaticCode.Hash()) {
		return nil, fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return nil, fmt.Errorf("unexpected data in condition")
	}

	return &VirtualChannel{
		Key:      key,
		Capacity: capacity,
		Fee:      fee,
		Prepay:   prepay,
		Deadline: int64(deadline.Uint64()),
	}, nil
}

func ParseActionSendUniversal(s *cell.Slice) (*ActionSendUniversal, error) {
	slc, err := readSliceOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse addr slice: %w", err)
	}
	addrA, err := slc.LoadAddr()
	if err != nil {
		return nil, fmt.Errorf("failed to parse addr: %w", err)
	}
	slc, err = readSliceOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse addr slice: %w", err)
	}
	addrB, err := slc.LoadAddr()
	if err != nil {
		return nil, fmt.Errorf("failed to parse addr: %w", err)
	}

	body, err := readCellOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse body ref: %w", err)
	}

	return &ActionSendUniversal{
		AddressA: addrA,
		AddressB: addrB,
		Body:     body,
	}, nil
}

func ParseVirtualChannelUniversalCond(s *cell.Slice) (*VirtualChannelUniversal, error) {
	actHashInt, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse fee: %w", err)
	}

	fee, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse fee: %w", err)
	}
	if fee.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse fee: incorrect bits len")
	}
	if fee.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse fee: cannot be negative")
	}

	capacity, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse capacity: %w", err)
	}
	if capacity.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse capacity: incorrect bits len")
	}
	if capacity.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse capacity: cannot be negative")
	}

	prepay, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse prepay: %w", err)
	}
	if prepay.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse prepay: incorrect bits len")
	}
	if prepay.Sign() < 0 {
		return nil, fmt.Errorf("failed to parse prepay: cannot be negative")
	}

	deadline, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse deadline: %w", err)
	}
	if deadline.BitLen() > 32 {
		return nil, fmt.Errorf("failed to parse deadline: incorrect bits len")
	}
	if deadline.Sign() <= 0 {
		return nil, fmt.Errorf("failed to parse deadline: cannot be negative or zero")
	}

	keyInt, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key: %w", err)
	}

	key := keyInt.Bytes()
	if len(key) > 32 {
		return nil, fmt.Errorf("too big key size")
	}

	if len(key) < 32 {
		// prepend it with zeroes
		key = append(make([]byte, 32-len(key)), key...)
	}

	actHash := actHashInt.Bytes()
	if len(key) > 32 {
		return nil, fmt.Errorf("too big act hash size")
	}

	if len(actHash) < 32 {
		// prepend it with zeroes
		actHash = append(make([]byte, 32-len(actHash)), actHash...)
	}

	code, err := s.LoadRefCell()
	if err != nil {
		return nil, fmt.Errorf("failed to parse code: %w", err)
	}

	if !bytes.Equal(code.Hash(), virtualChannelUniversalStaticCode.Hash()) {
		return nil, fmt.Errorf("incorrect code")
	}

	if s.BitsLeft() != 0 || s.RefsNum() != 0 {
		return nil, fmt.Errorf("unexpected data in condition")
	}

	return &VirtualChannelUniversal{
		ActionHash: actHash,
		Key:        key,
		Capacity:   capacity,
		Fee:        fee,
		Prepay:     prepay,
		Deadline:   int64(deadline.Uint64()),
	}, nil
}

// pushIntOP - Took from experimental tonutils tvm impl
func pushIntOP(val *big.Int) *cell.Builder {
	bitsSz := val.BitLen() + 1 // 1 bit for sign

	switch {
	case bitsSz <= 8:
		return cell.BeginCell().MustStoreUInt(0x80, 8).MustStoreBigInt(val, 8)
	case bitsSz <= 16:
		return cell.BeginCell().MustStoreUInt(0x81, 8).MustStoreBigInt(val, 16)
	default:
		if bitsSz < 19 {
			bitsSz = 19
		}
		sz := uint64(bitsSz - 19) // 8*l = 256 - 19

		l := sz / 8
		if sz%8 != 0 {
			l += 1
		}

		x := 19 + l*8

		c := cell.BeginCell().
			MustStoreUInt(0x82, 8).
			MustStoreUInt(l, 5)

		if x > 256 {
			c.MustStoreUInt(0, uint(x-256))
			x = 256
		}

		c.MustStoreBigInt(val, uint(x))

		return c
	}
}

func pushRef(slc *cell.Cell) *cell.Builder {
	return cell.BeginCell().MustStoreUInt(0x88, 8).MustStoreRef(slc)
}

func pushSliceRef(slc *cell.Slice) *cell.Builder {
	return cell.BeginCell().MustStoreUInt(0x89, 8).MustStoreRef(slc.MustToCell())
}

func readCellOP(code *cell.Slice) (*cell.Cell, error) {
	v, err := code.LoadUInt(8)
	if err != nil {
		return nil, err
	}

	if v != 0x88 {
		return nil, fmt.Errorf("incorrect opcode")
	}
	return code.LoadRefCell()
}

func readSliceOP(code *cell.Slice) (*cell.Slice, error) {
	v, err := code.LoadUInt(8)
	if err != nil {
		return nil, err
	}

	if v != 0x89 {
		return nil, fmt.Errorf("incorrect opcode")
	}
	return code.LoadRef()
}

// 8Bxsss
/*func pushSlice(slc *cell.Slice) *cell.Builder {
	bitsSz := slc.BitsLeft()

	ln := (bitsSz - 4) / 8
	if ln < 0 {
		ln = 0
	} else if (bitsSz-4)%8 != 0 {
		ln++
	}

	return cell.BeginCell().MustStoreUInt(0x8B, 8).
		MustStoreUInt(uint64(ln), 4).
		MustStoreSlice(slc.MustLoadSlice(bitsSz), 4+uint(ln)*8)
}*/

func readIntOP(code *cell.Slice) (*big.Int, error) {
	prefix, err := code.LoadUInt(8)
	if err != nil {
		return nil, err
	}
	switch prefix {
	case 0x80:
		val, err := code.LoadBigInt(8)
		if err != nil {
			return nil, err
		}
		return val, nil
	case 0x81:
		val, err := code.LoadBigInt(16)
		if err != nil {
			return nil, err
		}
		return val, nil
	case 0x82:
		szBytes, err := code.LoadUInt(5)
		if err != nil {
			return nil, err
		}

		sz := szBytes*8 + 19

		if sz > 257 {
			_, err = code.LoadUInt(uint(sz - 257)) // kill round bits
			if err != nil {
				return nil, err
			}
			sz = 257
		}

		val, err := code.LoadBigInt(uint(sz))
		if err != nil {
			return nil, err
		}

		return val, nil
	}
	return nil, fmt.Errorf("incorrect opcode")
}

func (c VirtualChannelState) ToCell() (*cell.Cell, error) {
	if len(c.Signature) != 64 {
		return nil, fmt.Errorf("icorrect signature size")
	}

	b := cell.BeginCell().
		MustStoreSlice(c.Signature, 512).
		MustStoreBuilder(c.serializePayload())
	return b.EndCell(), nil
}

func (c *VirtualChannelState) serializePayload() *cell.Builder {
	b := cell.BeginCell().MustStoreBigCoins(c.Amount)
	notFullBits := b.BitsUsed() % 8
	if notFullBits != 0 {
		b.MustStoreUInt(0, 8-notFullBits)
	}
	return b
}

func (c *VirtualChannelState) LoadFromCell(loader *cell.Slice) error {
	sign, err := loader.LoadSlice(512)
	if err != nil {
		return err
	}

	sz := loader.BitsLeft()
	coins, err := loader.LoadBigCoins()
	if err != nil {
		return err
	}
	sz -= loader.BitsLeft()

	notFullBits := sz % 8
	if notFullBits != 0 {
		_, err = loader.LoadUInt(8 - notFullBits)
		if err != nil {
			return err
		}
	}
	c.Signature = sign
	c.Amount = coins
	return nil
}

func (c *VirtualChannelState) Sign(key ed25519.PrivateKey) {
	cl := c.serializePayload().ToSlice()
	// we need hash of data part only, because CHEKSIGNS is used in condition
	c.Signature = ed25519.Sign(key, cl.MustLoadSlice(cl.BitsLeft()))
}

func (c *VirtualChannelState) Verify(key ed25519.PublicKey) bool {
	cl := c.serializePayload().ToSlice()
	// we need hash of data part only, because CHEKSIGNS is used in condition
	return ed25519.Verify(key, cl.MustLoadSlice(cl.BitsLeft()), c.Signature)
}
