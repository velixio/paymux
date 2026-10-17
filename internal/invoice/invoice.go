// Package invoice issues GST tax invoices and credit notes (spec section 10)
// inside the transaction that makes a payment or refund succeed, renders them
// to PDF in a job, delivers them, serves the hosted views and produces the
// monthly report CSVs.
package invoice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/invoice/render"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

// Flags raised on documents and refunds besides the classification flags
// (tax.FlagPOSDefaulted, tax.FlagExportIGST).
const (
	FlagFXPending         = "fx_pending"
	FlagCutoffPassed      = "credit_note_cutoff_passed"
	FlagRecipientInvalid  = "recipient_invalid"
	FlagReissued          = "reissued"
	testSeriesPrefix      = "TEST"
	kindInvoice           = "invoice"
	kindCreditNote        = "credit_note"
	JobRender             = "invoice.render"
	EventInvoiceIssued    = "invoice.issued"
	EventCreditNoteIssued = "credit_note.issued"
)

// Enqueuer enqueues a job inside the caller's transaction (*jobs.Queue).
type Enqueuer interface {
	Enqueue(ctx context.Context, tx store.Tx, j jobs.Job) error
}

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// Options configures a Service.
type Options struct {
	// Alerter receives the alerts for tax exceptions (pos_defaulted,
	// export_igst, fx_pending, credit_note_cutoff_passed). Nil raises none.
	Alerter  Alerter
	DB       *store.DB
	Settings *settings.Service
	Jobs     Enqueuer
	Emitter  events.Emitter
	// Mailer delivers documents when invoice_delivery is paymux or both. Nil
	// means notify.NoopMailer.
	Mailer notify.Mailer
	// BaseURL is the origin of hosted document URLs (PAYMUX_BASE_URL).
	BaseURL string
	Now     func() time.Time
	// IsMerchantOfRecord says whether a connector sells as merchant of record,
	// in which case Paymux issues no invoice. Default: the connector registry.
	IsMerchantOfRecord func(connectorName string) bool
	// Pages renders hosted HTML views; nil uses the embedded templates.
	Pages *web.Renderer
}

// Service issues and serves documents.
type Service struct {
	db       *store.DB
	settings *settings.Service
	jobs     Enqueuer
	emitter  events.Emitter
	mailer   notify.Mailer
	alerter  Alerter
	baseURL  string
	now      func() time.Time
	isMoR    func(string) bool

	pagesOnce sync.Once
	pagesR    *web.Renderer
	pagesErr  error
	pagesOpt  *web.Renderer
}

// SetAlerter sets the ops alert sender when it is built after the service (call before serving).
func (s *Service) SetAlerter(a Alerter) { s.alerter = a }

// SetPages sets the HTML renderer of the hosted views (call before serving).
func (s *Service) SetPages(r *web.Renderer) { s.pagesOpt = r }

// pages returns the HTML renderer (Options.Pages, or the embedded templates).
func (s *Service) pages() (*web.Renderer, error) {
	if s.pagesOpt != nil {
		return s.pagesOpt, nil
	}
	s.pagesOnce.Do(func() { s.pagesR, s.pagesErr = web.NewRenderer(web.Embedded()) })
	return s.pagesR, s.pagesErr
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Mailer == nil {
		o.Mailer = notify.NoopMailer{}
	}
	if o.Emitter == nil {
		o.Emitter = events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })
	}
	if o.IsMerchantOfRecord == nil {
		o.IsMerchantOfRecord = func(name string) bool {
			_, info, ok := connector.Lookup(name)
			return ok && info.MerchantOfRecord
		}
	}
	return &Service{db: o.DB, settings: o.Settings, jobs: o.Jobs, emitter: o.Emitter, mailer: o.Mailer, alerter: o.Alerter,
		baseURL: strings.TrimRight(o.BaseURL, "/"), now: o.Now, isMoR: o.IsMerchantOfRecord, pagesOpt: o.Pages}
}

// Supplier is the supplier snapshot stored on a document.
type Supplier struct {
	LegalName     string `json:"legal_name"`
	TradeName     string `json:"trade_name,omitempty"`
	GSTIN         string `json:"gstin"`
	PAN           string `json:"pan,omitempty"`
	Address       string `json:"address,omitempty"`
	StateCode     string `json:"state_code"`
	StateName     string `json:"state_name"`
	SignatoryName string `json:"signatory_name,omitempty"`
	FooterNote    string `json:"footer_note,omitempty"`
	LogoBlobID    string `json:"logo_blob_id,omitempty"`
	SupportEmail  string `json:"support_email,omitempty"`
}

// Recipient is the recipient snapshot stored on a document.
type Recipient struct {
	Name        string `json:"name"`
	GSTIN       string `json:"gstin,omitempty"`
	Address     string `json:"address,omitempty"`
	StateCode   string `json:"state_code,omitempty"`
	StateName   string `json:"state_name,omitempty"`
	Country     string `json:"country"`
	CountryName string `json:"country_name,omitempty"`
	// The address as parts, for the e-invoice (Address is these joined).
	Line1      string `json:"line1,omitempty"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
}

// Line is one document line (amounts in the document currency's minor units).
type Line struct {
	Description string `json:"description"`
	SAC         string `json:"sac"`
	Qty         int64  `json:"qty"`
	Unit        string `json:"unit"`
	UnitAmount  int64  `json:"unit_amount"`
	Taxable     int64  `json:"taxable"`
	CGSTRateBP  int    `json:"cgst_rate_bp"`
	SGSTRateBP  int    `json:"sgst_rate_bp"`
	IGSTRateBP  int    `json:"igst_rate_bp"`
	CGST        int64  `json:"cgst"`
	SGST        int64  `json:"sgst"`
	IGST        int64  `json:"igst"`
}

func newToken() (token, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("invoice: entropy source failed: %v", err))
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func amountsOf(taxable, cgst, sgst, igst, total int64) tax.Amounts {
	return tax.Amounts{Taxable: taxable, CGST: cgst, SGST: sgst, IGST: igst, Total: total}
}

func (s *Service) invoiceURLs(id, token string) (hosted, pdf string) {
	return s.baseURL + "/i/" + id + "?t=" + token, s.baseURL + "/i/" + id + ".pdf?t=" + token
}

// OnPaymentSucceeded is the core.PaymentHook: it loads the customer and issues
// the invoice, if any, in the caller's transaction.
func (s *Service) OnPaymentSucceeded(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt) error {
	var c store.Customer
	if p.CustomerID != "" {
		var err error
		if c, err = store.GetCustomer(ctx, tx, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	_, err := s.IssueForPayment(ctx, tx, p, a, c)
	return err
}

// IssueForPayment issues the tax invoice of a succeeded payment inside tx. It
// returns nil, nil when no invoice is due: the app issues none, the connector
// is merchant of record, the attempt is a duplicate, or the payment is a
// mandate setup. It is idempotent per payment.
func (s *Service) IssueForPayment(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt, c store.Customer) (*store.Invoice, error) {
	if a.Duplicate || p.Kind == "mandate_setup" {
		return nil, nil
	}
	prof, err := store.GetAppTaxProfile(ctx, tx, p.AppID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !prof.IssueInvoices) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pa, err := store.GetProviderAccount(ctx, tx, a.ProviderAccountID)
	if err != nil {
		return nil, fmt.Errorf("invoice: provider account of attempt %s: %w", a.ID, err)
	}
	if s.isMoR(pa.Connector) {
		return nil, nil
	}
	if existing, err := store.InvoicesForPayment(ctx, tx, p.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return &existing[0], nil
	}
	return s.issue(ctx, tx, p, a, c, prof, nil)
}

// prorationLines returns the invoice lines: just line, or, when the
// subscription cycle behind the payment includes prorated adjustments, the
// period's own amount followed by one line per adjustment (a credit is a
// negative line). The tax of the whole invoice (amts) is split over the lines
// by their amounts; rounding differences land on the last line, so the lines
// always add up to the invoice.
func (s *Service) prorationLines(ctx context.Context, tx store.Tx, p store.Payment, line Line, received int64, amts tax.Amounts,
	inclusive bool, rateBP int, interState, zeroRated bool) ([]Line, error) {
	if p.SubscriptionID == "" {
		return []Line{line}, nil
	}
	cy, err := store.GetCycleByPayment(ctx, tx, p.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && len(cy.Adjustments) == 0) {
		return []Line{line}, nil
	}
	if err != nil {
		return nil, err
	}
	type item struct {
		desc   string
		amount int64
	}
	var items []item
	adj := int64(0)
	for _, a := range cy.Adjustments {
		adj += a.Amount
	}
	if base := received - adj; base > 0 {
		items = append(items, item{line.Description, base})
	}
	for _, a := range cy.Adjustments {
		items = append(items, item{a.Description, a.Amount})
	}
	if len(items) == 0 {
		return []Line{line}, nil
	}
	var out []Line
	var sum tax.Amounts
	for i, it := range items {
		var a tax.Amounts
		if i == len(items)-1 {
			a = tax.Amounts{Taxable: amts.Taxable - sum.Taxable, CGST: amts.CGST - sum.CGST, SGST: amts.SGST - sum.SGST,
				IGST: amts.IGST - sum.IGST, Total: amts.Total - sum.Total}
		} else {
			abs := it.amount
			if abs < 0 {
				abs = -abs
			}
			var err error
			if inclusive {
				a, err = tax.FromTotal(abs, rateBP, interState, zeroRated)
			} else {
				a, err = tax.FromTaxable(abs, rateBP, interState, zeroRated)
			}
			if err != nil {
				return nil, fmt.Errorf("invoice: amounts for line %q: %w", it.desc, err)
			}
			if it.amount < 0 {
				a = tax.Amounts{Taxable: -a.Taxable, CGST: -a.CGST, SGST: -a.SGST, IGST: -a.IGST, Total: -a.Total}
			}
		}
		sum = sum.Add(a)
		l := line
		l.Description, l.UnitAmount, l.Taxable, l.CGST, l.SGST, l.IGST = it.desc, a.Taxable, a.Taxable, a.CGST, a.SGST, a.IGST
		out = append(out, l)
	}
	return out, nil
}

// issue writes a new invoice for the payment. IssueForPayment calls it once
// per payment; ReissueInvoice calls it again with corrected billing details.
func (s *Service) issue(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt, c store.Customer,
	prof store.AppTaxProfile, extraFlags []string) (*store.Invoice, error) {
	ent, err := store.GetTaxEntity(ctx, tx, prof.TaxEntityID)
	if err != nil {
		return nil, fmt.Errorf("invoice: tax entity %s of app %s: %w", prof.TaxEntityID, p.AppID, err)
	}

	issued := s.now().UTC()
	cur := firstNonEmpty(a.CurrencyReceived, p.Currency)
	received := p.AmountReceived
	if received <= 0 {
		return nil, fmt.Errorf("invoice: payment %s has no received amount", p.ID)
	}

	sup := tax.Supplier{GSTIN: ent.GSTIN, StateCode: ent.StateCode}
	if ent.LUTNumber != "" {
		sup.LUT = &tax.LUT{Number: ent.LUTNumber, ValidFrom: ent.LUTValidFrom, ValidTo: ent.LUTValidTo}
	}
	rcp := tax.Recipient{GSTIN: c.Billing.GSTIN, StateCode: c.Billing.StateCode, Country: c.Billing.Country}
	flags := slices.Clone(extraFlags)
	cls, err := tax.Classify(sup, rcp, issued, cur != "INR")
	if err != nil {
		// Bad billing data must not stop a paid order from being invoiced:
		// treat the recipient as unregistered with an unknown State, which
		// the classifier defaults to the supplier State, and flag it.
		rcp = tax.Recipient{Country: c.Billing.Country}
		if cls, err = tax.Classify(sup, rcp, issued, cur != "INR"); err != nil {
			return nil, err
		}
		flags = append(flags, FlagRecipientInvalid)
	}
	flags = append(flags, cls.Flags...)

	// What was received always includes the tax: with tax-inclusive prices the
	// price carried it, and with tax-exclusive prices the core charged
	// taxable + GST (core.chargeAmount). Either way the invoice total is the
	// amount received and the heads are derived from it.
	amts, err := tax.FromTotal(received, prof.GSTRateBP, cls.InterState, cls.ZeroRated())
	if err != nil {
		return nil, fmt.Errorf("invoice: amounts for payment %s: %w", p.ID, err)
	}

	fx, fxSource := "", ""
	if cur != "INR" {
		r, err := store.GetFXRate(ctx, tx, p.OrgID, cur, issued.In(tax.IST).Format("2006-01-02"))
		switch {
		case err == nil:
			fx, fxSource = r.Rate, r.Source
		case errors.Is(err, store.ErrNotFound):
			flags = append(flags, FlagFXPending) // alerted after the invoice is stored (alertInvoiceFlags)
		default:
			return nil, err
		}
	}

	fy := tax.FiscalYear(issued)
	prefix := firstNonEmpty(prof.InvoicePrefix, ent.InvoicePrefix, "INV")
	if p.Mode == "test" {
		prefix = testSeriesPrefix // test documents never consume the live series
	}
	seq, err := store.NextDocumentNumber(ctx, tx, ent.ID, kindInvoice, prefix, fy)
	if err != nil {
		return nil, err
	}
	number, err := tax.DocumentNumber(prefix, fy, seq)
	if err != nil {
		return nil, fmt.Errorf("invoice: numbering (prefix %q): %w", prefix, err)
	}

	stateName, _ := tax.StateName(ent.StateCode)
	supJSON, _ := json.Marshal(Supplier{LegalName: ent.LegalName, TradeName: ent.TradeName, GSTIN: ent.GSTIN, PAN: ent.PAN,
		Address: ent.Address, StateCode: ent.StateCode, StateName: stateName, SignatoryName: ent.SignatoryName,
		FooterNote: ent.FooterNote, LogoBlobID: ent.LogoBlobID, SupportEmail: ent.SupportEmail})
	rcpJSON, _ := json.Marshal(recipientSnapshot(c, rcp, cls))
	var cgstBP, sgstBP, igstBP int
	appliedBP := prof.GSTRateBP
	switch {
	case cls.ZeroRated():
		appliedBP = 0
	case cls.InterState:
		igstBP = prof.GSTRateBP
	default:
		cgstBP = prof.GSTRateBP / 2
		sgstBP = prof.GSTRateBP - cgstBP
	}
	line := Line{Description: firstNonEmpty(p.Description, "Payment "+firstNonEmpty(p.OrderRef, p.ID)), SAC: prof.SACCode, Qty: 1,
		Unit: "service", UnitAmount: amts.Taxable, Taxable: amts.Taxable, CGSTRateBP: cgstBP, SGSTRateBP: sgstBP,
		IGSTRateBP: igstBP, CGST: amts.CGST, SGST: amts.SGST, IGST: amts.IGST}
	// Proration adjustments are in the price's own terms: tax-inclusive, or the
	// taxable value when prices exclude tax.
	lineBase := received
	if !prof.PricesIncludeTax {
		lineBase = amts.Taxable
	}
	lines, err := s.prorationLines(ctx, tx, p, line, lineBase, amts, prof.PricesIncludeTax, prof.GSTRateBP, cls.InterState, cls.ZeroRated())
	if err != nil {
		return nil, err
	}
	linesJSON, _ := json.Marshal(lines)

	token, tokenHash := newToken()
	inv := store.Invoice{
		ID: ids.New("inv"), OrgID: p.OrgID, Mode: p.Mode, AppID: p.AppID, TaxEntityID: ent.ID, PaymentID: p.ID,
		AttemptID: a.ID, SubscriptionID: p.SubscriptionID, Number: number, Seq: seq, SeriesPrefix: prefix, FiscalYear: fy,
		IssuedAt: issued, SupplyType: string(cls.SupplyType), PlaceOfSupply: cls.PlaceOfSupply, InterState: cls.InterState,
		Currency: cur, Supplier: supJSON, Recipient: rcpJSON, Lines: linesJSON,
		Taxable: amts.Taxable, CGST: amts.CGST, SGST: amts.SGST, IGST: amts.IGST, Total: amts.Total, GSTRateBP: appliedBP,
		FXRate: fx, FXSource: fxSource, Flags: flags, PaymentMethod: a.Method, PaymentRef: a.ProviderRef,
		TemplateVersion: render.Version1, AccessTokenHash: tokenHash,
	}
	if cls.SupplyType == tax.ExportLUT {
		inv.LUTNumber = ent.LUTNumber
	}
	if err := store.InsertInvoice(ctx, tx, inv); err != nil {
		return nil, err
	}
	if err := s.enqueueRender(ctx, tx, kindInvoice, inv.ID); err != nil {
		return nil, err
	}
	if err := s.alertInvoiceFlags(ctx, tx, inv); err != nil {
		return nil, err
	}
	hosted, pdf := s.invoiceURLs(inv.ID, token)
	err = s.emitter.Emit(ctx, tx, events.Event{OrgID: inv.OrgID, AppID: inv.AppID, Mode: inv.Mode, Type: EventInvoiceIssued,
		ObjectType: "invoice", ObjectID: inv.ID, ObjectVersion: 1,
		Data: struct {
			InvoiceView
			HostedURL string `json:"hosted_url"`
			PDFURL    string `json:"pdf_url"`
		}{ViewInvoice(inv), hosted, pdf}})
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (s *Service) enqueueRender(ctx context.Context, tx store.Tx, kind, id string) error {
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRender, Payload: map[string]string{"kind": kind, "id": id},
		DedupeKey: JobRender + ":" + kind + ":" + id})
}

func recipientSnapshot(c store.Customer, rcp tax.Recipient, cls tax.Classification) Recipient {
	r := Recipient{
		Name:      firstNonEmpty(c.Billing.LegalName, c.Name, "Customer"),
		GSTIN:     rcp.GSTIN,
		StateCode: rcp.StateCode,
		Country:   strings.ToUpper(firstNonEmpty(rcp.Country, "IN")),
		Line1:     strings.TrimSpace(c.Billing.Line1), Line2: strings.TrimSpace(c.Billing.Line2),
		City: strings.TrimSpace(c.Billing.City), PostalCode: strings.TrimSpace(c.Billing.PostalCode),
	}
	var addr []string
	for _, p := range []string{c.Billing.Line1, c.Billing.Line2, c.Billing.City, c.Billing.PostalCode} {
		if strings.TrimSpace(p) != "" {
			addr = append(addr, strings.TrimSpace(p))
		}
	}
	r.Address = strings.Join(addr, ", ")
	if r.Country == "IN" {
		// The State printed is the place of supply State (declared or GSTIN).
		code := cls.PlaceOfSupply
		if rcp.GSTIN == "" && rcp.StateCode == "" {
			code = ""
		}
		r.StateCode = code
		r.StateName, _ = tax.StateName(code)
	} else {
		r.CountryName = r.Country
	}
	return r
}

// ErrCreditNoteCutoff means the statutory deadline for a credit note against
// the invoice has passed (returned for dispute credit notes, which an operator
// asks for explicitly; a refund gets a flag instead).
var ErrCreditNoteCutoff = errors.New("invoice: the credit note deadline for this invoice has passed")

// creditSource is what a credit note answers: a refund or a dispute.
type creditSource struct {
	refund   *store.Refund
	dispute  *store.Dispute
	amount   int64
	currency string
	reason   string
}

func (c creditSource) id() string {
	if c.refund != nil {
		return c.refund.ID
	}
	return c.dispute.ID
}

func (c creditSource) matches(n store.CreditNote) bool {
	if c.refund != nil {
		return n.RefundID == c.refund.ID
	}
	return n.DisputeID == c.dispute.ID
}

// IssueCreditNote issues the credit note of a succeeded refund inside tx. It
// returns nil, nil when none is due: the payment has no invoice, or the cutoff
// passed (the refund then gets the credit_note_cutoff_passed flag). It is
// idempotent per refund.
func (s *Service) IssueCreditNote(ctx context.Context, tx store.Tx, r store.Refund, p store.Payment, a store.Attempt) (*store.CreditNote, error) {
	return s.issueCredit(ctx, tx, creditSource{refund: &r, amount: r.Amount, currency: r.Currency, reason: firstNonEmpty(r.Reason, "Refund")}, p, a)
}

// IssueDisputeCreditNote issues a credit note for a dispute (a chargeback)
// for the disputed amount. It returns nil, nil when the payment has no
// invoice, ErrCreditNoteCutoff when the deadline passed, and is idempotent per
// dispute.
func (s *Service) IssueDisputeCreditNote(ctx context.Context, tx store.Tx, d store.Dispute, p store.Payment, a store.Attempt) (*store.CreditNote, error) {
	reason := "Chargeback: " + firstNonEmpty(d.Reason, d.Stage, "disputed payment")
	return s.issueCredit(ctx, tx, creditSource{dispute: &d, amount: d.Amount, currency: d.Currency, reason: reason}, p, a)
}

func (s *Service) issueCredit(ctx context.Context, tx store.Tx, src creditSource, p store.Payment, a store.Attempt) (*store.CreditNote, error) {
	invs, err := store.InvoicesForPayment(ctx, tx, p.ID)
	if err != nil || len(invs) == 0 {
		return nil, err
	}
	inv := invs[0]
	for _, i := range invs {
		if i.AttemptID == a.ID {
			inv = i
		}
	}
	prior, err := store.CreditNotesForInvoice(ctx, tx, inv.ID)
	if err != nil {
		return nil, err
	}
	for i := range prior {
		if src.matches(prior[i]) {
			return &prior[i], nil
		}
	}
	now := s.now().UTC()
	deadline, hasCutoff, err := s.creditNoteDeadline(ctx, tx, inv.OrgID, inv.IssuedAt)
	if err != nil {
		return nil, err
	}
	if hasCutoff && now.After(deadline) {
		if src.refund == nil {
			return nil, ErrCreditNoteCutoff
		}
		if err := store.AddRefundFlag(ctx, tx, src.refund.ID, FlagCutoffPassed, now); err != nil {
			return nil, err
		}
		return nil, s.alertCutoffPassed(ctx, tx, inv, src.refund.ID)
	}
	if src.currency != inv.Currency {
		return nil, fmt.Errorf("invoice: %s is in %s but invoice %s in %s", src.id(), src.currency, inv.Number, inv.Currency)
	}
	var done []tax.Amounts
	for _, c := range prior {
		done = append(done, amountsOf(c.Taxable, c.CGST, c.SGST, c.IGST, c.Total))
	}
	split, err := tax.CreditSplit(amountsOf(inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total), src.amount, done)
	if err != nil {
		return nil, fmt.Errorf("invoice: credit for %s: %w", src.id(), err)
	}
	refs := creditRefs{PaymentID: p.ID}
	if src.refund != nil {
		refs.RefundID = src.refund.ID
	} else {
		refs.DisputeID = src.dispute.ID
	}
	return s.insertCreditNote(ctx, tx, inv, refs, split, src.reason, now)
}

// creditRefs says what a credit note answers: always a payment, and a refund
// or a dispute unless it is a manual one (the credit note of a reissue).
type creditRefs struct{ PaymentID, RefundID, DisputeID string }

// insertCreditNote numbers, stores, renders and announces a credit note for
// the amounts in split against inv. It serves refund, dispute and reissue
// credit notes alike.
func (s *Service) insertCreditNote(ctx context.Context, tx store.Tx, inv store.Invoice, refs creditRefs,
	split tax.Amounts, reason string, now time.Time) (*store.CreditNote, error) {
	ent, err := store.GetTaxEntity(ctx, tx, inv.TaxEntityID)
	if err != nil {
		return nil, err
	}
	prof, err := store.GetAppTaxProfile(ctx, tx, inv.AppID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	fy := tax.FiscalYear(now)
	prefix := firstNonEmpty(prof.CreditNotePrefix, ent.CreditNotePrefix, "CN")
	if inv.Mode == "test" {
		prefix = testSeriesPrefix
	}
	seq, err := store.NextDocumentNumber(ctx, tx, ent.ID, kindCreditNote, prefix, fy)
	if err != nil {
		return nil, err
	}
	number, err := tax.DocumentNumber(prefix, fy, seq)
	if err != nil {
		return nil, fmt.Errorf("invoice: numbering (prefix %q): %w", prefix, err)
	}
	var origLines []Line
	_ = json.Unmarshal(inv.Lines, &origLines)
	line := Line{Description: "Credit for invoice " + inv.Number, SAC: prof.SACCode, Qty: 1, Unit: "service",
		UnitAmount: split.Taxable, Taxable: split.Taxable, CGST: split.CGST, SGST: split.SGST, IGST: split.IGST}
	if len(origLines) > 0 {
		o := origLines[0]
		line.Description, line.SAC = "Credit: "+o.Description, o.SAC
		line.CGSTRateBP, line.SGSTRateBP, line.IGSTRateBP = o.CGSTRateBP, o.SGSTRateBP, o.IGSTRateBP
	}
	linesJSON, _ := json.Marshal([]Line{line})
	token, tokenHash := newToken()
	cn := store.CreditNote{
		ID: ids.New("crn"), OrgID: inv.OrgID, Mode: inv.Mode, AppID: inv.AppID, TaxEntityID: ent.ID, InvoiceID: inv.ID,
		PaymentID: refs.PaymentID, RefundID: refs.RefundID, DisputeID: refs.DisputeID, Number: number, Seq: seq, SeriesPrefix: prefix, FiscalYear: fy, IssuedAt: now,
		Reason: reason, Currency: inv.Currency, Lines: linesJSON,
		Taxable: split.Taxable, CGST: split.CGST, SGST: split.SGST, IGST: split.IGST, Total: split.Total,
		TemplateVersion: render.Version1, AccessTokenHash: tokenHash,
	}
	if err := store.InsertCreditNote(ctx, tx, cn); err != nil {
		return nil, err
	}
	if err := s.enqueueRender(ctx, tx, kindCreditNote, cn.ID); err != nil {
		return nil, err
	}
	hosted, pdf := s.creditNoteURLs(cn.ID, token)
	err = s.emitter.Emit(ctx, tx, events.Event{OrgID: cn.OrgID, AppID: cn.AppID, Mode: cn.Mode, Type: EventCreditNoteIssued,
		ObjectType: "credit_note", ObjectID: cn.ID, ObjectVersion: 1,
		Data: struct {
			CreditNoteView
			HostedURL string `json:"hosted_url"`
			PDFURL    string `json:"pdf_url"`
		}{ViewCreditNote(cn), hosted, pdf}})
	if err != nil {
		return nil, err
	}
	return &cn, nil
}

func (s *Service) creditNoteURLs(id, token string) (hosted, pdf string) {
	return s.baseURL + "/i/" + id + "?t=" + token, s.baseURL + "/i/" + id + ".pdf?t=" + token
}

// OnRefundSucceeded is the core.RefundHook.
func (s *Service) OnRefundSucceeded(ctx context.Context, tx store.Tx, r store.Refund, p store.Payment, a store.Attempt) error {
	_, err := s.IssueCreditNote(ctx, tx, r, p, a)
	return err
}
