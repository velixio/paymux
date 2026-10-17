package dashboard

import (
	"bytes"
	"crypto/rand"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var (
	keyTokenRE = regexp.MustCompile(`pmx_(test|live)_sk_[0-9A-Za-z]{43}`)
	secretRE   = regexp.MustCompile(`whsec_[A-Za-z0-9+/=]+`)
)

func (e *env) postMultipart(c *http.Client, path string, fields map[string]string, file, fileName string, data []byte) (*http.Response, string) {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	if file != "" {
		fw, _ := w.CreateFormFile(file, fileName)
		_, _ = fw.Write(data)
	}
	_ = w.Close()
	resp, err := c.Post(e.srv.URL+path, w.FormDataContentType(), &buf)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func pngBytes(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if noisy {
		_, _ = rand.Read(img.Pix)
	} else {
		for i := range img.Pix {
			img.Pix[i] = 200
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestAppListAndCreate(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedApp("alpha")
		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/apps")
		has(t, body, "Alpha", "alpha")
		lacks(t, body, `action="/dashboard/apps"`)
		vt := csrfOf(t, body)
		if r, _ := e.post(viewer, "/dashboard/apps", url.Values{"name": {"X"}, "slug": {"x"}, "csrf": {vt}}); r.StatusCode != 403 {
			t.Fatalf("viewer create: %d", r.StatusCode)
		}

		admin := e.roleClient("admin")
		body = e.pageOK(admin, "/dashboard/apps")
		has(t, body, `action="/dashboard/apps"`)
		tok := csrfOf(t, body)
		r, _ := e.post(admin, "/dashboard/apps", url.Values{"name": {"Voicestudio"}, "slug": {"voicestudio"}, "return_url": {"https://vs.example.com/done"}, "csrf": {tok}})
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/dashboard/apps/app_") {
			t.Fatalf("create: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		app, err := e.core.AppBySlug(ctx, "voicestudio")
		if err != nil || app.DefaultReturnURL != "https://vs.example.com/done" {
			t.Fatalf("%+v %v", app, err)
		}
		if r.Header.Get("Location") != "/dashboard/apps/"+app.ID {
			t.Errorf("redirect %s", r.Header.Get("Location"))
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) { aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "app.create", 10); return })
		found := false
		for _, a := range aud {
			found = found || a.ActorType == "user" && a.ActorID == "usr_admin" && a.Target == app.ID
		}
		if !found {
			t.Errorf("app.create not audited with the user as actor: %+v", aud)
		}
		for name, c := range map[string]struct {
			form url.Values
			code int
		}{
			"duplicate slug": {url.Values{"name": {"Again"}, "slug": {"voicestudio"}}, 409},
			"bad slug":       {url.Values{"name": {"Again"}, "slug": {"Bad Slug"}}, 400},
			"no name":        {url.Values{"name": {" "}, "slug": {"ok"}}, 400},
			"bad return url": {url.Values{"name": {"N"}, "slug": {"n"}, "return_url": {"javascript:alert(1)"}}, 400},
		} {
			c.form.Set("csrf", tok)
			if r, _ := e.post(admin, "/dashboard/apps", c.form); r.StatusCode != c.code {
				t.Errorf("%s: %d want %d", name, r.StatusCode, c.code)
			}
		}
		if r, _ := e.get(admin, "/dashboard/apps/app_nope"); r.StatusCode != 404 {
			t.Errorf("unknown app: %d", r.StatusCode)
		}
	})
}

func TestAPIKeysShownOnceAndRevocable(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page))

		r, body := e.post(admin, page+"/keys", url.Values{"mode": {"test"}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("create key: %d\n%s", r.StatusCode, body)
		}
		token := keyTokenRE.FindString(body)
		if token == "" || !strings.HasPrefix(token, "pmx_test_sk_") {
			t.Fatalf("token not shown: %s", body)
		}
		has(t, body, "only time")
		if _, err := e.core.Authenticate(ctx, token); err != nil {
			t.Fatalf("new key does not authenticate: %v", err)
		}
		// never again
		again := e.pageOK(admin, page+"?tab=keys")
		lacks(t, again, token)
		has(t, again, token[:len("pmx_test_sk_")+8]) // the prefix is listed
		var keys []store.APIKey
		_ = db.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, app.ID); return })
		if len(keys) != 1 {
			t.Fatalf("keys %+v", keys)
		}

		if r, _ := e.post(admin, page+"/keys", url.Values{"mode": {"staging"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("bad mode: %d", r.StatusCode)
		}
		operator := e.roleClient("operator")
		ot := csrfOf(t, e.pageOK(operator, page))
		if r, _ := e.post(operator, page+"/keys", url.Values{"mode": {"live"}, "csrf": {ot}}); r.StatusCode != 403 {
			t.Errorf("operator create key: %d", r.StatusCode)
		}
		if r, _ := e.post(operator, page+"/keys/"+keys[0].ID+"/revoke", url.Values{"csrf": {ot}}); r.StatusCode != 403 {
			t.Errorf("operator revoke: %d", r.StatusCode)
		}

		// revoke
		other := e.seedApp("beta")
		if r, _ := e.post(admin, "/dashboard/apps/"+other.ID+"/keys/"+keys[0].ID+"/revoke", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("revoking through another app: %d", r.StatusCode)
		}
		r, _ = e.post(admin, page+"/keys/"+keys[0].ID+"/revoke", url.Values{"csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("revoke: %d", r.StatusCode)
		}
		if _, err := e.core.Authenticate(ctx, token); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("revoked key still works: %v", err)
		}
		has(t, e.pageOK(admin, page+"?tab=keys"), "revoked")
		var aud []store.AuditRow
		for _, act := range []string{"api_key.create", "api_key.revoke"} {
			_ = db.Read(ctx, func(q store.Querier) (err error) { aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, act, 10); return })
			if len(aud) != 1 || aud[0].ActorID != "usr_admin" {
				t.Errorf("%s audit: %+v", act, aud)
			}
		}
	})
}

func TestWebhookEndpointsFromDashboard(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, other := e.seedApp("alpha"), e.seedApp("beta")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page+"?tab=webhooks"))

		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		defer sink.Close()
		r, body := e.post(admin, page+"/endpoints", url.Values{"url": {sink.URL}, "events": {"payment.succeeded, refund.created"}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("add endpoint: %d\n%s", r.StatusCode, body)
		}
		secret := secretRE.FindString(body)
		if secret == "" {
			t.Fatalf("secret not shown: %s", body)
		}
		eps, _ := e.out.ListEndpoints(ctx, app.ID)
		if len(eps) != 1 || !eps[0].Enabled || strings.Join(eps[0].EventTypes, ",") != "payment.succeeded,refund.created" {
			t.Fatalf("endpoints %+v", eps)
		}
		again := e.pageOK(admin, page+"?tab=webhooks")
		lacks(t, again, secret)
		has(t, again, sink.URL, "payment.succeeded", "refund.created")

		if r, _ := e.post(admin, page+"/endpoints", url.Values{"url": {"ftp://x"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("bad url: %d", r.StatusCode)
		}
		viewer := e.roleClient("viewer")
		vt := csrfOf(t, e.pageOK(viewer, page+"?tab=webhooks"))
		if r, _ := e.post(viewer, page+"/endpoints", url.Values{"url": {sink.URL}, "csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("viewer add: %d", r.StatusCode)
		}

		// deliveries of the endpoint, replayable by an operator
		err := db.Write(ctx, func(tx store.Tx) error {
			return e.out.Emit(ctx, tx, events.Event{OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", Type: "payment.succeeded",
				ObjectType: "payment", ObjectID: "pay_x", ObjectVersion: 1, Data: map[string]any{"id": "pay_x"}})
		})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs()
		body = e.pageOK(admin, page+"?tab=webhooks&endpoint="+eps[0].ID)
		has(t, body, "payment.succeeded", "delivered", "/replay")

		// disable, and only through the owning app
		if r, _ := e.post(admin, "/dashboard/apps/"+other.ID+"/endpoints/"+eps[0].ID+"/disable", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("disable via another app: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, page+"/endpoints/"+eps[0].ID+"/disable", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("disable: %d", r.StatusCode)
		}
		eps, _ = e.out.ListEndpoints(ctx, app.ID)
		if eps[0].Enabled {
			t.Error("still enabled")
		}
		has(t, e.pageOK(admin, page+"?tab=webhooks"), "disabled")
		var aud []store.AuditRow
		for _, act := range []string{"webhook_endpoint.create", "webhook_endpoint.disable"} {
			_ = db.Read(ctx, func(q store.Querier) (err error) { aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, act, 10); return })
			if len(aud) != 1 || aud[0].ActorID != "usr_admin" || strings.Contains(aud[0].After+aud[0].Before, "whsec_") {
				t.Errorf("%s audit: %+v", act, aud)
			}
		}
	})
}

func TestBrandingAndLogoUpload(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page+"?tab=branding"))
		post := func(fields map[string]string, name string, data []byte) (*http.Response, string) {
			fields["csrf"] = tok
			return e.postMultipart(admin, page+"/branding", fields, "logo", name, data)
		}

		if r, _ := e.get(admin, page+"/logo"); r.StatusCode != 404 {
			t.Errorf("logo before upload: %d", r.StatusCode)
		}
		good := pngBytes(t, 16, 16, false)
		r, body := post(map[string]string{"display_name": "Alpha Pay", "accent": "#ff8800", "return_url": "https://alpha.example.com/back"}, "logo.png", good)
		if r.StatusCode != 303 {
			t.Fatalf("save: %d\n%s", r.StatusCode, body)
		}
		var br, ret string
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			br, err = store.GetAppBranding(ctx, q, app.ID)
			if err != nil {
				return
			}
			a, err := store.GetApp(ctx, q, app.ID)
			ret = a.DefaultReturnURL
			return err
		})
		has(t, br, `"name":"Alpha Pay"`, `"accent":"#ff8800"`, `"logo_blob_id":"blob_`)
		if ret != "https://alpha.example.com/back" {
			t.Fatalf("return url %q", ret)
		}
		lr, logo := e.get(admin, page+"/logo")
		if lr.StatusCode != 200 || lr.Header.Get("Content-Type") != "image/png" || logo != string(good) {
			t.Fatalf("logo: %d %q", lr.StatusCode, lr.Header.Get("Content-Type"))
		}
		body = e.pageOK(admin, page+"?tab=branding")
		has(t, body, "Alpha Pay", "#ff8800", page+"/logo")

		// saving without a new file keeps the logo
		if r, _ := post(map[string]string{"display_name": "Alpha 2", "accent": "#ff8800", "return_url": ""}, "", nil); r.StatusCode != 303 {
			t.Fatalf("resave: %d", r.StatusCode)
		}
		if _, l := e.get(admin, page+"/logo"); l != string(good) {
			t.Error("logo lost on resave")
		}

		// JPEG is fine too
		var jb bytes.Buffer
		_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
		if r, _ := post(map[string]string{"display_name": "A", "accent": ""}, "x.jpg", jb.Bytes()); r.StatusCode != 303 {
			t.Fatalf("jpeg: %d", r.StatusCode)
		}
		if lr, _ := e.get(admin, page+"/logo"); lr.Header.Get("Content-Type") != "image/jpeg" {
			t.Errorf("jpeg served as %s", lr.Header.Get("Content-Type"))
		}

		// rejections
		var gb bytes.Buffer
		_ = gif.Encode(&gb, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil)
		huge := pngBytes(t, 700, 700, true)
		if len(huge) <= 512*1024 {
			t.Fatalf("fixture too small: %d", len(huge))
		}
		for name, c := range map[string]struct {
			fields map[string]string
			file   string
			data   []byte
			want   string
		}{
			"gif":              {map[string]string{"display_name": "A"}, "a.gif", gb.Bytes(), "PNG or JPEG"},
			"text named png":   {map[string]string{"display_name": "A"}, "a.png", []byte("<html>not an image</html>"), "PNG or JPEG"},
			"svg":              {map[string]string{"display_name": "A"}, "a.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "PNG or JPEG"},
			"truncated png":    {map[string]string{"display_name": "A"}, "a.png", good[:30], "PNG or JPEG"},
			"over 512 KB":      {map[string]string{"display_name": "A"}, "big.png", huge, "512 KB"},
			"bad accent":       {map[string]string{"display_name": "A", "accent": "red"}, "", nil, "accent"},
			"bad return url":   {map[string]string{"display_name": "A", "return_url": "ftp://x"}, "", nil, "return URL"},
			"name too long":    {map[string]string{"display_name": strings.Repeat("n", 101)}, "", nil, "name"},
			"script in accent": {map[string]string{"display_name": "A", "accent": "#fff;}</style><script>"}, "", nil, "accent"},
		} {
			r, body := post(c.fields, c.file, c.data)
			if r.StatusCode != 400 || !strings.Contains(body, c.want) {
				t.Errorf("%s: %d (want 400 mentioning %q)", name, r.StatusCode, c.want)
			}
		}

		operator := e.roleClient("operator")
		ot := csrfOf(t, e.pageOK(operator, page+"?tab=branding"))
		if r, _ := e.postMultipart(operator, page+"/branding", map[string]string{"display_name": "Z", "csrf": ot}, "", "", nil); r.StatusCode != 403 {
			t.Errorf("operator: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) { aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "app.update", 20); return })
		if len(aud) < 3 || aud[0].ActorID != "usr_admin" {
			t.Errorf("app.update audit: %+v", aud)
		}
	})
}

func TestAllowedProviderAccounts(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, pa := range []store.ProviderAccountInfo{
				{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "live", Connector: "razorpay", Name: "Razorpay A", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
				{ID: "pa_2", OrgID: store.DefaultOrgID, Mode: "test", Connector: "whop", Name: "Whop T", Status: "active", Currencies: []string{"USD"}, Purposes: []string{"one_time"}},
			} {
				if err := store.InsertProviderAccountInfo(ctx, tx, pa, e.clk.Now()); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		body := e.pageOK(admin, page+"?tab=providers")
		has(t, body, "Razorpay A", "Whop T", `value="pa_1"`, `value="pa_2"`)
		tok := csrfOf(t, body)
		if r, _ := e.post(admin, page+"/providers", url.Values{"account": {"pa_1"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("save: %d", r.StatusCode)
		}
		var ids []string
		_ = db.Read(ctx, func(q store.Querier) (err error) { ids, err = store.ListAppProviderAccountIDs(ctx, q, app.ID); return })
		if strings.Join(ids, ",") != "pa_1" {
			t.Fatalf("allowed %v", ids)
		}
		has(t, e.pageOK(admin, page+"?tab=providers"), `value="pa_1" checked`)
		if r, _ := e.post(admin, page+"/providers", url.Values{"account": {"pa_1", "pa_ghost"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("unknown account accepted: %d", r.StatusCode)
		}
		// clearing the selection is allowed
		if r, _ := e.post(admin, page+"/providers", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Errorf("clear: %d", r.StatusCode)
		}
		operator := e.roleClient("operator")
		ot := csrfOf(t, e.pageOK(operator, page+"?tab=providers"))
		if r, _ := e.post(operator, page+"/providers", url.Values{"account": {"pa_1"}, "csrf": {ot}}); r.StatusCode != 403 {
			t.Errorf("operator: %d", r.StatusCode)
		}
	})
}

func TestAppSettingsTab(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		body := e.pageOK(admin, page+"?tab=settings")
		tok := csrfOf(t, body)
		// every app-level key is listed; org-only keys are not
		has(t, body, "payment_ttl", "routing", "issue_invoices", "sac_code", "gst_rate_bp", "duplicate_payment_action")
		lacks(t, body, "mandate_afa_limit", "recon_time", "webhook_retry_schedule")
		row := regexpFind(t, `(?s)(<tr[^>]*id="setting-payment_ttl".*?</tr>)`, body)
		has(t, row, ">60<", "org") // inherited from the org
		sacRow := regexpFind(t, `(?s)(<tr[^>]*id="setting-sac_code".*?</tr>)`, body)
		has(t, sacRow, "998315", "default")

		// override
		if r, _ := e.post(admin, page+"/settings/payment_ttl", url.Values{"value": {"30"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("override: %d", r.StatusCode)
		}
		v, _ := e.set.Resolve(ctx, app.ID, "payment_ttl")
		if string(v.Raw) != "30" || v.Source != "app" {
			t.Fatalf("resolved %s %s", v.Raw, v.Source)
		}
		row = regexpFind(t, `(?s)(<tr[^>]*id="setting-payment_ttl".*?</tr>)`, e.pageOK(admin, page+"?tab=settings"))
		has(t, row, ">30<", "app", "Reset to org")

		// validation always goes through settings
		for name, c := range map[string]struct{ key, val string }{
			"out of range": {"payment_ttl", "5"},
			"not json":     {"payment_ttl", "abc"},
			"bad enum":     {"duplicate_payment_action", `"maybe"`},
			"org only key": {"mandate_afa_limit", "100"},
			"unknown key":  {"nope", "1"},
			"bad routing":  {"routing", `{"rules":[]}`},
		} {
			r, body := e.post(admin, page+"/settings/"+c.key, url.Values{"value": {c.val}, "csrf": {tok}})
			if r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
			if name == "out of range" && !strings.Contains(body, "payment_ttl") {
				t.Errorf("error does not name the setting: %s", body)
			}
		}
		v, _ = e.set.Resolve(ctx, app.ID, "payment_ttl")
		if string(v.Raw) != "30" {
			t.Fatalf("invalid write changed the value: %s", v.Raw)
		}

		// reset
		if r, _ := e.post(admin, page+"/settings/payment_ttl/reset", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("reset: %d", r.StatusCode)
		}
		v, _ = e.set.Resolve(ctx, app.ID, "payment_ttl")
		if string(v.Raw) != "60" || v.Source != "org" {
			t.Fatalf("after reset %s %s", v.Raw, v.Source)
		}

		// viewers read, only admins write
		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, page+"?tab=settings")
		has(t, vb, "payment_ttl")
		lacks(t, vb, `action="/dashboard/apps/`+app.ID+`/settings/payment_ttl"`)
		vt := csrfOf(t, vb)
		if r, _ := e.post(viewer, page+"/settings/payment_ttl", url.Values{"value": {"30"}, "csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("viewer write: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) { aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "setting.update", 10); return })
		if len(aud) != 1 || aud[0].ActorType != "user" || aud[0].ActorID != "usr_admin" {
			t.Errorf("setting.update audit: %+v", aud)
		}
	})
}
