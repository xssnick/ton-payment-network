package vm

import (
	"fmt"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
)

// PushIntOP - Took from experimental tonutils tvm impl
func PushIntOP(val *big.Int) *cell.Builder {
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

		if x > 257 {
			// PUSHINT rounds the payload up to 259 bits; preserve the sign.
			padding := int64(0)
			if val.Sign() < 0 {
				padding = -1
			}
			c.MustStoreInt(padding, uint(x-257))
			x = 257
		}

		c.MustStoreBigInt(val, uint(x))

		return c
	}
}

func PushRef(slc *cell.Cell) *cell.Builder {
	return cell.BeginCell().MustStoreUInt(0x88, 8).MustStoreRef(slc)
}

func PushNull() *cell.Builder {
	return cell.BeginCell().MustStoreUInt(0x6D, 8)
}

func PushSliceRef(slc *cell.Slice) *cell.Builder {
	return cell.BeginCell().MustStoreUInt(0x89, 8).MustStoreRef(slc.MustToCell())
}

func ReadCellOP(code *cell.Slice) (*cell.Cell, error) {
	v, err := code.LoadUInt(8)
	if err != nil {
		return nil, err
	}

	if v != 0x88 {
		return nil, fmt.Errorf("incorrect opcode")
	}
	return code.LoadRefCell()
}

func ReadCellOrNullOP(code *cell.Slice) (*cell.Cell, error) {
	v, err := code.LoadUInt(8)
	if err != nil {
		return nil, err
	}

	if v != 0x88 && v != 0x6D {
		return nil, fmt.Errorf("incorrect opcode")
	}

	if v == 0x6D {
		return nil, nil
	}
	return code.LoadRefCell()
}

func ReadSliceOP(code *cell.Slice) (*cell.Slice, error) {
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
/*func PushSlice(slc *cell.Slice) *cell.Builder {
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

func ReadIntOP(code *cell.Slice) (*big.Int, error) {
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
