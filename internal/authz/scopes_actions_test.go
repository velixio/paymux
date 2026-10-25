package authz

import (
	"slices"
	"testing"
)

func TestActionsForScopes(t *testing.T) {
	for _, c := range []struct {
		scopes []string
		want   []Action
	}{
		{nil, []Action{PaymentCreate, PaymentCancel, RefundCreate}},
		{[]string{ScopePaymentsRead}, nil},
		{[]string{ScopePaymentsRead, ScopeRefundsRead, ScopeInvoicesRead, ScopeSubscriptionsRead}, nil},
		{[]string{ScopeRefundsWrite}, []Action{RefundCreate}},
		{[]string{ScopePayoutsRead}, []Action{PayoutCreate}},
		{[]string{ScopePayoutsWrite, ScopePaymentsWrite}, []Action{PayoutCreate, PaymentCreate, PaymentCancel}},
		{[]string{ScopeSubscriptionsWrite, ScopePaymentsWrite}, []Action{PaymentCreate, PaymentCancel}},
	} {
		got := ActionsForScopes(c.scopes)
		if !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v want %v", c.scopes, got, c.want)
		}
	}
}
