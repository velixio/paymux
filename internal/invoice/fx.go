package invoice

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
	"github.com/velixio/paymux/pkg/connector/money"
)

// parseRate parses a positive decimal rate such as "83.2500".
func parseRate(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 || len(s) > 20 {
		return nil, invalid("%q is not a positive decimal rate", s)
	}
	return r, nil
}

// ToINR converts minor units of currency to paise at rate (INR per unit),
// rounding half up.
func ToINR(minor int64, currency, rate string) (int64, error) {
	if currency == "INR" {
		return minor, nil
	}
	exp, err := money.Exponent(currency)
	if err != nil {
		return 0, err
	}
	r, err := parseRate(rate)
	if err != nil {
		return 0, err
	}
	// paise = minor / 10^exp * rate * 100
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(minor), r)
	v.Mul(v, big.NewRat(100, 1))
	v.Quo(v, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)))
	// round half up for non-negative values
	n := new(big.Int).Mul(v.Num(), big.NewInt(2))
	n.Add(n, v.Denom())
	n.Quo(n, new(big.Int).Mul(v.Denom(), big.NewInt(2)))
	if !n.IsInt64() {
		return 0, fmt.Errorf("invoice: INR value out of range")
	}
	return n.Int64(), nil
}

// inrAmounts converts a document's amounts to paise. ok is false while the
// FX rate is pending.
func inrAmounts(a tax.Amounts, currency, rate string) (tax.Amounts, bool, error) {
	if currency == "INR" {
		return a, true, nil
	}
	if rate == "" {
		return tax.Amounts{}, false, nil
	}
	var out tax.Amounts
	for _, c := range []struct {
		in  int64
		out *int64
	}{{a.Taxable, &out.Taxable}, {a.CGST, &out.CGST}, {a.SGST, &out.SGST}, {a.IGST, &out.IGST}} {
		v, err := ToINR(c.in, currency, rate)
		if err != nil {
			return tax.Amounts{}, false, err
		}
		*c.out = v
	}
	out.Total = out.Taxable + out.CGST + out.SGST + out.IGST
	return out, true, nil
}

// SetFXRate records the RBI/FBIL reference rate (INR per 1 unit of currency)
// for an IST calendar date (YYYY-MM-DD). Invoices of that day that were issued
// with fx_pending get the rate, lose the flag and are rendered again.
func (s *Service) SetFXRate(ctx context.Context, actor audit.Actor, currency, date, rate, source string) error {
	if _, err := money.Exponent(currency); err != nil || currency == "INR" {
		return invalid("%q is not a foreign currency", currency)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return invalid("date %q must be YYYY-MM-DD", date)
	}
	if _, err := parseRate(rate); err != nil {
		return err
	}
	if source != "manual" && source != "provider" {
		return invalid("source %q must be manual or provider", source)
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		now := s.now().UTC()
		if err := store.UpsertFXRate(ctx, tx, store.FXRate{OrgID: store.DefaultOrgID, Currency: currency, Date: date, Rate: rate, Source: source}, now); err != nil {
			return err
		}
		pending, err := store.InvoicesPendingFX(ctx, tx, store.DefaultOrgID, currency)
		if err != nil {
			return err
		}
		for _, inv := range pending {
			if inv.IssuedAt.In(tax.IST).Format("2006-01-02") != date {
				continue
			}
			flags := slices.DeleteFunc(slices.Clone(inv.Flags), func(f string) bool { return f == FlagFXPending })
			if err := store.SetInvoiceFX(ctx, tx, inv.ID, rate, source, flags); err != nil {
				return err
			}
			if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRender, Payload: map[string]string{"kind": kindInvoice, "id": inv.ID},
				DedupeKey: JobRender + ":fx:" + inv.ID}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: store.DefaultOrgID, Actor: actor, Action: "fx_rate.set",
			Target: currency + "/" + date, After: map[string]string{"rate": rate, "source": source}})
	})
}
