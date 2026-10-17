package dashboard

import (
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// settingRow returns the HTML of one row of the settings table.
func settingRowHTML(t *testing.T, body, key string) string {
	t.Helper()
	return regexpFind(t, `(?s)(<tr[^>]*id="setting-`+key+`".*?</tr>)`, body)
}

func TestSettingsRenderTypedControls(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		org := e.pageOK(admin, "/dashboard/settings")
		appTab := e.pageOK(admin, "/dashboard/apps/"+app.ID+"?tab=settings")

		// integer: a number input with the validator's bounds, a unit, help and the default
		row := settingRowHTML(t, org, "payment_ttl")
		has(t, row, `type="number"`, `min="10"`, `max="10080"`, `value="60"`, "minutes", "Default: 60 minutes", "stays payable")
		lacks(t, row, "<textarea")

		// enum: a select with the current option chosen
		row = settingRowHTML(t, org, "duplicate_payment_action")
		has(t, row, `<select`, `name="value"`, `<option value="review" selected`, `<option value="auto_refund"`, "Default: review")
		lacks(t, row, "<textarea", `type="number"`)

		// bool: a checkbox (with a hidden false for the unticked case)
		row = settingRowHTML(t, appTab, "issue_invoices")
		has(t, row, `type="checkbox"`, `name="value" value="true" checked`, `type="hidden" name="value" value="false"`, "Default: on")

		// durations: a comma separated box with units
		row = settingRowHTML(t, org, "dunning_schedule")
		has(t, row, `value="24, 72, 120"`, "hours", "Default: 24, 72, 120 hours")
		row = settingRowHTML(t, org, "webhook_retry_schedule")
		has(t, row, `value="0, 60, 300, 1800, 7200, 21600, 43200, 86400, 86400"`, "seconds")

		// paise are shown with the rupees
		row = settingRowHTML(t, org, "mandate_afa_limit")
		has(t, row, `value="1500000"`, "paise", "15,000.00")

		// clock and float and string
		has(t, settingRowHTML(t, org, "recon_time"), `type="time"`, `value="03:00"`)
		has(t, settingRowHTML(t, org, "mandate_headroom"), `type="number"`, `step="any"`, `min="1"`, `max="5"`, `value="1.5"`)
		has(t, settingRowHTML(t, appTab, "sac_code"), `type="text"`, `pattern="^[0-9]{6}$"`, `value="998315"`)

		// documents stay JSON, in a wide monospace box
		row = settingRowHTML(t, org, "upi_execution_windows")
		has(t, row, `<textarea class="json wide"`, `name="value"`)
		has(t, settingRowHTML(t, appTab, "routing"), `<textarea class="json wide"`)
		has(t, settingRowHTML(t, org, "pay_page_retry_buttons"), `<textarea class="json wide"`)

		// every row says what it is
		for _, key := range []string{"payment_ttl", "recon_time", "mandate_validity", "document_retention"} {
			if !strings.Contains(settingRowHTML(t, org, key), `class="help"`) {
				t.Errorf("%s: no help text", key)
			}
		}
		// read-only users get values, not controls
		viewer := e.roleClient("viewer")
		vrow := settingRowHTML(t, e.pageOK(viewer, "/dashboard/settings"), "payment_ttl")
		has(t, vrow, "60", "minutes")
		lacks(t, vrow, `<input`, `<select`, `<textarea`)
	})
}

func TestTypedFormValuesBecomeTheRightJSON(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/settings"))
		set := func(path string, vals ...string) int {
			r, _ := e.post(admin, path, url.Values{"value": vals, "csrf": {tok}})
			return r.StatusCode
		}
		raw := func(key string) string {
			v, err := e.set.ResolveOrg(ctx, store.DefaultOrgID, key)
			if err != nil {
				t.Fatal(err)
			}
			return string(v.Raw)
		}
		cases := []struct {
			key  string
			form []string
			want string
		}{
			{"payment_ttl", []string{"45"}, "45"},
			{"duplicate_payment_action", []string{"auto_refund"}, `"auto_refund"`},
			{"recon_time", []string{"04:30"}, `"04:30"`},
			{"dunning_schedule", []string{"12, 48"}, "[12,48]"},
			{"dunning_schedule", []string{""}, "[]"},
			{"webhook_retry_schedule", []string{"0 30 600"}, "[0,30,600]"},
			{"mandate_headroom", []string{"2.5"}, "2.5"},
			{"document_retention", []string{"0"}, "0"},
			{"upi_execution_windows", []string{`[{"start":"09:00","end":"10:00"}]`}, `[{"start":"09:00","end":"10:00"}]`},
			{"duplicate_payment_action", []string{`"review"`}, `"review"`}, // raw JSON is still accepted
		}
		for _, c := range cases {
			if code := set("/dashboard/settings/"+c.key, c.form...); code != 303 {
				t.Errorf("%s=%v: %d", c.key, c.form, code)
				continue
			}
			if got := raw(c.key); got != c.want {
				t.Errorf("%s=%v stored %s, want %s", c.key, c.form, got, c.want)
			}
		}
		// a checkbox posts a hidden false and, when ticked, a true after it
		appPath := "/dashboard/apps/" + app.ID + "/settings/issue_invoices"
		if code := set(appPath, "false", "true"); code != 303 {
			t.Fatalf("tick: %d", code)
		}
		if v, _ := e.set.Resolve(ctx, app.ID, "issue_invoices"); string(v.Raw) != "true" {
			t.Fatalf("ticked: %s", v.Raw)
		}
		if code := set(appPath, "false"); code != 303 {
			t.Fatalf("untick: %d", code)
		}
		if v, _ := e.set.Resolve(ctx, app.ID, "issue_invoices"); string(v.Raw) != "false" {
			t.Fatalf("unticked: %s", v.Raw)
		}
		// mistakes are still the validator's to refuse
		for _, c := range []struct {
			key  string
			form []string
		}{
			{"payment_ttl", []string{"5"}}, {"payment_ttl", []string{"soon"}}, {"payment_ttl", []string{"12.5"}},
			{"duplicate_payment_action", []string{"maybe"}}, {"recon_time", []string{"25:00"}},
			{"dunning_schedule", []string{"a, b"}}, {"dunning_schedule", []string{"1,2,3,4,5,6,7"}},
			{"mandate_headroom", []string{"9"}}, {"document_retention", []string{"3"}},
		} {
			if code := set("/dashboard/settings/"+c.key, c.form...); code != 400 {
				t.Errorf("%s=%v: %d", c.key, c.form, code)
			}
		}
		if raw("payment_ttl") != "45" {
			t.Fatalf("a refused value changed the setting: %s", raw("payment_ttl"))
		}
	})
}
