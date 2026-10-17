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

// Options configures a Service.
type Options struct {
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
	baseURL  string
	now      func() time.Time
	isMoR    func(string) bool

	pagesOnce sync.Once
	pagesR    *web.Renderer
	pagesErr  error
	pagesOpt  *web.Renderer
}

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
	return &Service{db: o.DB, settings: o.Settings, jobs: o.Jobs, emitter: o.Emitter, mailer: o.Mailer,
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
	var flags []string
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

	var amts tax.Amounts
	if prof.PricesIncludeTax {
		amts, err = tax.FromTotal(received, prof.GSTRateBP, cls.InterState, cls.ZeroRated())
	} else {
		// TODO(spec): with tax-exclusive pricing the core must charge
		// taxable+tax; until it does the invoice total can exceed what was received.
		amts, err = tax.FromTaxable(received, prof.GSTRateBP, cls.InterState, cls.ZeroRated())
	}
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
			flags = append(flags, FlagFXPending) // TODO(spec): ops alert (alerts for tax exceptions are not wired)
		default:
			return nil, err
		}
	}
	// TODO(spec): raise an ops alert for pos_defaulted and export_igst.

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
	switch {
	case cls.ZeroRated():
	case cls.InterState:
		igstBP = prof.GSTRateBP
	default:
		cgstBP = prof.GSTRateBP / 2
		sgstBP = prof.GSTRateBP - cgstBP
	}
	line := Line{Description: firstNonEmpty(p.Description, "Payment "+firstNonEmpty(p.OrderRef, p.ID)), SAC: prof.SACCode, Qty: 1,
		Unit: "service", UnitAmount: amts.Taxable, Taxable: amts.Taxable, CGSTRateBP: cgstBP, SGSTRateBP: sgstBP,
		IGSTRateBP: igstBP, CGST: amts.CGST, SGST: amts.SGST, IGST: amts.IGST}
	linesJSON, _ := json.Marshal([]Line{line})

	token, tokenHash := newToken()
	inv := store.Invoice{
		ID: ids.New("inv"), OrgID: p.OrgID, Mode: p.Mode, AppID: p.AppID, TaxEntityID: ent.ID, PaymentID: p.ID,
		AttemptID: a.ID, SubscriptionID: p.SubscriptionID, Number: number, SeriesPrefix: prefix, FiscalYear: fy,
		IssuedAt: issued, SupplyType: string(cls.SupplyType), PlaceOfSupply: cls.PlaceOfSupply, InterState: cls.InterState,
		Currency: cur, Supplier: supJSON, Recipient: rcpJSON, Lines: linesJSON,
		Taxable: amts.Taxable, CGST: amts.CGST, SGST: amts.SGST, IGST: amts.IGST, Total: amts.Total, GSTRateBP: prof.GSTRateBP,
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

// IssueCreditNote issues the credit note of a succeeded refund inside tx. It
// returns nil, nil when none is due: the payment has no invoice, or the cutoff
// passed (the refund then gets the credit_note_cutoff_passed flag). It is
// idempotent per refund.
func (s *Service) IssueCreditNote(ctx context.Context, tx store.Tx, r store.Refund, p store.Payment, a store.Attempt) (*store.CreditNote, error) {
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
		if prior[i].RefundID == r.ID {
			return &prior[i], nil
		}
	}
	now := s.now().UTC()
	if now.After(tax.CreditNoteDeadline(inv.IssuedAt)) {
		// TODO(spec): the cutoff is meant to be an org setting; the statutory
		// default is hard-wired until that setting exists.
		return nil, store.AddRefundFlag(ctx, tx, r.ID, FlagCutoffPassed, now)
	}
	if r.Currency != inv.Currency {
		return nil, fmt.Errorf("invoice: refund %s is in %s but invoice %s in %s", r.ID, r.Currency, inv.Number, inv.Currency)
	}
	var done []tax.Amounts
	for _, c := range prior {
		done = append(done, amountsOf(c.Taxable, c.CGST, c.SGST, c.IGST, c.Total))
	}
	split, err := tax.CreditSplit(amountsOf(inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total), r.Amount, done)
	if err != nil {
		return nil, fmt.Errorf("invoice: credit for refund %s: %w", r.ID, err)
	}
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
		PaymentID: p.ID, RefundID: r.ID, Number: number, SeriesPrefix: prefix, FiscalYear: fy, IssuedAt: now,
		Reason: firstNonEmpty(r.Reason, "Refund"), Currency: inv.Currency, Lines: linesJSON,
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
