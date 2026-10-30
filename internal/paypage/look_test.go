package paypage_test

import (
	"encoding/json"
	"io/fs"
	"net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/web"
)

func hindi(t *testing.T) map[string]string {
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

func (e *env) setApp(app, key, val string) {
	e.t.Helper()
	if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, app, key, json.RawMessage(val)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) canceled(appScope string) created {
	e.t.Helper()
	scope := e.scope
	scope.AppID = appScope
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: scope, Currency: "USD", Amount: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.core.CancelPayment(ctx, scope, audit.System, res.Payment.ID); err != nil {
		e.t.Fatal(err)
	}
	return created{res.Payment.ID, res.ClientSecret}
}

func TestResultPageUsesTheAppsCustomerLanguage(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		hi := hindi(t)
		e.setApp("app_1", "customer_language", `"hi"`)
		c1, c2 := e.canceled("app_1"), e.canceled("app_2")
		r := e.do("GET", c1.path(), "", nil)
		if !strings.Contains(r.body, hi["result.canceled.title"]) || !strings.Contains(r.body, `lang="hi"`) {
			t.Fatalf("app_1 should be in Hindi: %s", r.body)
		}
		r = e.do("GET", c2.path(), "", nil)
		if !strings.Contains(r.body, "Payment canceled") || !strings.Contains(r.body, `lang="en"`) {
			t.Fatalf("app_2 stays English: %s", r.body)
		}
	})
}

func TestLangQueryParameterIsAllowListed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		hi := hindi(t)
		c := e.canceled("app_2")
		if r := e.do("GET", c.path()+"&lang=hi", "", nil); !strings.Contains(r.body, hi["result.canceled.title"]) {
			t.Fatalf("?lang=hi: %s", r.body)
		}
		for _, bad := range []string{"zz", "../hi", "HI", "%3Cscript%3E", "hi.json"} {
			r := e.do("GET", c.path()+"&lang="+bad, "", nil)
			if !strings.Contains(r.body, "Payment canceled") || strings.Contains(r.body, "<script>") {
				t.Errorf("?lang=%s must be ignored: %s", bad, r.body)
			}
		}
		// An invalid link is translated too, with no payment known.
		if r := e.do("GET", "/pay/pay_missing?t=x&lang=hi", "", nil); r.status != 404 || !strings.Contains(r.body, hi["result.link_invalid.title"]) {
			t.Fatalf("404 page: %d %s", r.status, r.body)
		}
	})
}

func TestBillingFormAndItsErrorsAreTranslated(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		hi := hindi(t)
		e.setApp("app_1", "customer_language", `"hi"`)
		c := e.inrPayment(50000)
		page := e.do("GET", c.path(), "", nil)
		for _, key := range []string{"billing.name", "billing.state", "billing.gst_toggle", "billing.continue"} {
			if !strings.Contains(page.body, hi[key]) {
				t.Errorf("billing form lacks %q (%s)", hi[key], key)
			}
		}
		r := e.billingPost(c, func(f url.Values) { f.Set("name", " ") })
		if r.status != 400 || !strings.Contains(r.body, hi["billing.err.name"]) {
			t.Fatalf("validation error should be Hindi: %d %s", r.status, r.body)
		}
		// The language chosen by ?lang= survives the form round trip.
		e2 := e.inrPayment(50000)
		e.setApp("app_1", "customer_language", `"en"`)
		p := e.do("GET", e2.path()+"&lang=hi", "", nil)
		if !strings.Contains(p.body, "lang=hi") {
			t.Fatalf("the form should post back with lang=hi: %s", p.body)
		}
	})
}

func TestEachAppHasItsOwnTheme(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.setApp("app_1", "theme", `"minimal-dark"`)
		c1, c2 := e.canceled("app_1"), e.canceled("app_2")
		if r := e.do("GET", c1.path(), "", nil); !strings.Contains(r.body, `/theme/minimal-dark/theme.css`) {
			t.Fatalf("app_1 theme: %s", r.body)
		}
		if r := e.do("GET", c2.path(), "", nil); !strings.Contains(r.body, `/theme/default/theme.css`) || strings.Contains(r.body, "minimal-dark") {
			t.Fatalf("app_2 theme: %s", r.body)
		}
		// The org value is the default for apps that set none.
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "theme", json.RawMessage(`"minimal-dark"`)); err != nil {
			t.Fatal(err)
		}
		if r := e.do("GET", c2.path(), "", nil); !strings.Contains(r.body, `/theme/minimal-dark/theme.css`) {
			t.Fatalf("org theme should be inherited: %s", r.body)
		}
	})
}

func TestBrokenThemeFallsBackToTheEmbeddedLook(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.setApp("app_1", "theme", `"does-not-exist"`)
		r := e.do("GET", e.canceled("app_1").path(), "", nil)
		if r.status != 200 || strings.Contains(r.body, "/theme/") || !strings.Contains(r.body, "/assets/css/app.css") {
			t.Fatalf("missing theme: %d %s", r.status, r.body)
		}
	})
}
