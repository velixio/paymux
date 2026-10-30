package subscriptions_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/subscriptions"
	"github.com/velixio/paymux/pkg/connector"
)

func TestPredebitEmailCarriesTheRBIContents(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		if n := len(e.mailer.messages()); n != 0 {
			t.Fatalf("emails before the notice = %d", n)
		}
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		msgs := e.mailer.messages()
		if len(msgs) != 1 || len(e.r.notices) != 1 {
			t.Fatalf("emails=%d provider notices=%d (the email is in addition to the provider's notice)", len(msgs), len(e.r.notices))
		}
		m := msgs[0]
		ref := "pm_" + e.mandate(e.sub(sub.ID).MandateID).ID
		if len(m.To) != 1 || m.To[0] != "asha@example.com" {
			t.Errorf("to = %v", m.To)
		}
		if !strings.Contains(m.Subject, "INR 499.00") || !strings.Contains(m.Subject, "Voice Studio Pvt Ltd") || !strings.Contains(m.Subject, "28 Feb 2027") {
			t.Errorf("subject = %q", m.Subject)
		}
		for _, want := range []string{"Voice Studio Pvt Ltd", "INR 499.00", "28 Feb 2027", ref, "Renewal of your subscription: Pro", "Hello Asha", "cancel the mandate"} {
			if !strings.Contains(m.Text, want) {
				t.Errorf("email lacks %q:\n%s", want, m.Text)
			}
		}
		if strings.ContainsAny(m.Subject, "\r\n") {
			t.Error("multi-line subject")
		}
		// No second email for the same notice when the job is repeated.
		e.runJobs()
		if n := len(e.mailer.messages()); n != 1 {
			t.Errorf("emails after a repeat = %d", n)
		}
	})
}

func TestPredebitEmailRespectsTheSettingAndTheAddress(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.setSetting("predebit_email", "false")
		active(t, e, nil)
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if len(e.mailer.messages()) != 0 || len(e.r.notices) != 1 {
			t.Fatalf("setting off: emails=%d provider notices=%d", len(e.mailer.messages()), len(e.r.notices))
		}

		// On again, but a customer without an email address gets none.
		e.setSetting("predebit_email", "true")
		noMail := &core.CustomerInput{ExternalRef: "u2", Name: "Ravi", Phone: "9999999999", Billing: store.Billing{StateCode: "29", Country: "IN"}}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Customer = noMail; i.Interval = "day" })
		e.activateMandate(c)
		e.drive(25*hour, hour)
		if len(e.mailer.messages()) != 0 {
			t.Fatalf("emailed a customer without an address: %+v", e.mailer.messages())
		}
		if len(e.r.notices) < 2 {
			t.Errorf("the provider notice must go out anyway: %d", len(e.r.notices))
		}
	})
}

func TestPredebitEmailOnlyAfterTheProviderNotice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		active(t, e, nil)
		e.r.noticeErr = errors.New("provider down")
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if len(e.mailer.messages()) != 0 {
			t.Fatal("emailed although the provider notice failed")
		}
		e.r.mu.Lock()
		e.r.noticeErr = nil
		e.r.mu.Unlock()
		e.drive(2*hour, 10*60*1e9)
		if n := len(e.mailer.messages()); n != 1 {
			t.Errorf("emails after the retry = %d", n)
		}
	})
}

// A provider without a notice API (Zoho) still gets the customer email.
func TestPredebitEmailWhenTheProviderHasNoNoticeAPI(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		active(t, e, nil)
		e.r.noticeErr = connector.ErrUnsupported
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if n := len(e.mailer.messages()); n != 1 {
			t.Errorf("emails = %d", n)
		}
	})
}

func TestPredebitEmailTemplateCanBeOverridden(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.subs.SetTemplates(fstest.MapFS{"templates/email_predebit.txt": {Data: []byte(
			"Subject: Heads up {{.Merchant}}\r\n\r\nPay {{.Amount}} on {{.DebitDate}} ({{.MandateRef}}) for {{.Reason}}.\n")}})
		active(t, e, nil)
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		msgs := e.mailer.messages()
		if len(msgs) != 1 || msgs[0].Subject != "Heads up Voice Studio Pvt Ltd" || !strings.HasPrefix(msgs[0].Text, "Pay INR 499.00 on 28 Feb 2027 (pm_mdt_") {
			t.Fatalf("messages = %+v", msgs)
		}
		// A broken override sends nothing but never blocks the provider notice.
		e.subs.SetTemplates(fstest.MapFS{"templates/email_predebit.txt": {Data: []byte("Subject: x\n\n{{.NoSuchField}}")}})
		c := e.mustCreate(nil)
		e.activateMandate(c)
		e.advanceTo(utc(2027, 2, 26, 4, 0))
	})
}
