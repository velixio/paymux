package dashboard

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
)

// lutWarnDays is how long before a LUT's last day the warning appears. LUTs
// run April to 31 March, so it shows from the start of March.
const lutWarnDays = 30

type reportsView struct {
	Entity, Month string
	Files         []reportFileRow
	ZipHref       string
	EInvoice      *eInvoiceReport
}

type reportFileRow struct{ Name, Href string }

type taxEntRow struct {
	ID, Name, LegalName, GSTIN, State, LUT, LUTTo, Prefixes string
}

type taxEntForm struct {
	ID, LegalName, TradeName, GSTIN, PAN, Address, State                  string
	LUTNumber, LUTFrom, LUTTo, InvoicePrefix, CreditNotePrefix, Signatory string
	Footer, SupportEmail, TurnoverBand                                    string
	HasLogo, Editing                                                      bool
	// The digital signature certificate that signs the entity's PDFs, if any.
	DSCSubject, DSCNotAfter string
	DSCSoon                 bool // expires within 30 days
}

type fxRow struct{ Currency, Date, Rate, Source string }

type fxPendingRow struct {
	Currency, Date string
	Count          int
}

type taxProfileView struct {
	Exists                          bool
	TaxEntityID, SAC, RatePct       string
	Issue, Inclusive                bool
	InvoicePrefix, CreditNotePrefix string
	Entities                        []entityOpt
}

func (d *Dashboard) routesTax() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/reports", d.authed(authz.PaymentRead, d.reportsPage))
	m.HandleFunc("GET "+prefix+"/reports/file", d.authed(authz.PaymentRead, d.reportFile))
	m.HandleFunc("GET "+prefix+"/reports/zip", d.authed(authz.PaymentRead, d.reportZip))
	m.HandleFunc("GET "+prefix+"/tax-entities", d.authed(authz.SettingsRead, d.taxEntitiesPage))
	m.HandleFunc("POST "+prefix+"/tax-entities", d.authed(authz.SettingsWrite, d.recent(d.taxEntityCreate)))
	m.HandleFunc("GET "+prefix+"/tax-entities/{id}", d.authed(authz.SettingsRead, d.taxEntityPage))
	m.HandleFunc("GET "+prefix+"/tax-entities/{id}/logo", d.authed(authz.SettingsRead, d.taxEntityLogo))
	m.HandleFunc("POST "+prefix+"/tax-entities/{id}", d.authed(authz.SettingsWrite, d.recent(d.taxEntityUpdate)))
	m.HandleFunc("POST "+prefix+"/tax-entities/{id}/dsc", d.authed(authz.SettingsWrite, d.recent(d.taxEntityDSCSet)))
	m.HandleFunc("POST "+prefix+"/tax-entities/{id}/dsc/remove", d.authed(authz.SettingsWrite, d.recent(d.taxEntityDSCRemove)))
	m.HandleFunc("POST "+prefix+"/apps/{id}/tax", d.authed(authz.SettingsWrite, d.recent(d.appTaxSave)))
	m.HandleFunc("GET "+prefix+"/fx", d.authed(authz.SettingsRead, d.fxPage))
	m.HandleFunc("POST "+prefix+"/fx", d.authed(authz.SettingsWrite, d.recent(d.fxSave)))
}

func (d *Dashboard) today() string { return d.o.Now().In(tax.IST).Format("2006-01-02") }

// ---- LUT warnings ----

// lutWarnings lists a message for every LUT that has expired or expires within
// lutWarnDays. A LUT is valid through the end of its last day (IST).
func (d *Dashboard) lutWarnings(ents []store.TaxEntity) []string {
	now := d.o.Now()
	var out []string
	for _, e := range ents {
		if e.LUTNumber == "" || e.LUTValidTo.IsZero() {
			continue
		}
		last := e.LUTValidTo.In(tax.IST)
		end := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, tax.IST).AddDate(0, 0, 1)
		day := last.Format("2006-01-02")
		name := firstNonBlank(e.TradeName, e.LegalName)
		switch left := end.Sub(now); {
		case left <= 0:
			out = append(out, fmt.Sprintf("The LUT %s of %s expired on %s. Exports are taxed at IGST until a new LUT is entered.", e.LUTNumber, name, day))
		case left <= lutWarnDays*24*time.Hour:
			out = append(out, fmt.Sprintf("The LUT %s of %s expires on %s (in %d days). File the new LUT and enter it before then.",
				e.LUTNumber, name, day, int(math.Ceil(left.Hours()/24))))
		}
	}
	return out
}

func (d *Dashboard) listEntities(ctx context.Context) ([]store.TaxEntity, error) {
	org, err := d.orgID(ctx)
	if err != nil {
		return nil, err
	}
	var ents []store.TaxEntity
	err = d.o.DB.Read(ctx, func(q store.Querier) (err error) { ents, err = store.ListTaxEntities(ctx, q, org); return })
	return ents, err
}

// ---- reports ----

// reportRequest validates entity and month of a report request.
func (d *Dashboard) reportRequest(r *http.Request) (store.TaxEntity, string, error) {
	qv := r.URL.Query()
	month := qv.Get("month")
	if _, _, err := monthBounds(month); err != nil {
		return store.TaxEntity{}, "", err
	}
	ents, err := d.listEntities(r.Context())
	if err != nil {
		return store.TaxEntity{}, "", err
	}
	for _, e := range ents {
		if e.ID == qv.Get("entity") {
			return e, month, nil
		}
	}
	return store.TaxEntity{}, "", fieldErr("choose a tax entity")
}

func (d *Dashboard) reportsPage(w http.ResponseWriter, r *http.Request, s *session) {
	ents, err := d.entityOpts(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "reports", "GST reports")
	v.Entities = ents
	qv := r.URL.Query()
	rv := &reportsView{Entity: qv.Get("entity"), Month: qv.Get("month")}
	v.Reports = rv
	if rv.Entity == "" && rv.Month == "" {
		rv.Month = d.o.Now().In(tax.IST).Format("2006-01")
		d.render(w, http.StatusOK, "dash_reports", v)
		return
	}
	ent, month, err := d.reportRequest(r)
	var fe fieldErr
	if errors.As(err, &fe) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	set, err := d.o.Invoice.Reports(r.Context(), ent.ID, month)
	if err != nil {
		d.fail(w, err)
		return
	}
	for _, f := range set.Files {
		rv.Files = append(rv.Files, reportFileRow{Name: f.Name,
			Href: prefix + "/reports/file?" + url.Values{"entity": {ent.ID}, "month": {month}, "name": {f.Name}}.Encode()})
	}
	rv.ZipHref = prefix + "/reports/zip?" + url.Values{"entity": {ent.ID}, "month": {month}}.Encode()
	if rv.EInvoice, err = d.reportEInvoice(r, ent.ID, month); err != nil {
		d.fail(w, err)
		return
	}
	d.render(w, http.StatusOK, "dash_reports", v)
}

func (d *Dashboard) reportSet(w http.ResponseWriter, r *http.Request) (invoice.ReportSet, bool) {
	ent, month, err := d.reportRequest(r)
	var fe fieldErr
	if errors.As(err, &fe) {
		badRequest(w, err.Error())
		return invoice.ReportSet{}, false
	}
	if err == nil {
		var set invoice.ReportSet
		if set, err = d.o.Invoice.Reports(r.Context(), ent.ID, month); err == nil {
			return set, true
		}
	}
	d.fail(w, err)
	return invoice.ReportSet{}, false
}

func (d *Dashboard) reportFile(w http.ResponseWriter, r *http.Request, s *session) {
	set, ok := d.reportSet(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	for _, f := range set.Files {
		if f.Name == name {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, f.Name))
			_, _ = w.Write(f.CSV)
			return
		}
	}
	http.NotFound(w, r)
}

func (d *Dashboard) reportZip(w http.ResponseWriter, r *http.Request, s *session) {
	set, ok := d.reportSet(w, r)
	if !ok {
		return
	}
	// Streamed to the client as it is compressed (the route runs outside the
	// timeout buffer, see server.IsStreamed). Once bytes are out a failure can only
	// cut the download short, which the client sees as a broken archive.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gst-reports-%s-%s.zip"`, set.Entity.GSTIN, set.Month))
	zw := zip.NewWriter(w)
	stamp, _, _ := monthBounds(set.Month)
	for _, f := range set.Files {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate, Modified: stamp})
		if err == nil {
			_, err = fw.Write(f.CSV)
		}
		if err != nil {
			d.o.Logger.Error("dashboard: report zip", "err", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		d.o.Logger.Error("dashboard: report zip", "err", err)
	}
}

// ---- tax entities ----

func parseISTDate(label, s string) (time.Time, error) {
	if s = strings.TrimSpace(s); s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, tax.IST)
	if err != nil {
		return time.Time{}, fieldErr(label + " must be a date like 2026-04-01")
	}
	return t, nil
}

func entForm(e store.TaxEntity) taxEntForm {
	f := taxEntForm{ID: e.ID, LegalName: e.LegalName, TradeName: e.TradeName, GSTIN: e.GSTIN, PAN: e.PAN, Address: e.Address, LUTNumber: e.LUTNumber,
		InvoicePrefix: e.InvoicePrefix, CreditNotePrefix: e.CreditNotePrefix, Signatory: e.SignatoryName, Footer: e.FooterNote,
		SupportEmail: e.SupportEmail, TurnoverBand: e.TurnoverBand, HasLogo: e.LogoBlobID != "", Editing: true}
	f.State, _ = tax.StateName(e.StateCode)
	f.DSCSubject = e.DSCSubject
	if !e.DSCNotAfter.IsZero() {
		f.DSCNotAfter = e.DSCNotAfter.In(tax.IST).Format("2006-01-02")
	}
	if !e.LUTValidFrom.IsZero() {
		f.LUTFrom = e.LUTValidFrom.In(tax.IST).Format("2006-01-02")
	}
	if !e.LUTValidTo.IsZero() {
		f.LUTTo = e.LUTValidTo.In(tax.IST).Format("2006-01-02")
	}
	return f
}

// formEntity reads the tax entity form (urlencoded or multipart).
func formEntity(r *http.Request) (invoice.TaxEntityInput, taxEntForm, error) {
	g := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	f := taxEntForm{LegalName: g("legal_name"), TradeName: g("trade_name"), GSTIN: strings.ToUpper(g("gstin")), PAN: strings.ToUpper(g("pan")), Address: g("address"),
		LUTNumber: g("lut_number"), LUTFrom: g("lut_from"), LUTTo: g("lut_to"), InvoicePrefix: strings.ToUpper(g("invoice_prefix")),
		CreditNotePrefix: strings.ToUpper(g("credit_note_prefix")), Signatory: g("signatory_name"), Footer: g("footer_note"),
		SupportEmail: g("support_email"), TurnoverBand: g("turnover_band")}
	in := invoice.TaxEntityInput{LegalName: f.LegalName, TradeName: f.TradeName, GSTIN: f.GSTIN, PAN: f.PAN, Address: f.Address, LUTNumber: f.LUTNumber,
		InvoicePrefix: f.InvoicePrefix, CreditNotePrefix: f.CreditNotePrefix, SignatoryName: f.Signatory, FooterNote: f.Footer, SupportEmail: f.SupportEmail,
		TurnoverBand: f.TurnoverBand, RemoveLogo: r.PostForm.Get("remove_logo") != ""}
	var err error
	if in.LUTFrom, err = parseISTDate("LUT valid-from", f.LUTFrom); err != nil {
		return in, f, err
	}
	in.LUTTo, err = parseISTDate("LUT valid-to", f.LUTTo)
	return in, f, err
}

// uploadedLogo returns the validated logo of a multipart form, or nil.
func uploadedLogo(r *http.Request) (data []byte, contentType string, err error) {
	if r.MultipartForm == nil {
		return nil, "", nil
	}
	fhs := r.MultipartForm.File["logo"]
	if len(fhs) == 0 || fhs[0].Filename == "" {
		return nil, "", nil
	}
	if fhs[0].Size > maxLogoBytes {
		return nil, "", fieldErr("The logo must be 512 KB or smaller.")
	}
	fh, err := fhs[0].Open()
	if err != nil {
		return nil, "", err
	}
	defer fh.Close()
	data, err = io.ReadAll(io.LimitReader(fh, maxLogoBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxLogoBytes {
		return nil, "", fieldErr("The logo must be 512 KB or smaller.")
	}
	ct, err := sniffLogo(data)
	if err != nil {
		return nil, "", fieldErr(err.Error())
	}
	return data, ct, nil
}

func (d *Dashboard) taxEntitiesPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderTaxEntities(w, r, s, http.StatusOK, "", nil)
}

func (d *Dashboard) renderTaxEntities(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string, echo *taxEntForm) {
	ents, err := d.listEntities(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "tax-entities", "Tax entities")
	v.Error, v.CanManage = errMsg, s.can(authz.SettingsWrite)
	v.Warnings = d.lutWarnings(ents)
	for _, e := range ents {
		st, _ := tax.StateName(e.StateCode)
		row := taxEntRow{ID: e.ID, Name: firstNonBlank(e.TradeName, e.LegalName), LegalName: e.LegalName, GSTIN: e.GSTIN, State: st,
			Prefixes: e.InvoicePrefix + " / " + e.CreditNotePrefix, LUT: e.LUTNumber}
		if !e.LUTValidTo.IsZero() {
			row.LUTTo = e.LUTValidTo.In(tax.IST).Format("2006-01-02")
		}
		v.TaxEnts = append(v.TaxEnts, row)
	}
	form := taxEntForm{InvoicePrefix: "INV", CreditNotePrefix: "CN"}
	if echo != nil {
		form = *echo
	}
	v.TaxForm = &form
	d.render(w, status, "dash_tax_entities", v)
}

// storeLogo saves an uploaded logo and returns its blob id ("" when none).
func (d *Dashboard) storeLogo(ctx context.Context, orgID string, data []byte, ct string) (string, error) {
	if data == nil {
		return "", nil
	}
	b := store.Blob{ID: ids.New("blob"), OrgID: orgID, Kind: "logo", ContentType: ct, Data: data, CreatedAt: d.o.Now()}
	return b.ID, d.o.DB.Write(ctx, func(tx store.Tx) error { return store.InsertBlob(ctx, tx, b) })
}

func (d *Dashboard) taxEntityCreate(w http.ResponseWriter, r *http.Request, s *session) {
	in, form, err := formEntity(r)
	var data []byte
	var ct string
	if err == nil {
		data, ct, err = uploadedLogo(r)
	}
	var fe fieldErr
	if errors.As(err, &fe) {
		d.renderTaxEntities(w, r, s, http.StatusBadRequest, err.Error(), &form)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	if in.LogoBlobID, err = d.storeLogo(r.Context(), org, data, ct); err != nil {
		d.fail(w, err)
		return
	}
	ent, err := d.o.Invoice.AddTaxEntity(r.Context(), s.auditActor(), in)
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/tax-entities/"+ent.ID, http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderTaxEntities(w, r, s, http.StatusBadRequest, strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": "), &form)
	default:
		d.fail(w, err)
	}
}

// loadEntity answers 404 for a missing entity or one of another org.
func (d *Dashboard) loadEntity(w http.ResponseWriter, r *http.Request) (store.TaxEntity, bool) {
	var e store.TaxEntity
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		e, err = store.GetTaxEntity(r.Context(), q, r.PathValue("id"))
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(r.Context(), e.OrgID) {
		http.NotFound(w, r)
		return e, false
	}
	if err != nil {
		d.fail(w, err)
		return e, false
	}
	return e, true
}

func (d *Dashboard) taxEntityPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderTaxEntity(w, r, s, http.StatusOK, "", nil)
}

func (d *Dashboard) renderTaxEntity(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string, echo *taxEntForm) {
	e, ok := d.loadEntity(w, r)
	if !ok {
		return
	}
	form := entForm(e)
	form.DSCSoon = !e.DSCNotAfter.IsZero() && e.DSCNotAfter.Before(d.o.Now().AddDate(0, 0, 30))
	if echo != nil {
		form.LegalName, form.TradeName, form.PAN, form.Address = echo.LegalName, echo.TradeName, echo.PAN, echo.Address
		form.LUTNumber, form.LUTFrom, form.LUTTo = echo.LUTNumber, echo.LUTFrom, echo.LUTTo
		form.InvoicePrefix, form.CreditNotePrefix, form.Signatory, form.Footer = echo.InvoicePrefix, echo.CreditNotePrefix, echo.Signatory, echo.Footer
		form.SupportEmail, form.TurnoverBand = echo.SupportEmail, echo.TurnoverBand
	}
	v := d.shell(r, s, "tax-entities", firstNonBlank(e.TradeName, e.LegalName))
	v.Error, v.CanManage, v.TaxForm = errMsg, s.can(authz.SettingsWrite), &form
	v.Warnings = d.lutWarnings([]store.TaxEntity{e})
	d.render(w, status, "dash_tax_entity", v)
}

func (d *Dashboard) taxEntityUpdate(w http.ResponseWriter, r *http.Request, s *session) {
	e, ok := d.loadEntity(w, r)
	if !ok {
		return
	}
	in, form, err := formEntity(r)
	var data []byte
	var ct string
	if err == nil {
		data, ct, err = uploadedLogo(r)
	}
	var fe fieldErr
	if errors.As(err, &fe) {
		d.renderTaxEntity(w, r, s, http.StatusBadRequest, err.Error(), &form)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if in.LogoBlobID, err = d.storeLogo(r.Context(), e.OrgID, data, ct); err != nil {
		d.fail(w, err)
		return
	}
	_, err = d.o.Invoice.UpdateTaxEntity(r.Context(), s.auditActor(), e.ID, in)
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/tax-entities/"+e.ID, http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderTaxEntity(w, r, s, http.StatusBadRequest, strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": "), &form)
	default:
		d.fail(w, err)
	}
}

// maxDSCBytes bounds the uploaded PKCS#12 file; the service refuses more than 64 KB.
const maxDSCBytes = 64 << 10

// taxEntityDSCSet uploads the digital signature certificate (PKCS#12) of an
// entity. The file and its password are validated, then stored sealed; neither
// is ever shown again.
func (d *Dashboard) taxEntityDSCSet(w http.ResponseWriter, r *http.Request, s *session) {
	e, ok := d.loadEntity(w, r)
	if !ok {
		return
	}
	var data []byte
	if r.MultipartForm != nil {
		if fhs := r.MultipartForm.File["dsc"]; len(fhs) > 0 && fhs[0].Filename != "" {
			fh, err := fhs[0].Open()
			if err != nil {
				d.fail(w, err)
				return
			}
			defer fh.Close()
			if data, err = io.ReadAll(io.LimitReader(fh, maxDSCBytes+1)); err != nil {
				d.fail(w, err)
				return
			}
		}
	}
	if len(data) == 0 {
		d.renderTaxEntity(w, r, s, http.StatusBadRequest, "Choose the PKCS#12 (.p12 or .pfx) file of the certificate.", nil)
		return
	}
	_, err := d.o.Invoice.SetTaxEntityDSC(r.Context(), s.auditActor(), e.ID, data, r.PostForm.Get("dsc_password"))
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/tax-entities/"+e.ID, http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderTaxEntity(w, r, s, http.StatusBadRequest, trimInvalid(err), nil)
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) taxEntityDSCRemove(w http.ResponseWriter, r *http.Request, s *session) {
	e, ok := d.loadEntity(w, r)
	if !ok {
		return
	}
	if err := d.o.Invoice.ClearTaxEntityDSC(r.Context(), s.auditActor(), e.ID); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/tax-entities/"+e.ID, http.StatusSeeOther)
}

func (d *Dashboard) taxEntityLogo(w http.ResponseWriter, r *http.Request, s *session) {
	e, ok := d.loadEntity(w, r)
	if !ok {
		return
	}
	var blob store.Blob
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		if e.LogoBlobID == "" {
			return store.ErrNotFound
		}
		blob, err = store.GetBlob(r.Context(), q, e.LogoBlobID)
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", blob.ContentType)
	_, _ = w.Write(blob.Data)
}

// ---- app tax profile (a tab of the app page) ----

func formatPercent(bp int) string { return strconv.FormatFloat(float64(bp)/100, 'f', -1, 64) }

func parsePercent(s string) (int, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	bp := math.Round(f * 100)
	if err != nil || math.Abs(f*100-bp) > 1e-6 || bp < 0 || bp > 2800 {
		return 0, fieldErr("The GST rate must be a percentage between 0 and 28, like 18 or 12.5.")
	}
	return int(bp), nil
}

func (d *Dashboard) taxProfileFor(ctx context.Context, appID string) (*taxProfileView, error) {
	pv := &taxProfileView{SAC: "998315", RatePct: "18", Issue: true, Inclusive: true}
	var prof store.AppTaxProfile
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { prof, err = store.GetAppTaxProfile(ctx, q, appID); return })
	switch {
	case err == nil:
		pv.Exists, pv.TaxEntityID, pv.SAC, pv.RatePct = true, prof.TaxEntityID, prof.SACCode, formatPercent(prof.GSTRateBP)
		pv.Issue, pv.Inclusive, pv.InvoicePrefix, pv.CreditNotePrefix = prof.IssueInvoices, prof.PricesIncludeTax, prof.InvoicePrefix, prof.CreditNotePrefix
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	ents, err := d.listEntities(ctx)
	for _, e := range ents {
		pv.Entities = append(pv.Entities, entityOpt{ID: e.ID, Name: firstNonBlank(e.TradeName, e.LegalName), GSTIN: e.GSTIN})
	}
	return pv, err
}

func (d *Dashboard) appTaxSave(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	g := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	bp, err := parsePercent(g("gst_rate"))
	if err == nil {
		_, err = d.o.Invoice.SetAppTaxProfile(r.Context(), s.auditActor(), invoice.AppTaxInput{AppID: app.ID, TaxEntityID: g("tax_entity_id"),
			IssueInvoices: r.PostForm.Get("issue_invoices") != "", SACCode: g("sac_code"), GSTRateBP: bp, PricesIncludeTax: r.PostForm.Get("prices_include_tax") != "",
			InvoicePrefix: strings.ToUpper(g("invoice_prefix")), CreditNotePrefix: strings.ToUpper(g("credit_note_prefix"))})
	}
	var fe fieldErr
	switch {
	case err == nil:
		d.appRedirect(w, r, app, "tax")
	case errors.As(err, &fe):
		d.renderApp(w, r, s, app, http.StatusBadRequest, "tax", err.Error(), nil)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderApp(w, r, s, app, http.StatusBadRequest, "tax", strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": "), nil)
	default:
		d.fail(w, err)
	}
}

// ---- FX rates ----

func (d *Dashboard) fxPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderFX(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderFX(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	var rates []store.FXRate
	var pending []store.Invoice
	err = d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if rates, err = store.ListFXRates(ctx, q, org, 60); err != nil {
			return err
		}
		pending, err = store.AllInvoicesPendingFX(ctx, q, org)
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "fx", "FX rates")
	v.Error, v.CanManage, v.FXDate = errMsg, s.can(authz.SettingsWrite), d.today()
	for _, fr := range rates {
		v.FXRates = append(v.FXRates, fxRow{Currency: fr.Currency, Date: fr.Date, Rate: fr.Rate, Source: fr.Source})
	}
	counts := map[[2]string]int{}
	var order [][2]string
	for _, inv := range pending {
		k := [2]string{inv.Currency, inv.IssuedAt.In(tax.IST).Format("2006-01-02")}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	for _, k := range order {
		v.FXPending = append(v.FXPending, fxPendingRow{Currency: k[0], Date: k[1], Count: counts[k]})
	}
	d.render(w, status, "dash_fx", v)
}

func (d *Dashboard) fxSave(w http.ResponseWriter, r *http.Request, s *session) {
	err := d.o.Invoice.SetFXRate(r.Context(), s.auditActor(), strings.ToUpper(strings.TrimSpace(r.PostForm.Get("currency"))),
		strings.TrimSpace(r.PostForm.Get("date")), strings.TrimSpace(r.PostForm.Get("rate")), "manual")
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/fx", http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderFX(w, r, s, http.StatusBadRequest, strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": "))
	default:
		d.fail(w, err)
	}
}
