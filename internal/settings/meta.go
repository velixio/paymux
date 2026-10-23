package settings

// Kind says which control suits a setting.
type Kind string

const (
	KindEnum   Kind = "enum"   // one of Options
	KindInt    Kind = "int"    // whole number between Min and Max (Max 0: no upper bound)
	KindFloat  Kind = "float"  // decimal between Min and Max
	KindBool   Kind = "bool"   // true or false
	KindString Kind = "string" // text matching Pattern
	KindClock  Kind = "clock"  // HH:MM
	KindList   Kind = "list"   // a list of whole numbers
	KindJSON   Kind = "json"   // a document; edited as JSON
)

// Meta is what a UI needs to draw a control for one setting. The tests pin
// each Meta to the setting's validator, so the two cannot drift apart.
type Meta struct {
	Help string
	Kind Kind
	// Unit of the value (or of each list item): minutes, hours, days, years,
	// seconds, basis points, paise, x.
	Unit    string
	Options []string
	// Min and Max bound a number (or each list item). Max 0 means no upper
	// bound. ZeroOK allows 0 below Min ("0 = forever").
	Min, Max float64
	ZeroOK   bool
	// MinItems and MaxItems bound a list.
	MinItems, MaxItems int
	Pattern            string // KindString
}

var metas = map[string]Meta{
	"routing": {Kind: KindJSON, Help: "Which provider accounts take which payments: ordered rules, then a catch-all. Edited on the org Settings page."},
	"duplicate_payment_action": {Kind: KindEnum, Options: []string{"review", "auto_refund"},
		Help: "What to do when a customer pays an already-paid order a second time: hold it for review, or refund it automatically."},
	"billing_details": {Kind: KindEnum, Options: []string{"off", "state_required", "optional", "required"},
		Help: "Billing details asked on the pay page: none, only the State, optional, or required (address and GSTIN)."},
	"payment_ttl": {Kind: KindInt, Unit: "minutes", Min: 10, Max: 7 * 24 * 60,
		Help: "How long a payment stays payable before it expires."},
	"pay_page_retry_buttons": {Kind: KindJSON, Help: `Buttons on the failed-payment screen: {"try_again": true, "pay_another_way": true}.`},
	"invoice_delivery": {Kind: KindEnum, Options: []string{"app", "paymux", "both"},
		Help: "Who emails invoices: your app (Paymux only produces them), Paymux, or both."},
	"issue_invoices":     {Kind: KindBool, Help: "Issue a GST invoice for each successful payment of this app."},
	"sac_code":           {Kind: KindString, Pattern: `^[0-9]{6}$`, Help: "Services Accounting Code printed on invoices (6 digits)."},
	"gst_rate_bp":        {Kind: KindInt, Unit: "basis points", Min: 0, Max: 2800, Help: "GST rate in basis points: 1800 is 18%."},
	"prices_include_tax": {Kind: KindBool, Help: "Whether the prices your app charges already include GST."},
	"invoice_series_prefix": {Kind: KindString, Pattern: `^[A-Z0-9]{1,5}$`,
		Help: "Prefix of invoice numbers, 1 to 5 capital letters or digits."},
	"credit_note_series_prefix": {Kind: KindString, Pattern: `^[A-Z0-9]{1,5}$`,
		Help: "Prefix of credit note numbers, 1 to 5 capital letters or digits."},
	"dispute_credit_note": {Kind: KindEnum, Options: []string{"manual", "on_loss"},
		Help: "Credit notes for lost disputes: issued by an operator, or automatically when the dispute is lost."},
	"credit_note_cutoff": {Kind: KindString, Pattern: `^(off|(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01]))$`,
		Help: `Last day (month-day, for example 11-30) after the end of the financial year of the sale on which a credit note can still be issued; "off" never stops one. The legal day is 30 November.`},
	"verification_debit_fallback": {Kind: KindEnum, Options: []string{"refund_after_72h", "never"},
		Help: "The Rs 1 debit of a mandate registration is meant to be reversed by the provider. When that is not certain, refund_after_72h makes Paymux check the provider after 72 hours and refund the debit itself if it has not been reversed (never a second time); never leaves it alone."},
	"einvoice_irp_keys": {Kind: KindJSON,
		Help: `The e-invoice portal's public keys, as a list of PEM certificates or public keys: ["-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----"]. With one set, the signature of the SignedQRCode you record is verified and a token not signed by it is refused. With none, only what the token says is checked. Get the keys from the portal's published certificates; RSA, 2048 bits or more.`},
	"einvoice_igst_refund_claim": {Kind: KindEnum, Options: []string{"auto", "Y", "N"},
		Help: "Whether an export e-invoice that paid IGST claims a refund of it (RefClm). auto claims it, the usual case for an exporter paying IGST; Y or N force it. An export under a LUT paid no IGST and never claims."},
	"b2cl_threshold": {Kind: KindInt, Unit: "paise", Min: 1, Max: 1_000_000_000_000,
		Help: "Invoice value above which an inter-state sale to a consumer goes to the B2CL sheet of the GST report. The legal limit is 1,00,000 rupees."},
	"dunning_schedule": {Kind: KindList, Unit: "hours", Min: 1, Max: 30 * 24, MaxItems: 6,
		Help: "Retry a failed renewal this many hours after the failure; up to 6 retries, comma separated. Empty means no retries."},
	"dunning_final_action": {Kind: KindEnum, Options: []string{"cancel", "keep_past_due"},
		Help: "What to do with a subscription when every retry has failed."},
	"theme": {Kind: KindString, Pattern: `^[a-z0-9][a-z0-9-]{0,39}$`,
		Help: "Look of the pages. On the org it themes the dashboard (and is the default for customer pages); on an app it themes that app's pay, subscription and invoice pages. Pick from the Appearance section."},
	"customer_language": {Kind: KindString, Pattern: `^[a-z]{2,3}$`,
		Help: "Language of customer pages and the pre-debit email: a two-letter code such as en or hi. Unknown languages fall back to English."},
	"predebit_email": {Kind: KindBool, Help: "Also email the customer a pre-debit notice before each automatic charge."},
	"predebit_lead": {Kind: KindInt, Unit: "hours", Min: 25, Max: 7 * 24,
		Help: "How long before a mandate debit the customer is notified (at least 25 hours)."},
	"mandate_headroom": {Kind: KindFloat, Unit: "x", Min: 1, Max: 5,
		Help: "A mandate's maximum amount, as a multiple of the subscription price."},
	"mandate_afa_limit": {Kind: KindInt, Unit: "paise", Min: 1, Max: 10000000,
		Help: "Largest debit that needs no extra customer authentication. The RBI limit is 15,000 rupees."},
	"mandate_validity": {Kind: KindInt, Unit: "years", Min: 1, Max: 30, Help: "How long a new mandate stays valid."},
	"upi_execution_windows": {Kind: KindJSON,
		Help: `Peak windows (IST) to avoid when debiting UPI mandates: [{"start": "10:00", "end": "13:00"}].`},
	"webhook_retry_schedule": {Kind: KindList, Unit: "seconds", Min: 0, MinItems: 1, MaxItems: 15,
		Help: "Delay before each delivery attempt of a webhook to your app, in seconds, comma separated (1 to 15 steps; the first is usually 0)."},
	"recon_time": {Kind: KindClock, Help: "Time of day (in the hub's time zone) the daily reconciliation runs."},
	"provider_event_retention": {Kind: KindInt, Unit: "days", Min: 30, ZeroOK: true,
		Help: "How long raw provider webhooks are kept. 0 keeps them forever; otherwise at least 30 days."},
	"document_retention": {Kind: KindInt, Unit: "years", Min: 8, ZeroOK: true,
		Help: "How long invoices and credit notes are kept. 0 keeps them forever; otherwise at least 8 years."},
	"log_retention_error_days": {Kind: KindInt, Unit: "days", Min: 1, Max: 3650,
		Help: "How long error log rows (errors, failed provider calls with their bodies, 5xx responses, dead jobs) are kept on the Logs page."},
	"log_retention_warn_days": {Kind: KindInt, Unit: "days", Min: 1, Max: 365,
		Help: "How long warning log rows (warnings, rejected API and webhook requests, retries) are kept on the Logs page."},
	"log_max_rows": {Kind: KindInt, Unit: "rows", Min: 1000, Max: 100000000,
		Help: "The most log rows kept, whatever their age; the oldest go first. Keeps a SQLite logs.db small."},
	"log_max_mb": {Kind: KindInt, Unit: "MB", Min: 16, Max: 1000000,
		Help: "The most space the log rows may take (the text of their messages, bodies and details), whatever their age; the oldest go first."},
	"session_idle_hours": {Kind: KindInt, Unit: "hours", Min: 1, Max: 720,
		Help: "A dashboard sign-in ends after this many hours without a request (unless Remember me was ticked)."},
	"session_max_days": {Kind: KindInt, Unit: "days", Min: 1, Max: 90,
		Help: "A dashboard sign-in ends this many days after it began, however active (unless Remember me was ticked)."},
	"session_remember_days": {Kind: KindInt, Unit: "days", Min: 1, Max: 90, ZeroOK: true,
		Help: "With Remember me ticked, a sign-in lasts this many days from the sign-in and ends after this many days without a request. 0 removes the checkbox."},
	"audit_retention_days": {Kind: KindInt, Unit: "days", Min: 365, ZeroOK: true,
		Help: "How long audit log entries are kept. 0 keeps them forever; otherwise at least 365 days (PCI DSS 10.5.1)."},
}

// MetaFor returns the UI description of a setting.
func MetaFor(key string) (Meta, bool) {
	m, ok := metas[key]
	return m, ok
}
