package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/recon"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// settlementLookback is how far before the window settlements are listed:
// payments of the window settle a few days after they were captured.
const settlementLookback = 5 * 24 * time.Hour

// reconcileSettlements upserts the provider's settlements (spec section 11):
// it attaches each payment's fee, the GST on the fee and the net amount to the
// attempt, writes the fee, fee_tax and payout ledger entries, and flags a
// settlement whose fee totals differ from the sum of its payments' fees. It is
// idempotent: a fee is attached and a payout recorded once.
func (s *Service) reconcileSettlements(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, conn connector.Connector) error {
	settler, ok := conn.(connector.Settler)
	if !ok {
		return nil
	}
	list, err := settler.ListSettlements(ctx, connector.TimeRange{From: run.WindowFrom.Add(-settlementLookback).UnixMilli(), To: run.WindowTo.UnixMilli()})
	if err != nil {
		return fmt.Errorf("listing settlements: %w", err)
	}
	run.Checked += len(list)
	for _, snap := range list {
		if err := s.applySettlement(ctx, pa, run, snap); err != nil {
			return fmt.Errorf("settlement %s: %w", snap.ProviderSettlementID, err)
		}
	}
	return nil
}

func (s *Service) applySettlement(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, snap connector.SettlementSnapshot) error {
	var (
		row            store.Settlement
		ourFee, ourTax int64
		checked        bool
		mismatch       bool
		wasFlagged     bool
	)
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		now := s.now()
		in := store.Settlement{ID: ids.New("stl"), OrgID: pa.OrgID, Mode: pa.Mode, ProviderAccountID: pa.ID,
			ProviderSettlementID: snap.ProviderSettlementID, Amount: snap.Amount, Fees: snap.Fees, FeeTax: snap.Tax, Currency: snap.Currency,
			Status: snap.Status, UTR: snap.UTR}
		if snap.SettledAt > 0 {
			in.SettledAt = time.UnixMilli(snap.SettledAt).UTC()
		}
		var err error
		if row, err = store.UpsertSettlement(ctx, tx, in, now); err != nil {
			return err
		}
		wasFlagged = row.FeeMismatch
		var itemFees, itemTax int64
		for _, it := range snap.Items {
			itemFees, itemTax = itemFees+it.Fee, itemTax+it.Tax
			if it.Kind != "payment" || it.ProviderPaymentID == "" {
				continue
			}
			a, err := store.FindAttempt(ctx, tx, pa.ID, "", "", it.ProviderPaymentID)
			if errors.Is(err, store.ErrNotFound) {
				continue // not a payment of ours: the payment reconciliation reports it
			}
			if err != nil {
				return err
			}
			if !strings.EqualFold(a.CurrencyReceived, snap.Currency) {
				// A fee in another currency than the payment it is charged on cannot
				// be booked against it. The settlement's fee check then differs from
				// ours and is reported.
				s.o.Logger.Warn("reconcile: settlement line in another currency than the payment; fees not booked",
					"settlement", snap.ProviderSettlementID, "attempt", a.ID, "payment_currency", a.CurrencyReceived, "settlement_currency", snap.Currency)
				continue
			}
			attached, err := store.AttachSettlementFees(ctx, tx, a.ID, row.ID, it.Fee, it.Tax, it.Amount-it.Fee-it.Tax)
			if err != nil {
				return err
			}
			if !attached {
				continue
			}
			for _, e := range []struct {
				kind   string
				amount int64
			}{{"fee", -it.Fee}, {"fee_tax", -it.Tax}} {
				if e.amount == 0 {
					continue
				}
				if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: a.OrgID, Mode: a.Mode, Kind: e.kind,
					Amount: e.amount, Currency: snap.Currency, PaymentID: a.PaymentID, SettlementID: row.ID, AttemptID: a.ID, OccurredAt: now}); err != nil {
					return err
				}
			}
		}
		if snap.Status == "processed" {
			first, err := store.MarkPayoutRecorded(ctx, tx, row.ID)
			if err != nil {
				return err
			}
			if first && snap.Amount != 0 {
				if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: pa.OrgID, Mode: pa.Mode, Kind: "payout",
					Amount: -snap.Amount, Currency: snap.Currency, SettlementID: row.ID, OccurredAt: now}); err != nil {
					return err
				}
			}
		}
		if len(snap.Items) == 0 {
			return nil // the connector gave no lines: nothing to check
		}
		checked = true
		if ourFee, ourTax, err = store.SumSettlementAttemptFees(ctx, tx, row.ID); err != nil {
			return err
		}
		_, ok := recon.CheckSettlementFees(recon.Settlement{ID: snap.ProviderSettlementID, Fees: snap.Fees, FeeTax: snap.Tax},
			[]int64{ourFee}, []int64{ourTax})
		mismatch = !ok
		return store.SetSettlementFeeCheck(ctx, tx, row.ID, itemFees, itemTax, mismatch)
	})
	if err != nil {
		return err
	}
	if !checked {
		return nil
	}
	if !mismatch {
		if wasFlagged {
			return s.o.DB.Write(ctx, func(tx store.Tx) error {
				return store.CloseReconItems(ctx, tx, pa.ID, recon.KindFeeMismatch, snap.ProviderSettlementID)
			})
		}
		return nil
	}
	detail := fmt.Sprintf("Settlement %s of %s account %s reports fees %d and fee GST %d, but the fees of its payments add up to %d and %d",
		snap.ProviderSettlementID, pa.Connector, pa.Name, snap.Fees, snap.Tax, ourFee, ourTax)
	return s.record(ctx, pa, run, recon.Item{Kind: recon.KindFeeMismatch, ProviderID: snap.ProviderSettlementID,
		Ours:   &recon.Record{ProviderID: snap.ProviderSettlementID, Amount: ourFee, Currency: snap.Currency},
		Theirs: recon.Record{ProviderID: snap.ProviderSettlementID, Amount: snap.Fees, Currency: snap.Currency}}, false, recordNote{detail: detail})
}
