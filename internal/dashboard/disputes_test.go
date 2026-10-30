package dashboard

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

// noResponder hides the DisputeResponder side of a connector: an interface
// field exposes only what the interface declares.
type noResponder struct{ connector.Connector }

// disputeAccount creates a live provider account backed by a scripted fake
// connector; responder false builds one that cannot accept disputes or take
// evidence, like Zoho.
func (e *env) disputeAccount(app store.App, responder bool) (store.ProviderAccount, *fakeconn.State) {
	e.t.Helper()
	st := fakeconn.Register("dispfake")
	name := st.Name
	if !responder {
		fac, info, _ := connector.Lookup(st.Name)
		name = st.Name + "-nr"
		connector.Register(name, func(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
			c, err := fac(cfg, deps)
			if err != nil {
				return nil, err
			}
			return noResponder{c}, nil
		}, info)
	}
	pa, err := e.conn.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
		Connector: name, Name: "Acct " + name, Mode: "live", Config: json.RawMessage(fakeconn.ValidConfig), AppID: app.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	return pa, st
}

// openDispute records a dispute on the payment the way a webhook would.
func (e *env) openDispute(st *fakeconn.State, pa store.ProviderAccount, p store.Payment, id string, status connector.DisputeStatus, due time.Time) store.Dispute {
	e.t.Helper()
	var atts []store.Attempt
	_ = e.db.Read(ctx, func(q store.Querier) (err error) { atts, err = store.ListAttempts(ctx, q, p.ID); return })
	snap := connector.DisputeSnapshot{ProviderDisputeID: id, ProviderPaymentID: atts[0].ProviderPaymentID, Status: connector.DisputeNeedsResponse, RawStatus: "open",
		Phase: "chargeback", Reason: "fraudulent", Amount: p.AmountReceived, Currency: "INR", CreatedAt: e.clk.Now().UnixMilli()}
	if !due.IsZero() {
		snap.DueBy = due.UnixMilli()
	}
	st.Locked(func() { st.Disputes[id] = snap })
	res, err := e.disp.Apply(ctx, pa.ID, snap, "webhook")
	if err != nil {
		e.t.Fatal(err)
	}
	if status != connector.DisputeNeedsResponse {
		snap.Status, snap.RawStatus = status, string(status)
		st.Locked(func() { st.Disputes[id] = snap })
		if res, err = e.disp.Apply(ctx, pa.ID, snap, "webhook"); err != nil {
			e.t.Fatal(err)
		}
	}
	return res.Dispute
}

func TestDisputeInboxListsByDeadlineWithFilters(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, _ := e.taxApp("alpha", supplierGSTIN)
		beta, _ := e.taxApp("beta", supplierGSTIN)
		pa, st := e.disputeAccount(alpha, true)
		paNR, stNR := e.disputeAccount(beta, false)
		now := e.clk.Now()
		pay := func(app store.App, acct string) store.Payment { return e.payVia(app, 50000, "INR", intraB2C, acct) }
		d1 := e.openDispute(st, pa, pay(alpha, pa.ID), "dsp_soon", connector.DisputeNeedsResponse, now.Add(24*time.Hour))
		d2 := e.openDispute(st, pa, pay(alpha, pa.ID), "dsp_later", connector.DisputeNeedsResponse, now.Add(10*24*time.Hour))
		d3 := e.openDispute(st, pa, pay(alpha, pa.ID), "dsp_review", connector.DisputeUnderReview, now.Add(5*24*time.Hour))
		d4 := e.openDispute(stNR, paNR, pay(beta, paNR.ID), "dsp_nr", connector.DisputeNeedsResponse, now.Add(20*time.Hour))
		e.otherOrg()
		c := e.roleClient("viewer")
		e.setMode(c, "live")
		link := func(d store.Dispute) string { return `href="/dashboard/disputes/` + d.ID + `"` }

		body := e.pageOK(c, "/dashboard/disputes")
		has(t, body, link(d1), link(d2), link(d3), link(d4))
		// soonest deadline first: d4 (20h), d1 (24h), d3 (5d), d2 (10d)
		order := []store.Dispute{d4, d1, d3, d2}
		for i := 0; i+1 < len(order); i++ {
			if strings.Index(body, link(order[i])) > strings.Index(body, link(order[i+1])) {
				t.Errorf("%s should be listed before %s", order[i].ID, order[i+1].ID)
			}
		}
		// under 72 hours is highlighted
		row := func(d store.Dispute) string {
			for _, seg := range strings.Split(body, "<tr") {
				if strings.Contains(seg, `/disputes/`+d.ID+`"`) {
					return "<tr" + seg
				}
			}
			t.Fatalf("no row for %s", d.ID)
			return ""
		}
		has(t, row(d1), "due-soon")
		has(t, row(d4), "due-soon")
		lacks(t, row(d2), "due-soon")
		lacks(t, row(d3), "due-soon")

		for name, c2 := range map[string]struct {
			q         string
			want, not []store.Dispute
		}{
			"status":   {"?status=under_review", []store.Dispute{d3}, []store.Dispute{d1, d2, d4}},
			"app":      {"?app=" + beta.ID, []store.Dispute{d4}, []store.Dispute{d1, d2, d3}},
			"provider": {"?provider=" + pa.Connector, []store.Dispute{d1, d2, d3}, []store.Dispute{d4}},
		} {
			b := e.pageOK(c, "/dashboard/disputes"+c2.q)
			for _, d := range c2.want {
				if !strings.Contains(b, link(d)) {
					t.Errorf("%s: missing %s", name, d.ID)
				}
			}
			for _, d := range c2.not {
				if strings.Contains(b, link(d)) {
					t.Errorf("%s: unexpected %s", name, d.ID)
				}
			}
		}
		lacks(t, body, "dsp_foreign")
		for _, q := range []string{"?status=bogus", "?app=app_nope"} {
			if r, _ := e.get(c, "/dashboard/disputes"+q); r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
		}
	})
}

func TestDisputeDetailAndActions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		pa, st := e.disputeAccount(app, true)
		p := e.payVia(app, 50000, "INR", intraB2C, pa.ID)
		d := e.openDispute(st, pa, p, "dsp_1", connector.DisputeNeedsResponse, e.clk.Now().Add(48*time.Hour))
		page := "/dashboard/disputes/" + d.ID

		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, page)
		has(t, vb, d.ID, "dsp_1", "needs_response", "fraudulent", "500.00 INR", `href="/dashboard/payments/`+p.ID+`"`, "chargeback")
		lacks(t, vb, `action="`+page+`/accept"`, `action="`+page+`/evidence"`)
		if r, _ := e.post(viewer, page+"/accept", url.Values{"csrf": {csrfOf(t, vb)}}); r.StatusCode != 403 {
			t.Fatalf("viewer accept: %d", r.StatusCode)
		}

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, `action="`+page+`/accept"`, `action="`+page+`/evidence"`, `enctype="multipart/form-data"`)
		lacks(t, body, "provider's portal")
		tok := csrfOf(t, body)

		// evidence: notes plus a file
		png := pngBytes(t, 8, 8, false)
		r, b := e.postMultipart(op, page+"/evidence", map[string]string{"notes": "Delivered on 3 Nov, signed.", "type_1": "shipping_proof", "csrf": tok}, "file_1", "proof.png", png)
		if r.StatusCode != 303 {
			t.Fatalf("evidence: %d\n%s", r.StatusCode, b)
		}
		if len(st.Evidence) != 1 || st.Evidence[0].Text != "Delivered on 3 Nov, signed." || len(st.Evidence[0].Files) != 1 {
			t.Fatalf("evidence sent to the provider: %+v", st.Evidence)
		}
		body = e.pageOK(op, page)
		cur, _ := e.disp.Get(ctx, core.OrgScope{OrgID: store.DefaultOrgID}, d.ID)
		var doc struct {
			Notes   string   `json:"notes"`
			BlobIDs []string `json:"blob_ids"`
		}
		_ = json.Unmarshal([]byte(cur.Evidence), &doc)
		if len(doc.BlobIDs) != 1 {
			t.Fatalf("evidence doc %s", cur.Evidence)
		}
		has(t, body, "Delivered on 3 Nov, signed.", "image/png", `href="`+page+`/evidence/`+doc.BlobIDs[0]+`"`)
		rr, got := e.get(op, page+"/evidence/"+doc.BlobIDs[0])
		if rr.StatusCode != 200 || rr.Header.Get("Content-Type") != "image/png" || got != string(png) {
			t.Fatalf("download: %d %q", rr.StatusCode, rr.Header.Get("Content-Type"))
		}
		if rr, _ := e.get(op, page+"/evidence/blob_nope"); rr.StatusCode != 404 {
			t.Errorf("unknown evidence: %d", rr.StatusCode)
		}
		// evidence validation
		gif := []byte("GIF89a......")
		for name, c := range map[string]struct {
			fields map[string]string
			file   []byte
		}{
			"gif":     {map[string]string{"notes": "x", "type_1": "proof", "csrf": tok}, gif},
			"no type": {map[string]string{"notes": "x", "csrf": tok}, png},
			"nothing": {map[string]string{"csrf": tok}, nil},
		} {
			file, fname := "", ""
			if c.file != nil {
				file, fname = "file_1", "f.bin"
			}
			if r, _ := e.postMultipart(op, page+"/evidence", c.fields, file, fname, c.file); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if len(st.Evidence) != 1 {
			t.Fatal("rejected evidence reached the provider")
		}

		// accept
		if r, _ := e.post(op, page+"/accept", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("accept: %d", r.StatusCode)
		}
		if len(st.Accepts) != 1 || st.Accepts[0].ProviderDisputeID != "dsp_1" {
			t.Fatalf("accepts %+v", st.Accepts)
		}
		for _, act := range []string{"dispute.accept", "dispute.submit_evidence"} {
			var aud []store.AuditRow
			_ = db.Read(ctx, func(q store.Querier) (err error) {
				aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, act, 5)
				return
			})
			if len(aud) != 1 || aud[0].ActorType != "user" || aud[0].ActorID != "usr_operator" || aud[0].Target != d.ID {
				t.Errorf("%s audit: %+v", act, aud)
			}
		}
		if r, _ := e.post(op, "/dashboard/disputes/dsp_nope/accept", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown dispute: %d", r.StatusCode)
		}
	})
}

func TestDisputeWithoutResponderShowsPortalNoteAndManualStatus(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		pa, st := e.disputeAccount(app, false)
		p := e.payVia(app, 50000, "INR", intraB2C, pa.ID)
		d := e.openDispute(st, pa, p, "dsp_nr", connector.DisputeNeedsResponse, e.clk.Now().Add(48*time.Hour))
		page := "/dashboard/disputes/" + d.ID
		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, "provider's portal")
		lacks(t, body, `action="`+page+`/accept"`, `action="`+page+`/evidence"`)
		tok := csrfOf(t, body)
		// a provider-synced dispute is not edited by hand
		lacks(t, body, `action="`+page+`/status"`)
		if r, b := e.post(op, page+"/accept", url.Values{"csrf": {tok}}); r.StatusCode != 400 || !strings.Contains(b, "cannot respond") {
			t.Errorf("accept via a connector that cannot respond: %d", r.StatusCode)
		}
		if r, _ := e.post(op, page+"/status", url.Values{"status": {"won"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("manual status of a synced dispute: %d", r.StatusCode)
		}

		// a dispute entered by hand: record it, then mark its outcome
		before := e.clk.Now().Add(72 * time.Hour).In(taxIST).Format("2006-01-02")
		form := url.Values{"payment_id": {p.ID}, "amount": {"250.50"}, "reason": {"Customer disputes the charge"}, "stage": {"chargeback"}, "due_by": {before}, "csrf": {tok}}
		r, b := e.post(op, "/dashboard/disputes", form)
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/dashboard/disputes/dsp_") {
			t.Fatalf("manual entry: %d %s\n%s", r.StatusCode, r.Header.Get("Location"), b)
		}
		manual, err := e.disp.Get(ctx, core.OrgScope{OrgID: store.DefaultOrgID}, strings.TrimPrefix(r.Header.Get("Location"), "/dashboard/disputes/"))
		if err != nil || manual.Source != "manual" || manual.Amount != 25050 || manual.Status != "needs_response" || manual.Reason != "Customer disputes the charge" || manual.DueBy.IsZero() {
			t.Fatalf("manual dispute %+v %v", manual, err)
		}
		mpage := "/dashboard/disputes/" + manual.ID
		body = e.pageOK(op, mpage)
		has(t, body, `action="`+mpage+`/status"`, "Mark as", "provider's portal")
		lacks(t, body, `action="`+mpage+`/accept"`)
		if r, _ := e.post(op, mpage+"/status", url.Values{"status": {"under_review"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("mark: %d", r.StatusCode)
		}
		if r, _ := e.post(op, mpage+"/status", url.Values{"status": {"lost"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("mark lost: %d", r.StatusCode)
		}
		if cur, _ := e.disp.Get(ctx, core.OrgScope{OrgID: store.DefaultOrgID}, manual.ID); cur.Status != "lost" {
			t.Fatalf("status %s", cur.Status)
		}
		if r, _ := e.post(op, mpage+"/status", url.Values{"status": {"won"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("leaving a final state: %d", r.StatusCode)
		}
		if r, _ := e.post(op, mpage+"/status", url.Values{"status": {"bogus"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("bogus status: %d", r.StatusCode)
		}
		// manual entry validation
		for name, o := range map[string]map[string]string{
			"unknown payment": {"payment_id": "pay_nope"},
			"zero amount":     {"amount": "0"},
			"not a number":    {"amount": "lots"},
			"over the paid":   {"amount": "9999.00"},
			"bad date":        {"due_by": "tomorrow"},
		} {
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			for k, v := range o {
				f.Set(k, v)
			}
			if r, _ := e.post(op, "/dashboard/disputes", f); r.StatusCode != 400 && r.StatusCode != 404 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, "/dashboard/disputes", url.Values{"payment_id": {p.ID}, "amount": {"1"}, "csrf": {csrfOf(t, e.pageOK(viewer, "/dashboard/disputes"))}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "dispute.create_manual", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" {
			t.Errorf("audit %+v", aud)
		}
	})
}

func TestDisputeCreditNoteWithReasonCode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		pa, st := e.disputeAccount(app, true)
		p := e.payVia(app, 50000, "INR", intraB2C, pa.ID)
		open := e.openDispute(st, pa, p, "dsp_open", connector.DisputeNeedsResponse, e.clk.Now().Add(48*time.Hour))
		p2 := e.payVia(app, 50000, "INR", intraB2C, pa.ID)
		lost := e.openDispute(st, pa, p2, "dsp_lost", connector.DisputeLost, time.Time{})
		page := "/dashboard/disputes/" + lost.ID

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, `action="`+page+`/credit-note"`, `name="reason_code"`, "chargeback_lost")
		lacks(t, e.pageOK(op, "/dashboard/disputes/"+open.ID), `action="/dashboard/disputes/`+open.ID+`/credit-note"`) // only lost or accepted
		tok := csrfOf(t, body)

		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, page+"/credit-note", url.Values{"reason_code": {"chargeback_lost"}, "csrf": {csrfOf(t, e.pageOK(viewer, page))}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}
		if r, _ := e.post(op, page+"/credit-note", url.Values{"reason_code": {"made-up"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("unknown reason code: %d", r.StatusCode)
		}
		if r, b := e.post(op, "/dashboard/disputes/"+open.ID+"/credit-note", url.Values{"reason_code": {"chargeback_lost"}, "csrf": {tok}}); r.StatusCode != 400 || !strings.Contains(b, "lost or accepted") {
			t.Errorf("open dispute: %d", r.StatusCode)
		}
		r, b := e.post(op, page+"/credit-note", url.Values{"reason_code": {"chargeback_lost"}, "csrf": {tok}})
		cns := e.creditNotesOf(p2.ID)
		if r.StatusCode != 303 || len(cns) != 1 || cns[0].DisputeID != lost.ID || r.Header.Get("Location") != "/dashboard/credit-notes/"+cns[0].ID {
			t.Fatalf("credit note: %d %s (%d notes)\n%s", r.StatusCode, r.Header.Get("Location"), len(cns), b)
		}
		// asking again returns the same note
		if r, _ := e.post(op, page+"/credit-note", url.Values{"reason_code": {"chargeback_lost"}, "csrf": {tok}}); r.StatusCode != 303 || len(e.creditNotesOf(p2.ID)) != 1 {
			t.Fatalf("repeat: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "dispute.credit_note", 5)
			return
		})
		if len(aud) == 0 || aud[0].ActorID != "usr_operator" {
			t.Fatalf("audit %+v", aud)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "dispute.credit_note_reason", 5)
			return
		})
		if len(aud) == 0 || !strings.Contains(aud[0].After, "chargeback_lost") || aud[0].Target != lost.ID {
			t.Fatalf("reason code audit %+v", aud)
		}
	})
}

func TestDisputesOfAnotherOrgAreInvisible(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.otherOrg()
		err := db.Write(ctx, func(tx store.Tx) error {
			return store.InsertDispute(ctx, tx, store.Dispute{ID: "dsp_foreign", OrgID: "org_other", AppID: "app_foreign", Mode: "live", PaymentID: "pay_foreign", AttemptID: "att_x",
				ProviderAccountID: "pa_x", ProviderDisputeID: "pd_x", Amount: 1, Currency: "INR", Status: "needs_response", Source: "webhook", CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		op := e.roleClient("operator")
		lacks(t, e.pageOK(op, "/dashboard/disputes"), "dsp_foreign")
		tok := csrfOf(t, e.pageOK(op, "/dashboard/disputes"))
		if r, _ := e.get(op, "/dashboard/disputes/dsp_foreign"); r.StatusCode != 404 {
			t.Errorf("detail: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/disputes/dsp_foreign/accept", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("accept: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/disputes", url.Values{"payment_id": {"pay_foreign"}, "amount": {"1"}, "csrf": {tok}}); r.StatusCode != 404 && r.StatusCode != 400 {
			t.Errorf("manual entry against another org's payment: %d", r.StatusCode)
		}
	})
}

func TestNavLinksCarryOpenDisputeAndAlertCounts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, _ := e.taxApp("alpha", supplierGSTIN)
		pa, st := e.disputeAccount(alpha, true)
		c := e.roleClient("viewer")
		body := e.pageOK(c, "/dashboard/overview")
		lacks(t, body, "data-count")

		now := e.clk.Now()
		pay := func() store.Payment { return e.payVia(alpha, 50000, "INR", intraB2C, pa.ID) }
		e.openDispute(st, pa, pay(), "dsp_a", connector.DisputeNeedsResponse, now.Add(24*time.Hour))
		e.openDispute(st, pa, pay(), "dsp_b", connector.DisputeNeedsResponse, now.Add(48*time.Hour))
		e.openDispute(st, pa, pay(), "dsp_c", connector.DisputeUnderReview, now.Add(72*time.Hour)) // waiting on the provider
		e.otherOrg()
		for _, page := range []string{"/dashboard/overview", "/dashboard/payments"} {
			body = e.pageOK(c, page)
			has(t, body, `href="/dashboard/disputes" data-count="2"`)
			lacks(t, body, `href="/dashboard/alerts" data-count`)
		}
		e.raise(notify.SeverityWarning, "domain.failed", "pay.shop.example.org failed", map[string]string{"domain": "d1"})
		has(t, e.pageOK(c, "/dashboard/overview"), `href="/dashboard/alerts" data-count="1"`)
	})
}
