package payments

import (
	"math/big"
	"testing"
)

func Test_pushIntOP(t *testing.T) {
	for _, x := range []*big.Int{big.NewInt(0), big.NewInt(10), big.NewInt(246), big.NewInt(-10),
		big.NewInt(30000), big.NewInt(-30000), big.NewInt(80000000), big.NewInt(-80000000)} {
		got := pushIntOP(new(big.Int).Set(x)).EndCell()
		v, err := readIntOP(got.BeginParse())
		if err != nil {
			t.Fatal(err.Error())
		}

		if v.Cmp(x) != 0 {
			t.Fatal("incorrect value", x.String(), v.String())
		}
	}
}
