package tax

import (
	"errors"
	"math/big"
)

// MaxAmount bounds totals so intermediate products stay well inside int64.
const MaxAmount int64 = 400_000_000_000_000

// Amounts are the GST heads of a document, in minor units.
type Amounts struct {
	Taxable, CGST, SGST, IGST, Total int64
}

// Tax returns the total tax.
func (a Amounts) Tax() int64 { return a.CGST + a.SGST + a.IGST }

// Add returns the field-wise sum.
func (a Amounts) Add(b Amounts) Amounts {
	return Amounts{a.Taxable + b.Taxable, a.CGST + b.CGST, a.SGST + b.SGST, a.IGST + b.IGST, a.Total + b.Total}
}

var errAmount = errors.New("tax: amount out of range")
var errRate = errors.New("tax: rate must be 0..10000 basis points")

func check(v int64, rateBP int) error {
	if v < 0 || v > MaxAmount {
		return errAmount
	}
	if rateBP < 0 || rateBP > 10000 {
		return errRate
	}
	return nil
}

// mulDivRound returns round_half_up(a*b/c) for non-negative a, b and positive c.
func mulDivRound(a, b, c int64) int64 {
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	n.Mul(n, big.NewInt(2))
	n.Add(n, big.NewInt(c))
	n.Quo(n, new(big.Int).Mul(big.NewInt(c), big.NewInt(2)))
	return n.Int64()
}

func split(taxable, tax int64, interState bool) Amounts {
	a := Amounts{Taxable: taxable, Total: taxable + tax}
	if interState {
		a.IGST = tax
	} else {
		a.CGST = tax / 2
		a.SGST = tax - a.CGST
	}
	return a
}

// FromTotal derives the heads from a tax-inclusive total. zeroRated (export
// under LUT) means no tax is charged.
func FromTotal(total int64, rateBP int, interState, zeroRated bool) (Amounts, error) {
	if err := check(total, rateBP); err != nil {
		return Amounts{}, err
	}
	if zeroRated || rateBP == 0 {
		return Amounts{Taxable: total, Total: total}, nil
	}
	taxable := mulDivRound(total, 10000, int64(10000+rateBP))
	return split(taxable, total-taxable, interState), nil
}

// FromTaxable derives the heads from a tax-exclusive taxable value.
func FromTaxable(taxable int64, rateBP int, interState, zeroRated bool) (Amounts, error) {
	if err := check(taxable, rateBP); err != nil {
		return Amounts{}, err
	}
	if zeroRated || rateBP == 0 {
		return Amounts{Taxable: taxable, Total: taxable}, nil
	}
	return split(taxable, mulDivRound(taxable, int64(rateBP), 10000), interState), nil
}

// ErrCreditExceedsInvoice means credits would exceed the invoice total.
var ErrCreditExceedsInvoice = errors.New("tax: credit exceeds invoice total")

// CreditSplit apportions a refund of `refund` minor units across the invoice's
// heads, given credits already issued. Cumulative credits are computed in
// proportion to the invoice so that crediting the full total reproduces the
// invoice exactly; each credit's heads sum to the refund.
func CreditSplit(inv Amounts, refund int64, prior []Amounts) (Amounts, error) {
	if refund <= 0 {
		return Amounts{}, errAmount
	}
	var done Amounts
	for _, p := range prior {
		done = done.Add(p)
	}
	cumTotal := done.Total + refund
	if cumTotal > inv.Total || inv.Total == 0 {
		return Amounts{}, ErrCreditExceedsInvoice
	}
	cumTaxable := mulDivRound(inv.Taxable, cumTotal, inv.Total)
	cumTax := cumTotal - cumTaxable
	cum := Amounts{Taxable: cumTaxable, Total: cumTotal}
	switch invTax := inv.Tax(); {
	case invTax == 0:
		cum.Taxable = cumTotal
	case inv.IGST > 0:
		cum.IGST = cumTax
	default:
		cum.CGST = mulDivRound(cumTax, inv.CGST, invTax)
		cum.SGST = cumTax - cum.CGST
	}
	return Amounts{
		Taxable: cum.Taxable - done.Taxable,
		CGST:    cum.CGST - done.CGST,
		SGST:    cum.SGST - done.SGST,
		IGST:    cum.IGST - done.IGST,
		Total:   refund,
	}, nil
}
