package tonpayments

import (
	"fmt"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func executeConditionalSettlement(cond payments.Conditional, actionState, resolve *cell.Cell, locked map[string]*payments.LockedDepositInfo) (*cell.Cell, error) {
	if _, ok := cond.(*conditionals.ConditionalResolvable); ok {
		var state conditionals.ResolvableState
		if err := payments.LoadState(&state, resolve); err != nil {
			return nil, fmt.Errorf("failed to load resolve state: %w", err)
		}
		if state.Amount.Sign() == 0 {
			return actionState, nil
		}
	}
	return cond.Execute(actionState, resolve, locked)
}
