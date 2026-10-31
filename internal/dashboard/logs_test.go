package dashboard

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func logsOf(t *testing.T, db *store.DB) *store.DB {
	t.Helper()
	l, err := db.OpenLogs(ctx, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if l != db {
		t.Cleanup(func() { l.Close() })
	}
	return l
}

var logT0 = time.Unix(1_800_000_000, 0).UTC().Add(-time.Hour)

func seedLogs(t *testing.T, l *store.DB) {
	t.Helper()
	rows := []store.LogRow{
		{ID: "log_a", At: logT0, Level: "error", Kind: "provider", Component: "connector:payu", Msg: "provider call failed: POST secure.payu.in/x -> 504",
			RequestID: "req_aaa", PaymentID: "pay_1", AttemptID: "att_1", ProviderAccountID: "pa_1", Connector: "payu", IDs: `{"provider_request_id":"preq_9"}`,
			Attrs: `{"failure":"status","attempt":1}`, Request: `{"method":"POST","url":"https://secure.payu.in/x?hash=[redacted]","body":"{\"txnid\":\"tx1\"}"}`,
			Response: `{"status":504,"body":"gateway timeout"}`, Status: 504, DurationMS: 30000},
		{ID: "log_b", At: logT0.Add(time.Minute), Level: "warn", Kind: "http", Component: "http", Msg: "POST /v1/ -> 401", RequestID: "req_bbb", Status: 401},
		{ID: "log_c", At: logT0.Add(2 * time.Minute), Level: "error", Kind: "app", Component: "internal/jobs", Msg: "database is locked", RequestID: "req_ccc"},
		{ID: "log_d", At: logT0.Add(3 * time.Minute), Level: "warn", Kind: "job", Component: "internal/jobs", Msg: "job failed, will retry", JobID: "job_1",
			IDs: `{"job_kind":"subscription.charge"}`, RefundID: "rfd_1", SubscriptionID: "sub_1"},
	}
	if err := l.Write(ctx, func(tx store.Tx) error { return store.InsertLogs(ctx, tx, rows) }); err != nil {
		t.Fatal(err)
	}
}

func TestLogsPageIsForLogsReadOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		seedLogs(t, l)
		for _, role := range []string{"viewer", "operator"} {
			c := e.roleClient(role)
			for _, p := range []string{"/dashboard/logs", "/dashboard/logs/log_a", "/dashboard/logs.jsonl"} {
				if r, _ := e.get(c, p); r.StatusCode != 403 {
					t.Errorf("%s %s: %d", role, p, r.StatusCode)
				}
			}
			if _, body := e.get(c, "/dashboard/payments"); strings.Contains(body, `href="/dashboard/logs"`) {
				t.Errorf("%s sees the Logs link in the navigation", role)
			}
		}
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/logs")
		has(t, body, "Logs", `href="/dashboard/logs"`, "log_d")
		for _, p := range []string{"/dashboard/logs/log_a", "/dashboard/logs.jsonl"} {
			if r, _ := e.get(admin, p); r.StatusCode != 200 {
				t.Errorf("admin %s: %d", p, r.StatusCode)
			}
		}
		// Anonymous: sent to the login page, nothing shown.
		if r, _ := e.get(e.client(), "/dashboard/logs"); r.StatusCode != 303 {
			t.Errorf("anonymous: %d", r.StatusCode)
		}
	})
}

func TestACustomRoleCanBeGrantedLogsRead(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		seedLogs(t, l)
		_, secret := e.seedUser("root@example.com", "admin", true)
		e.login(e.c, "root@example.com", secret)
		tok := e.authedCSRF(e.c)
		if r, _ := e.post(e.c, "/dashboard/roles", url.Values{"name": {"support"}, "action": {"payment.read", "logs.read"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("create role: %d", r.StatusCode)
		}
		uid, usecret := e.seedUser("sup@example.com", "viewer", true)
		if r, _ := e.post(e.c, "/dashboard/users/"+uid+"/role", url.Values{"role": {"support"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("assign: %d", r.StatusCode)
		}
		c := e.client()
		e.login(c, "sup@example.com", usecret)
		if r, _ := e.get(c, "/dashboard/logs"); r.StatusCode != 200 {
			t.Errorf("custom role with logs.read: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/audit"); r.StatusCode != 403 {
			t.Errorf("logs.read must not open the audit log: %d", r.StatusCode)
		}
	})
}

var logIDRE = regexp.MustCompile(`/dashboard/logs/(log_[a-z0-9]+)`)

func logIDs(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range logIDRE.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func TestLogsPageFiltersAndPages(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		seedLogs(t, l)
		admin := e.roleClient("admin")
		ids := func(q string) []string { return logIDs(e.pageOK(admin, "/dashboard/logs"+q)) }
		eq := func(label string, got, want []string) {
			t.Helper()
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: got %v want %v", label, got, want)
			}
		}
		eq("all (newest first, page size 2)", ids(""), []string{"log_d", "log_c"})
		eq("level", ids("?level=error"), []string{"log_c", "log_a"})
		eq("kind", ids("?kind=http"), []string{"log_b"})
		eq("connector", ids("?connector=payu"), []string{"log_a"})
		eq("account", ids("?account=pa_1"), []string{"log_a"})
		eq("request id", ids("?request_id=req_bbb"), []string{"log_b"})
		eq("payment id", ids("?payment_id=pay_1"), []string{"log_a"})
		eq("any id: attempt", ids("?id=att_1"), []string{"log_a"})
		eq("any id: refund", ids("?id=rfd_1"), []string{"log_d"})
		eq("any id: subscription", ids("?id=sub_1"), []string{"log_d"})
		eq("any id: json id", ids("?id=preq_9"), []string{"log_a"})
		eq("text", ids("?q=locked"), []string{"log_c"})
		eq("text in a body", ids("?q=gateway+timeout"), []string{"log_a"})
		eq("none", ids("?q=nothing-like-this"), nil)
		day := logT0.Format("2006-01-02")
		eq("day", ids("?from="+day+"&to="+day+"&level=warn"), []string{"log_d", "log_b"})
		if r, _ := e.get(admin, "/dashboard/logs?level=loud"); r.StatusCode != 400 {
			t.Errorf("bad level: %d", r.StatusCode)
		}
		if r, _ := e.get(admin, "/dashboard/logs?from=yesterday"); r.StatusCode != 400 {
			t.Errorf("bad date: %d", r.StatusCode)
		}
		// Paging: page size is 2, so the second page has the older two.
		body := e.pageOK(admin, "/dashboard/logs")
		m := regexp.MustCompile(`href="(/dashboard/logs\?[^"]*before=[^"]+)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no link to older rows:\n%s", body)
		}
		eq("second page", logIDs(e.pageOK(admin, strings.ReplaceAll(m[1], "&amp;", "&"))), []string{"log_b", "log_a"})
		// The filters survive the paging link.
		body = e.pageOK(admin, "/dashboard/logs?level=warn&kind=job")
		if strings.Contains(body, "before=") {
			t.Error("a short result has no next page")
		}
	})
}

func TestLogDetailShowsIDsAttrsAndBodies(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		seedLogs(t, l)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/logs/log_a")
		has(t, body, "provider call failed", "req_aaa", "pay_1", "att_1", "pa_1", "payu", "preq_9", "504", "gateway timeout", "txnid", "failure")
		// A link to the neighbours of this row: the same request and payment.
		has(t, body, "/dashboard/logs?request_id=req_aaa", "/dashboard/logs?payment_id=pay_1")
		if r, _ := e.get(admin, "/dashboard/logs/log_nope"); r.StatusCode != 404 {
			t.Errorf("unknown row: %d", r.StatusCode)
		}
	})
}

func TestLogsExportIsJSONLinesOfTheFilteredRows(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		seedLogs(t, l)
		admin := e.roleClient("admin")
		resp, err := admin.Get(e.srv.URL + "/dashboard/logs.jsonl?level=error")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "ndjson") ||
			!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(resp.Header.Get("Content-Disposition"), ".jsonl") {
			t.Fatalf("%d %v", resp.StatusCode, resp.Header)
		}
		var got []map[string]any
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("not JSON: %s", sc.Text())
			}
			got = append(got, m)
		}
		if len(got) != 2 || got[0]["id"] != "log_c" || got[1]["id"] != "log_a" {
			t.Fatalf("rows = %v", got)
		}
		a := got[1]
		if a["level"] != "error" || a["kind"] != "provider" || a["request_id"] != "req_aaa" || a["payment_id"] != "pay_1" || a["connector"] != "payu" ||
			a["status"] != float64(504) || a["time"] == "" {
			t.Errorf("fields: %v", a)
		}
		if req, _ := a["request"].(map[string]any); req == nil || req["method"] != "POST" {
			t.Errorf("request body missing: %v", a["request"])
		}
		if resp, _ := a["response"].(map[string]any); resp == nil || resp["status"] != float64(504) {
			t.Errorf("response missing: %v", a["response"])
		}
		if ids, _ := a["ids"].(map[string]any); ids["provider_request_id"] != "preq_9" {
			t.Errorf("ids: %v", a["ids"])
		}
	})
}

// A secret that became known after a row was stored is still hidden when the
// row is shown or exported.
func TestLogsAreRedactedAgainOnTheWayOut(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) {
			o.LogDB = l
			o.LogRedact = func(s string) string { return strings.ReplaceAll(s, "LATE-SECRET-VALUE", "[redacted]") }
		})
		if err := l.Write(ctx, func(tx store.Tx) error {
			return store.InsertLogs(ctx, tx, []store.LogRow{{ID: "log_z", At: logT0, Level: "error", Kind: "app", Component: "t",
				Msg: "failed with LATE-SECRET-VALUE", Attrs: `{"k":"LATE-SECRET-VALUE"}`, Request: `{"body":"LATE-SECRET-VALUE"}`, Response: `LATE-SECRET-VALUE`}})
		}); err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		for _, p := range []string{"/dashboard/logs", "/dashboard/logs/log_z", "/dashboard/logs.jsonl"} {
			_, body := e.get(admin, p)
			lacks(t, body, "LATE-SECRET-VALUE")
			has(t, body, "[redacted]")
		}
	})
}

func TestLogLinksOnPaymentPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		a := e.seedApp("alpha")
		e.seedPayment("pay_a1", a.ID, "test", "succeeded", "ORD-1", 1000, logT0)
		e.seedAttempt(store.Attempt{ID: "att_a1", Mode: "test", PaymentID: "pay_a1", ProviderAccountID: "pa_x", ProviderRef: "ref1", Status: "captured", Method: "upi"})
		admin := e.roleClient("admin")
		e.setMode(admin, "test")
		body := e.pageOK(admin, "/dashboard/payments/pay_a1")
		has(t, body, "/dashboard/logs?payment_id=pay_a1", "/dashboard/logs?id=att_a1")
		viewer := e.roleClient("viewer")
		e.setMode(viewer, "test")
		lacks(t, e.pageOK(viewer, "/dashboard/payments/pay_a1"), "/dashboard/logs")
	})
}

var _ = http.StatusOK

// Logs are scoped to the dashboard's organisation: a row of another org is
// absent from the list and the export and its detail page is a 404; rows with
// no org (process-wide errors) stay visible.
func TestLogsAreScopedToTheOrganisation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		e := newEnv(t, db, func(o *Options) { o.LogDB = l })
		if err := l.Write(ctx, func(tx store.Tx) error {
			return store.InsertLogs(ctx, tx, []store.LogRow{
				{ID: "log_other", At: logT0, Level: "error", Kind: "app", Component: "t", Msg: "OTHER-ORG-ROW", OrgID: "org_someone_else"},
				{ID: "log_sys", At: logT0, Level: "error", Kind: "app", Component: "t", Msg: "PROCESS-WIDE-ROW"},
			})
		}); err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		for _, p := range []string{"/dashboard/logs", "/dashboard/logs.jsonl", "/dashboard/logs?q=OTHER-ORG-ROW", "/dashboard/logs?id=org_someone_else"} {
			_, body := e.get(admin, p)
			lacks(t, body, "log_other") // the search box echoes the query text, so look for the row id
		}
		_, body := e.get(admin, "/dashboard/logs")
		lacks(t, body, "OTHER-ORG-ROW")
		has(t, body, "PROCESS-WIDE-ROW")
		if r, _ := e.get(admin, "/dashboard/logs/log_other"); r.StatusCode != 404 {
			t.Errorf("another org's log row: %d, want 404", r.StatusCode)
		}
		if r, _ := e.get(admin, "/dashboard/logs/log_sys"); r.StatusCode != 200 {
			t.Errorf("process-wide log row: %d, want 200", r.StatusCode)
		}
	})
}
