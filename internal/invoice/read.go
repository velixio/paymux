package invoice

import (
	"context"
	"errors"
	"fmt"

	"github.com/velixio/paymux/internal/store"
)

// ErrNotFound is returned for unknown documents and for documents outside the
// caller's app and mode; it wraps store.ErrNotFound.
var ErrNotFound = fmt.Errorf("invoice: %w", store.ErrNotFound)

// ErrPDFNotReady means the document exists but its PDF is not rendered yet.
var ErrPDFNotReady = errors.New("invoice: the PDF is not ready yet")

// GetInvoice returns an invoice of the app in the mode.
func (s *Service) GetInvoice(ctx context.Context, appID, mode, id string) (store.Invoice, error) {
	var inv store.Invoice
	err := s.db.Read(ctx, func(q store.Querier) (err error) { inv, err = store.GetInvoice(ctx, q, id); return })
	if err != nil || inv.AppID != appID || inv.Mode != mode {
		return store.Invoice{}, ErrNotFound
	}
	return inv, nil
}

// ListInvoices lists the invoices of a payment of the app in the mode.
func (s *Service) ListInvoices(ctx context.Context, appID, mode, paymentID string) ([]store.Invoice, error) {
	var all []store.Invoice
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { all, err = store.InvoicesForPayment(ctx, q, paymentID); return }); err != nil {
		return nil, err
	}
	out := []store.Invoice{}
	for _, i := range all {
		if i.AppID == appID && i.Mode == mode {
			out = append(out, i)
		}
	}
	return out, nil
}

// GetCreditNote returns a credit note of the app in the mode.
func (s *Service) GetCreditNote(ctx context.Context, appID, mode, id string) (store.CreditNote, error) {
	var c store.CreditNote
	err := s.db.Read(ctx, func(q store.Querier) (err error) { c, err = store.GetCreditNote(ctx, q, id); return })
	if err != nil || c.AppID != appID || c.Mode != mode {
		return store.CreditNote{}, ErrNotFound
	}
	return c, nil
}

// ListCreditNotes lists the credit notes of a payment of the app in the mode.
func (s *Service) ListCreditNotes(ctx context.Context, appID, mode, paymentID string) ([]store.CreditNote, error) {
	var all []store.CreditNote
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { all, err = store.CreditNotesForPayment(ctx, q, paymentID); return }); err != nil {
		return nil, err
	}
	out := []store.CreditNote{}
	for _, c := range all {
		if c.AppID == appID && c.Mode == mode {
			out = append(out, c)
		}
	}
	return out, nil
}

// PDF returns the rendered PDF bytes of a stored blob id (ErrPDFNotReady when empty).
func (s *Service) PDF(ctx context.Context, blobID string) ([]byte, error) {
	if blobID == "" {
		return nil, ErrPDFNotReady
	}
	var b store.Blob
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { b, err = store.GetBlob(ctx, q, blobID); return }); err != nil {
		return nil, err
	}
	return b.Data, nil
}
