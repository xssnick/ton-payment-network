package payments

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

type VirtualChannel struct {
	Key      ed25519.PublicKey
	Capacity *big.Int
	Fee      *big.Int
	Deadline int64
}

type VirtualChannelState struct {
	Signature []byte
	Amount    tlb.Coins
}

func SignState(amount tlb.Coins, signKey ed25519.PrivateKey, to ed25519.PublicKey) ([]byte, error) {
	st := &VirtualChannelState{
		Amount: amount,
	}
	st.Sign(signKey)

	cll, err := st.ToCell()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize cell: %w", err)
	}
	data := cll.ToBOC()

	sharedKey, err := adnl.SharedKey(signKey, to)
	if err != nil {
		return nil, fmt.Errorf("failed to generate shared key: %w", err)
	}
	pub := signKey.Public().(ed25519.PublicKey)

	stream, err := adnl.BuildSharedCipher(sharedKey, pub)
	if err != nil {
		return nil, fmt.Errorf("failed to init cipher: %w", err)
	}
	// we encrypt state to be sure no one can hijack it and use in the middle of the chain
	stream.XORKeyStream(data, data)

	return append(pub, data...), nil
}

func ParseState(data []byte, to ed25519.PrivateKey) (ed25519.PublicKey, VirtualChannelState, error) {
	if len(data) <= 32+64 || len(data) > 32+64+64 {
		return nil, VirtualChannelState{}, fmt.Errorf("incorrect len of state")
	}

	sharedKey, err := adnl.SharedKey(to, data[:32])
	if err != nil {
		return nil, VirtualChannelState{}, fmt.Errorf("failed to generate shared key: %w", err)
	}

	var payload = data[32:]
	stream, err := adnl.BuildSharedCipher(sharedKey, data[:32])
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
	// compiled using FunC code:
	/*
		int cond(slice input, int fee, int capacity, int deadline, int key) {
		    slice sign = input~load_bits(512);
		    throw_unless(24, check_data_signature(input, sign, key));
		    throw_unless(25, deadline >= now());

		    int amount = input~load_coins();
		    throw_unless(26, amount <= capacity);

		    return amount + fee;
		}
	*/

	/*
		s0 s4 XCHG
		9 PUSHPOW2
		LDSLICEX
		TUCK
		s0 s6 XCHG
		CHKSIGNS
		24 THROWIFNOT
		NOW
		GEQ
		25 THROWIFNOT
		s0 s2 XCHG
		LDGRAMS
		DROP
		s0 s2 PUXC
		LEQ
		26 THROWIFNOT
		ADD
	*/
	data, err := hex.DecodeString("b5ee9c7241010101001c000034048308d7186606f911f298f823bef29902fa00305203bbf29aa0c8677719")
	if err != nil {
		panic(err.Error())
	}

	code, err := cell.FromBOC(data)
	if err != nil {
		panic(err.Error())
	}
	return code
}()

func (c *VirtualChannel) Serialize() *cell.Cell {
	return cell.BeginCell().
		MustStoreBuilder(pushIntOP(c.Fee)).
		MustStoreBuilder(pushIntOP(c.Capacity)).
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

	capacity, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse capacity: %w", err)
	}
	if capacity.BitLen() > 127 {
		return nil, fmt.Errorf("failed to parse capacity: incorrect bits len")
	}

	deadline, err := readIntOP(s)
	if err != nil {
		return nil, fmt.Errorf("failed to parse deadline: %w", err)
	}
	if deadline.BitLen() > 32 {
		return nil, fmt.Errorf("failed to parse deadline: incorrect bits len")
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
		Deadline: int64(deadline.Uint64()),
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
	b := cell.BeginCell().MustStoreBigCoins(c.Amount.Nano())
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
	c.Amount = tlb.FromNanoTON(coins)
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
