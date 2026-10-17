package subscriptions_test

import (
	"encoding/json"
	"io/fs"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/web"
)

func hindiCatalog(t *testing.T) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(web.Embedded(), "i18n/hi.json")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHostedCheckoutFollowsTheCustomerLanguage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		hi := hindiCatalog(t)
		srv := hostedServer(e)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		target := srv.URL + u.RequestURI()

		if _, body, _ := fetch(t, target); !strings.Contains(body, "Taking you to the secure payment page") || !strings.Contains(body, `lang="en"`) {
			t.Fatalf("English by default: %s", body)
		}
		// ?lang= picks an installed language and nothing else.
		if _, body, _ := fetch(t, target+"&lang=hi"); !strings.Contains(body, hi["redirect.taking"]) || !strings.Contains(body, `lang="hi"`) {
			t.Fatalf("?lang=hi: %s", body)
		}
		if _, body, _ := fetch(t, target+"&lang=zz"); !strings.Contains(body, `lang="en"`) {
			t.Fatalf("?lang=zz must be ignored: %s", body)
		}
		// The app setting applies without the parameter.
		e.setSetting("customer_language", `"hi"`)
		if _, body, _ := fetch(t, target); !strings.Contains(body, hi["redirect.taking"]) {
			t.Fatalf("customer_language=hi: %s", body)
		}
		// Result pages too.
		e.activateMandate(c)
		if _, body, _ := fetch(t, target); !strings.Contains(body, hi["subscription.active.title"]) {
			t.Fatalf("active page: %s", body)
		}
	})
}

func TestPredebitEmailInTheCustomerLanguage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		hi := hindiCatalog(t)
		e.setSetting("customer_language", `"hi"`)
		active(t, e, nil)
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		msgs := e.mailer.messages()
		if len(msgs) != 1 {
			t.Fatalf("emails = %d", len(msgs))
		}
		m := msgs[0]
		wantSubject := strings.NewReplacer("%s", "\x00").Replace(hi["predebit.subject"])
		if !strings.HasPrefix(m.Subject, strings.SplitN(wantSubject, "\x00", 2)[0]) && !strings.Contains(m.Subject, "आगामी") {
			t.Errorf("subject should be Hindi: %q", m.Subject)
		}
		for _, want := range []string{"नमस्ते Asha", hi["predebit.merchant"], hi["predebit.footer"], "INR 499.00", "28 Feb 2027", hi["predebit.reason_renewal"] + ": Pro"} {
			if !strings.Contains(m.Text, want) {
				t.Errorf("email lacks %q:\n%s", want, m.Text)
			}
		}
		if strings.Contains(m.Text, "Hello") || strings.Contains(m.Text, "reminder from") {
			t.Errorf("English left in the Hindi email:\n%s", m.Text)
		}
		if strings.ContainsAny(m.Subject, "\r\n") {
			t.Error("multi-line subject")
		}
	})
}
