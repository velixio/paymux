package invoice_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func pdfOf(t *testing.T, e *env, blobID string) []byte {
	t.Helper()
	if blobID == "" {
		t.Fatal("no pdf blob")
	}
	var b store.Blob
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { b, err = store.GetBlob(ctx, q, blobID); return }); err != nil {
		t.Fatal(err)
	}
	if b.ContentType != "application/pdf" || !bytes.HasPrefix(b.Data, []byte("%PDF-")) {
		t.Fatalf("blob %+v", b)
	}
	return b.Data
}

func TestRenderJobStoresThePDF(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		if only(t, e, p.ID).PDFBlobID != "" {
			t.Fatal("pdf exists before the job ran")
		}
		e.runJobs()
		pdfOf(t, e, only(t, e, p.ID).PDFBlobID)
		e.refund(p, 10000)
		e.runJobs()
		pdfOf(t, e, e.creditNotes(p.ID)[0].PDFBlobID)
		if e.mailer.count() != 0 {
			t.Error("default delivery (app) must not email")
		}
	})
}

func TestEmailOnlyWhenDeliveryIsPaymuxOrBoth(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)

		e.setDelivery("app")
		e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		if e.mailer.count() != 0 {
			t.Fatalf("delivery=app sent %d emails", e.mailer.count())
		}

		e.setDelivery("paymux")
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		if e.mailer.count() != 1 {
			t.Fatalf("delivery=paymux sent %d emails", e.mailer.count())
		}
		m := e.mailer.sent[0]
		inv := only(t, e, p.ID)
		if len(m.To) != 1 || m.To[0] != "asha@example.com" || len(m.Attachments) != 1 ||
			m.Attachments[0].Filename != "INV-2627-000002.pdf" || !bytes.HasPrefix(m.Attachments[0].Data, []byte("%PDF-")) ||
			!bytes.Equal(m.Attachments[0].Data, pdfOf(t, e, inv.PDFBlobID)) {
			t.Errorf("message = %+v", m)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			sends, _ := store.DocumentSends(ctx, q, "invoice", inv.ID)
			if len(sends) != 1 || sends[0].Status != "sent" || sends[0].Recipient != "asha@example.com" {
				t.Errorf("sends = %+v", sends)
			}
			return nil
		})
		// A second render run (a retry) must not email again.
		e.runJobs()
		if e.mailer.count() != 1 {
			t.Errorf("emails after re-run = %d", e.mailer.count())
		}

		e.setDelivery("both")
		e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		if e.mailer.count() != 2 {
			t.Errorf("delivery=both: emails = %d", e.mailer.count())
		}

		// Credit notes follow the same rule.
		e.refund(p, 5000)
		e.runJobs()
		if e.mailer.count() != 3 || e.mailer.sent[2].Attachments[0].Filename != "CN-2627-000001.pdf" {
			t.Errorf("credit note email missing: %d", e.mailer.count())
		}

		// No customer email: nothing to send.
		e.pay(118000, "INR", &core.CustomerInput{Name: "No Mail", Billing: store.Billing{StateCode: "27", Country: "IN"}}, "pa_1")
		e.runJobs()
		if e.mailer.count() != 3 {
			t.Errorf("emailed a customer without an address")
		}
	})
}

func TestFailedSendIsRecordedAndRetried(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setDelivery("paymux")
		e.mailer.err = errors.New("smtp down")
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs() // the job fails and is rescheduled with backoff
		inv := only(t, e, p.ID)
		pdfOf(t, e, inv.PDFBlobID) // the PDF is stored even though delivery failed
		_ = db.Read(ctx, func(q store.Querier) error {
			sends, _ := store.DocumentSends(ctx, q, "invoice", inv.ID)
			if len(sends) == 0 || sends[0].Status != "failed" || sends[0].Error == "" {
				t.Errorf("sends = %+v", sends)
			}
			return nil
		})
	})
}

func TestFXPendingInvoiceRendersOnceTheRateIsEntered(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		e.runJobs()
		inv := only(t, e, p.ID)
		if !inv.HasFlag("fx_pending") || inv.PDFBlobID != "" {
			t.Fatalf("pending invoice rendered early: %+v", inv)
		}
		e.setFX("USD", "2026-10-17", "83.2500")
		e.runJobs()
		inv = only(t, e, p.ID)
		if inv.HasFlag("fx_pending") || inv.FXRate != "83.2500" || inv.FXSource != "manual" {
			t.Errorf("after rate: %+v", inv)
		}
		pdfOf(t, e, inv.PDFBlobID)
	})
}
