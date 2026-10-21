package vm

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/xssnick/tonutils-go/tvm/cell"
	stackop "github.com/xssnick/tonutils-go/tvm/op/stack"
)

func Test_pushIntOP(t *testing.T) {
	for _, x := range []*big.Int{big.NewInt(0), big.NewInt(10), big.NewInt(246), big.NewInt(-10),
		big.NewInt(30000), big.NewInt(-30000), big.NewInt(80000000), big.NewInt(-80000000)} {
		got := PushIntOP(new(big.Int).Set(x)).EndCell()
		v, err := ReadIntOP(got.MustBeginParse())
		if err != nil {
			t.Fatal(err.Error())
		}

		if v.Cmp(x) != 0 {
			t.Fatal("incorrect value", x.String(), v.String())
		}
	}
}

func TestPushIntOPLargeIntegers(t *testing.T) {
	pow2 := func(bits uint) *big.Int {
		return new(big.Int).Lsh(big.NewInt(1), bits)
	}
	for _, tc := range []struct {
		name  string
		value *big.Int
	}{
		{"high_bit_hash", pow2(255)},
		{"max_hash", new(big.Int).Sub(pow2(256), big.NewInt(1))},
		{"positive_251_bits", pow2(250)},
		{"positive_255_bits", pow2(254)},
		{"negative_251_bits", new(big.Int).Neg(pow2(250))},
		{"negative_255_bits", new(big.Int).Neg(pow2(254))},
		{"negative_256_bits", new(big.Int).Neg(pow2(255))},
		{"min_tvm_integer", new(big.Int).Neg(pow2(256))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := new(big.Int).Set(tc.value)
			encoded := PushIntOP(tc.value).EndCell()
			got, err := ReadIntOP(encoded.MustBeginParse())
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("decoded %s, want %s", got, want)
			}
			if tc.value.Cmp(want) != 0 {
				t.Fatal("encoding changed the input integer")
			}

			// Decode with TVM's implementation as well: ReadIntOP skips padding.
			op := stackop.PUSHINT(nil)
			if err := op.Deserialize(encoded.MustBeginParse()); err != nil {
				t.Fatal(err)
			}
			if got := op.SerializeText(); got != want.String()+" PUSHINT" {
				t.Fatalf("TVM decoded %s, want %s PUSHINT", got, want)
			}

			if want.Sign() >= 0 {
				// Preserve the existing encoding of unsigned 256-bit hashes.
				legacy := cell.BeginCell().MustStoreUInt(0x82, 8).
					MustStoreUInt(30, 5).MustStoreUInt(0, 3).
					MustStoreBigUInt(want, 256).EndCell()
				if !bytes.Equal(encoded.Hash(), legacy.Hash()) {
					t.Fatal("encoding of an existing positive integer changed")
				}
			}
		})
	}
}
