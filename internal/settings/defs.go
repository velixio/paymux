package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/velixio/paymux/internal/invoice/irp"
	"github.com/velixio/paymux/internal/routing"
)

// Level says where a setting may be set (spec section 8a, "Level" column).
type Level int

const (
	LevelOrg Level = 1 << iota
	LevelApp
)

// Def describes one setting: its built-in default (a JSON document) and the
// check that enforces the "Allowed" column.
type Def struct {
	Key      string
	Default  string
	Levels   Level
	Validate func(json.RawMessage) error
}

// adminOnlyKeys are the settings only the built-in admin may change: they set
// how long a stolen session lives and which keys vouch for an e-invoice, so a
// custom role holding settings.write must not reach them.
var adminOnlyKeys = []string{"session_idle_hours", "session_max_days", "session_remember_days", "einvoice_irp_keys"}

// AdminOnly reports whether key may be changed by an admin only.
func AdminOnly(key string) bool { return slices.Contains(adminOnlyKeys, key) }

// Defs returns every setting definition, ordered by key.
func Defs() []Def {
	out := slices.Clone(defs)
	slices.SortFunc(out, func(a, b Def) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	return out
}

// Lookup returns the definition of key.
func Lookup(key string) (Def, bool) {
	for _, d := range defs {
		if d.Key == key {
			return d, true
		}
	}
	return Def{}, false
}

const orgApp = LevelOrg | LevelApp

// defaultRouting is spec section 8: INR to razorpay, zoho, payu; BDT to
// razorpay, whop; everything else to razorpay, whop.
const defaultRouting = `{"rules":[` +
	`{"name":"inr-default","when":{"currency":["INR"]},"action":{"kind":"route","targets":[{"connector":"razorpay"},{"connector":"zoho"},{"connector":"payu"}]}},` +
	`{"name":"bdt-default","when":{"currency":["BDT"]},"action":{"kind":"route","targets":[{"connector":"razorpay"},{"connector":"whop"}]}}],` +
	`"catch_all":{"name":"catch-all","when":{},"action":{"kind":"route","targets":[{"connector":"razorpay"},{"connector":"whop"}]}}}`

// The built-in defaults live here, in code (spec section 8a). They are copied
// into the org values on first start.
//
// Notes on rows of the table that are not settings here:
//   - pdf_signing (tax-entity level) is not a setting: a PDF is signed
//     exactly when the tax entity holds a digital signature certificate
//     (internal/invoice, Service.SetTaxEntityDSC), else the signatory note is
//     printed. Removing the certificate is how to choose the note.
//   - billing_details defaults to "off" when invoicing is off: the default here
//     is the invoicing-on value and the pay page applies the conditional
//     (internal/paypage billingNeed).
//   - invoice and credit-note prefixes also exist per tax entity and per app
//     profile; the order is app profile, tax entity, then these settings.
//   - the UPI cap of 3 dunning retries per cycle is enforced by the
//     subscriptions service, which knows the mandate method.
var defs = []Def{
	{"routing", defaultRouting, orgApp, validRouting},
	{"checkout_mode", `"hosted"`, LevelApp, oneOf("hosted", "provider_action")},
	{"duplicate_payment_action", `"review"`, orgApp, oneOf("review", "auto_refund")},
	{"billing_details", `"state_required"`, orgApp, oneOf("off", "state_required", "optional", "required")},
	{"payment_ttl", `60`, orgApp, intRange(10, 7*24*60)}, // minutes
	{"pay_page_retry_buttons", `{"try_again":true,"pay_another_way":true}`, orgApp, validRetryButtons},
	{"invoice_delivery", `"app"`, orgApp, oneOf("app", "paymux", "both")},
	{"issue_invoices", `true`, LevelApp, isBool},
	{"sac_code", `"998315"`, LevelApp, matches(`^[0-9]{6}$`)},
	{"gst_rate_bp", `1800`, LevelApp, intRange(0, 2800)},
	{"prices_include_tax", `true`, LevelApp, isBool},
	{"invoice_series_prefix", `"INV"`, orgApp, matches(`^[A-Z0-9]{1,5}$`)},
	{"credit_note_series_prefix", `"CN"`, orgApp, matches(`^[A-Z0-9]{1,5}$`)},
	{"dispute_credit_note", `"manual"`, orgApp, oneOf("manual", "on_loss")},
	// credit_note_cutoff: the last day (MM-DD, IST) after the end of the
	// financial year of the supply on which a credit note may still be issued;
	// "off" never stops one. The statutory day is 30 November (CGST Act s.34(2)).
	{"credit_note_cutoff", `"11-30"`, LevelOrg, validCutoff},
	// verification_debit_fallback: what to do about the Rs 1 debit of a mandate
	// registration when it is not known whether the provider reverses it: refund
	// it explicitly after 72 hours unless the provider (or someone) already has,
	// or never.
	{"verification_debit_fallback", `"refund_after_72h"`, LevelOrg, oneOf("refund_after_72h", "never")},
	// einvoice_irp_keys: the portal's public keys (PEM certificates or public
	// keys, RSA of 2048 bits or more). While any is set, the signature of a
	// recorded SignedQRCode is verified against them; with none, only the claims
	// in it are checked. Paymux ships no key: the IRP publishes its own, and an
	// invented default would be worse than none.
	{"einvoice_irp_keys", `[]`, LevelOrg, validIRPKeys},
	// einvoice_igst_refund_claim: RefClm of an export e-invoice that paid IGST.
	// auto claims the refund (the usual case); Y and N force it. An export under
	// a LUT paid no IGST and never claims, whatever this says.
	{"einvoice_igst_refund_claim", `"auto"`, LevelOrg, oneOf("auto", "Y", "N")},
	// b2cl_threshold: the invoice value (INR paise) above which an inter-state
	// B2C invoice goes to the B2CL sheet of the GST report (Rs 1,00,000).
	{"b2cl_threshold", `10000000`, LevelOrg, intRange(1, 1_000_000_000_000)},
	{"dunning_schedule", `[24,72,120]`, orgApp, validDunning}, // hours after the failure, 0-6 retries
	{"dunning_final_action", `"cancel"`, orgApp, oneOf("cancel", "keep_past_due")},
	{"predebit_lead", `48`, orgApp, intRange(25, 7*24)}, // hours
	// predebit_email: also email the customer a pre-debit notice (merchant,
	// amount, date, mandate reference, reason), on top of the provider's own.
	{"predebit_email", `true`, orgApp, isBool},
	// theme: the look of the pages. At the org level it is the dashboard's
	// theme (and the default for customer pages); an app's own value is the
	// theme of its customer pages (pay, subscription, invoice).
	{"theme", `"default"`, orgApp, matches(`^[a-z0-9][a-z0-9-]{0,39}$`)},
	// customer_language: the language of customer pages and the pre-debit email.
	{"customer_language", `"en"`, orgApp, matches(`^[a-z]{2,3}$`)},
	{"mandate_headroom", `1.5`, orgApp, floatRange(1.0, 5.0)},
	{"mandate_afa_limit", `1500000`, LevelOrg, intRange(1, 10000000)}, // INR paise (Rs 15,000)
	{"mandate_validity", `5`, orgApp, intRange(1, 30)},                // years
	// upi_execution_windows are the peak windows to avoid: debits run outside them (IST).
	{"upi_execution_windows", `[{"start":"10:00","end":"13:00"},{"start":"17:00","end":"21:30"}]`, LevelOrg, validWindows},
	{"webhook_retry_schedule", `[0,60,300,1800,7200,21600,43200,86400,86400]`, LevelOrg, validRetrySchedule}, // seconds
	{"recon_time", `"03:00"`, LevelOrg, validClock},
	{"provider_event_retention", `400`, LevelOrg, zeroOrAtLeast(30)}, // days; 0 = forever
	{"document_retention", `0`, LevelOrg, zeroOrAtLeast(8)},          // years; 0 = forever
	// The error log in the database: error rows (and failed provider calls with
	// their bodies) are kept longer than warning rows; log_max_rows and
	// log_max_mb cap the table whatever the age, oldest first.
	{"log_retention_error_days", `90`, LevelOrg, intRange(1, 3650)},
	{"log_retention_warn_days", `7`, LevelOrg, intRange(1, 365)},
	{"log_max_rows", `500000`, LevelOrg, intRange(1000, 100_000_000)},
	{"log_max_mb", `1024`, LevelOrg, intRange(16, 1_000_000)},
	// audit_retention_days: PCI DSS 10.5.1 asks for at least a year of audit
	// history, so nothing below 365 days is accepted; 0 keeps it forever.
	{"audit_retention_days", `400`, LevelOrg, zeroOrAtLeast(365)},
	// How long a dashboard sign-in lasts. A session ends after session_idle_hours
	// without a request and after session_max_days whatever happens. With
	// "Remember me" ticked at sign-in both limits are session_remember_days instead;
	// 0 removes the checkbox.
	{"session_idle_hours", `12`, LevelOrg, intRange(1, 720)},
	{"session_max_days", `7`, LevelOrg, intRange(1, 90)},
	{"session_remember_days", `30`, LevelOrg, intRange(0, 90)},
}

var clockRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func decodeStrict(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("must be %T: %v", dst, err)
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// validCutoff accepts "off" or a real calendar day "MM-DD" (29 February counts).
func validCutoff(raw json.RawMessage) error {
	var s string
	if err := decodeStrict(raw, &s); err != nil {
		return err
	}
	if s == "off" {
		return nil
	}
	if !cutoffRE.MatchString(s) {
		return fmt.Errorf(`must be "off" or a day as "MM-DD", for example "11-30"`)
	}
	t, err := time.Parse("2006-01-02", "2024-"+s) // 2024 is a leap year
	if err != nil || t.Format("01-02") != s {
		return fmt.Errorf("%q is not a calendar day", s)
	}
	return nil
}

var cutoffRE = regexp.MustCompile(`^[0-9]{2}-[0-9]{2}$`)

func oneOf(allowed ...string) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var s string
		if err := decodeStrict(raw, &s); err != nil {
			return err
		}
		if !slices.Contains(allowed, s) {
			return fmt.Errorf("must be one of %v", allowed)
		}
		return nil
	}
}

func matches(pattern string) func(json.RawMessage) error {
	re := regexp.MustCompile(pattern)
	return func(raw json.RawMessage) error {
		var s string
		if err := decodeStrict(raw, &s); err != nil {
			return err
		}
		if !re.MatchString(s) {
			return fmt.Errorf("must match %s", pattern)
		}
		return nil
	}
}

func intRange(lo, hi int64) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var n int64
		if err := decodeStrict(raw, &n); err != nil {
			return err
		}
		if n < lo || n > hi {
			return fmt.Errorf("must be between %d and %d", lo, hi)
		}
		return nil
	}
}

func zeroOrAtLeast(lo int64) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var n int64
		if err := decodeStrict(raw, &n); err != nil {
			return err
		}
		if n != 0 && n < lo {
			return fmt.Errorf("must be 0 (forever) or at least %d", lo)
		}
		return nil
	}
}

func floatRange(lo, hi float64) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var f float64
		if err := decodeStrict(raw, &f); err != nil {
			return err
		}
		if f < lo || f > hi {
			return fmt.Errorf("must be between %v and %v", lo, hi)
		}
		return nil
	}
}

func isBool(raw json.RawMessage) error {
	var b bool
	return decodeStrict(raw, &b)
}

func validClock(raw json.RawMessage) error {
	var s string
	if err := decodeStrict(raw, &s); err != nil {
		return err
	}
	if !clockRE.MatchString(s) {
		return fmt.Errorf("must be HH:MM")
	}
	return nil
}

func validRetryButtons(raw json.RawMessage) error {
	var v struct {
		TryAgain      *bool `json:"try_again"`
		PayAnotherWay *bool `json:"pay_another_way"`
	}
	if err := decodeStrict(raw, &v); err != nil {
		return err
	}
	if v.TryAgain == nil || v.PayAnotherWay == nil {
		return fmt.Errorf("try_again and pay_another_way are both required")
	}
	return nil
}

func validDunning(raw json.RawMessage) error {
	var hours []int64
	if err := decodeStrict(raw, &hours); err != nil {
		return err
	}
	if len(hours) > 6 {
		return fmt.Errorf("at most 6 retries")
	}
	for _, h := range hours {
		if h < 1 || h > 30*24 {
			return fmt.Errorf("each retry must be 1 to 720 hours after the failure")
		}
	}
	return nil
}

func validIRPKeys(raw json.RawMessage) error {
	var pems []string
	if err := decodeStrict(raw, &pems); err != nil {
		return err
	}
	if len(pems) > 5 {
		return fmt.Errorf("at most 5 keys")
	}
	if len(pems) == 0 {
		return nil
	}
	_, err := irp.ParseKeys(pems)
	return err
}

func validRetrySchedule(raw json.RawMessage) error {
	var secs []int64
	if err := decodeStrict(raw, &secs); err != nil {
		return err
	}
	if len(secs) < 1 || len(secs) > 15 {
		return fmt.Errorf("must have 1 to 15 steps")
	}
	for _, s := range secs {
		if s < 0 {
			return fmt.Errorf("steps must not be negative")
		}
	}
	return nil
}

func validWindows(raw json.RawMessage) error {
	var ws []struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if err := decodeStrict(raw, &ws); err != nil {
		return err
	}
	for _, w := range ws {
		if !clockRE.MatchString(w.Start) || !clockRE.MatchString(w.End) || w.Start >= w.End {
			return fmt.Errorf("each window needs HH:MM start < end")
		}
	}
	return nil
}

func validAction(a routing.Action) error {
	switch a.Kind {
	case "reject":
		return nil
	case "route":
		if len(a.Targets) == 0 {
			return fmt.Errorf("a route action needs targets")
		}
		for _, t := range a.Targets {
			if t.Connector == "" && t.AccountID == "" {
				return fmt.Errorf("each target needs a connector or an account_id")
			}
		}
		return nil
	}
	return fmt.Errorf("action kind must be route or reject")
}

// validRouting checks the routing document: {"rules": [Rule...], "catch_all":
// Rule}. The org value is read as the org rules plus the catch-all; an app
// override supplies the app rules. catch_all is optional.
func validRouting(raw json.RawMessage) error {
	var v struct {
		Rules    []routing.Rule `json:"rules"`
		CatchAll *routing.Rule  `json:"catch_all"`
	}
	if err := decodeStrict(raw, &v); err != nil {
		return err
	}
	if v.Rules == nil {
		return fmt.Errorf("rules is required (use [] for none)")
	}
	rules := v.Rules
	if v.CatchAll != nil {
		rules = append(rules, *v.CatchAll)
	}
	for _, r := range rules {
		if err := validAction(r.Action); err != nil {
			return fmt.Errorf("rule %q: %v", r.Name, err)
		}
	}
	return nil
}
