package dashboard

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ist = func() *time.Location {
	l, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		panic(err)
	}
	return l
}()

func istDay(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, ist) }

func TestParseRevenueQuery(t *testing.T) {
	now := time.Date(2026, 10, 25, 15, 30, 0, 0, ist) // a Sunday
	first := time.Date(2024, 3, 9, 22, 0, 0, 0, time.UTC)
	tomorrow := istDay(2026, 10, 26)
	cases := []struct {
		name, query string
		from, to    time.Time
		rng, bucket string
	}{
		{"default", "", tomorrow.AddDate(0, 0, -30), tomorrow, "30d", "auto"},
		{"today", "range=today", istDay(2026, 10, 25), tomorrow, "today", "auto"},
		{"7d", "range=7d", tomorrow.AddDate(0, 0, -7), tomorrow, "7d", "auto"},
		{"3m", "range=3m", istDay(2026, 7, 26), tomorrow, "3m", "auto"},
		{"6m", "range=6m", istDay(2026, 4, 26), tomorrow, "6m", "auto"},
		{"12m", "range=12m", istDay(2025, 10, 26), tomorrow, "12m", "auto"},
		{"ytd", "range=ytd", istDay(2026, 1, 1), tomorrow, "ytd", "auto"},
		{"all from first entry", "range=all", istDay(2024, 3, 10), tomorrow, "all", "auto"}, // 22:00 UTC is 03:30 IST on the 10th
		{"custom inclusive end", "range=custom&from=2026-09-01&to=2026-09-30", istDay(2026, 9, 1), istDay(2026, 10, 1), "custom", "auto"},
		{"explicit bucket", "range=12m&bucket=week", istDay(2025, 10, 26), tomorrow, "12m", "week"},
	}
	for _, c := range cases {
		v, _ := url.ParseQuery(c.query)
		rq, err := parseRevenueQuery(v, now, ist, first, true)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !rq.From.Equal(c.from) || !rq.To.Equal(c.to) || rq.Range != c.rng || rq.Bucket != c.bucket {
			t.Errorf("%s: got %v..%v %s/%s want %v..%v %s/%s", c.name, rq.From, rq.To, rq.Range, rq.Bucket, c.from, c.to, c.rng, c.bucket)
		}
	}
	// all time with no entries is an empty window, not an error
	rq, err := parseRevenueQuery(url.Values{"range": {"all"}}, now, ist, time.Time{}, false)
	if err != nil || !rq.From.Equal(rq.To) {
		t.Errorf("all/no entries: %+v %v", rq, err)
	}
	for _, bad := range []string{"range=decade", "bucket=fortnight", "range=custom", "range=custom&from=2026-09-01", "range=custom&from=2026-09-30&to=2026-09-01",
		"range=custom&from=nope&to=2026-09-01", "range=custom&from=2026-09-01&to=2026-9-1x"} {
		v, _ := url.ParseQuery(bad)
		if _, err := parseRevenueQuery(v, now, ist, first, true); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// the app and currency pass through; a currency must look like an ISO code
	v, _ := url.ParseQuery("app=app_1&currency=USD")
	if rq, err := parseRevenueQuery(v, now, ist, first, true); err != nil || rq.App != "app_1" || rq.Currency != "USD" {
		t.Errorf("filters: %+v %v", rq, err)
	}
	if _, err := parseRevenueQuery(url.Values{"currency": {"usd1"}}, now, ist, first, true); err == nil {
		t.Error("bad currency accepted")
	}
}

func TestAutoBucket(t *testing.T) {
	to := istDay(2026, 10, 26)
	for _, c := range []struct {
		rng        string
		from       time.Time
		want       string
		toOverride time.Time
	}{
		{"today", istDay(2026, 10, 25), "hour", time.Time{}},
		{"7d", to.AddDate(0, 0, -7), "day", time.Time{}},
		{"30d", to.AddDate(0, 0, -30), "day", time.Time{}},
		{"3m", to.AddDate(0, -3, 0), "week", time.Time{}},
		{"6m", to.AddDate(0, -6, 0), "week", time.Time{}},
		{"12m", to.AddDate(-1, 0, 0), "month", time.Time{}},
		{"ytd", istDay(2026, 1, 1), "month", time.Time{}},
		{"all", to.AddDate(-2, 0, 0), "month", time.Time{}},
		{"all", to.AddDate(-3, 0, 0), "month", time.Time{}}, // exactly 36 months
		{"all", to.AddDate(-3, 0, -1), "quarter", time.Time{}},
		{"all", istDay(2020, 1, 1), "quarter", time.Time{}},
		{"custom", istDay(2026, 10, 25), "hour", time.Time{}},
		{"custom", istDay(2026, 10, 1), "day", time.Time{}},
		{"custom", istDay(2026, 4, 26), "week", time.Time{}},
		{"custom", istDay(2024, 4, 1), "month", time.Time{}},
	} {
		if got := autoBucket(c.rng, c.from, to); got != c.want {
			t.Errorf("%s from %s: %s want %s", c.rng, c.from.Format("2006-01-02"), got, c.want)
		}
	}
}

func TestBucketStartWeekMondayQuarter(t *testing.T) {
	at := time.Date(2026, 10, 25, 15, 30, 0, 0, ist) // Sunday
	for _, c := range []struct {
		bucket string
		in     time.Time
		want   time.Time
	}{
		{"hour", at, time.Date(2026, 10, 25, 15, 0, 0, 0, ist)},
		{"day", at, istDay(2026, 10, 25)},
		{"week", at, istDay(2026, 10, 19)},                   // Sunday belongs to the week of the Monday before
		{"week", istDay(2026, 10, 19), istDay(2026, 10, 19)}, // Monday starts its own week
		{"week", time.Date(2026, 1, 1, 1, 0, 0, 0, ist), istDay(2025, 12, 29)},
		{"month", at, istDay(2026, 10, 1)},
		{"quarter", at, istDay(2026, 10, 1)},
		{"quarter", istDay(2026, 9, 30), istDay(2026, 7, 1)},
		{"quarter", istDay(2026, 5, 2), istDay(2026, 4, 1)},
		{"quarter", istDay(2026, 2, 28), istDay(2026, 1, 1)},
	} {
		if got := bucketStart(c.in, c.bucket, ist); !got.Equal(c.want) {
			t.Errorf("%s %s: %v want %v", c.bucket, c.in.Format(time.RFC3339), got, c.want)
		}
	}
}

// The test clock is 2027-01-15 08:00 UTC and the dashboard runs in UTC.
func (e *env) d15(dayOfJan int) time.Time { return time.Date(2027, 1, dayOfJan, 12, 0, 0, 0, time.UTC) }

type ledRow struct {
	pay, kind, cur string
	amt            int64
	at             time.Time
}

var ledSeq int

func (e *env) seedLedger(rows ...ledRow) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		for _, l := range rows {
			ledSeq++
			err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: fmt.Sprintf("rev_%d", ledSeq), OrgID: store.DefaultOrgID, Mode: "live",
				Kind: l.kind, Amount: l.amt, Currency: l.cur, PaymentID: l.pay, OccurredAt: l.at})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seedRates(rates ...store.FXRate) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		for _, r := range rates {
			r.OrgID, r.Source = store.DefaultOrgID, "manual"
			if err := store.UpsertFXRate(ctx, tx, r, e.clk.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

type revPoint struct {
	start                                  string
	gross, refunds, prevGross, prevRefunds int64
}

var revRowRE = regexp.MustCompile(`<tr data-start="([^"]+)" data-gross="(-?\d+)" data-refunds="(-?\d+)" data-prev-gross="(-?\d+)" data-prev-refunds="(-?\d+)"`)

func revPoints(body string) []revPoint {
	var out []revPoint
	for _, m := range revRowRE.FindAllStringSubmatch(body, -1) {
		n := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
		out = append(out, revPoint{m[1], n(m[2]), n(m[3]), n(m[4]), n(m[5])})
	}
	return out
}

func revByDay(body string) map[string]revPoint {
	m := map[string]revPoint{}
	for _, p := range revPoints(body) {
		m[p.start[:10]] = p
	}
	return m
}

func tableAttr(t *testing.T, body, attr string) string {
	t.Helper()
	return regexpFind(t, `<table id="revenue-data"[^>]*\b`+attr+`="([^"]*)"`, body)
}

func TestRevenueKeepsNativeCurrenciesSeparate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(
			ledRow{"pay_1", "charge", "INR", 1000000, e.d15(15)},
			ledRow{"pay_1", "charge", "USD", 1000, e.d15(13)},
			ledRow{"pay_1", "charge", "USD", 500, e.d15(14)},
			ledRow{"pay_1", "charge", "USD", 100, e.d15(10)},
			ledRow{"pay_1", "refund", "USD", -200, e.d15(13)},
			ledRow{"pay_1", "charge", "EUR", 2000, e.d15(12)},
		)
		// A manual rate may exist for invoices, but Overview must not silently
		// use it to combine unlike currencies.
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-13", Rate: "80"})
		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if c, conv, est := tableAttr(t, body, "data-currency"), tableAttr(t, body, "data-converted"), tableAttr(t, body, "data-estimated"); c != "USD" || conv != "false" || est != "false" {
			t.Fatalf("currency %s converted %s estimated %s", c, conv, est)
		}
		if tableAttr(t, body, "data-exp") != "2" || tableAttr(t, body, "data-bucket") != "day" {
			t.Errorf("exp/bucket: %s %s", tableAttr(t, body, "data-exp"), tableAttr(t, body, "data-bucket"))
		}
		pts := revByDay(body)
		if len(pts) != 7 {
			t.Fatalf("want 7 daily points, got %d", len(pts))
		}
		for day, want := range map[string][2]int64{"2027-01-13": {1000, 200}, "2027-01-14": {500, 0}, "2027-01-10": {100, 0}, "2027-01-15": {0, 0}, "2027-01-12": {0, 0}} {
			if got := pts[day]; got.gross != want[0] || got.refunds != want[1] {
				t.Errorf("%s: %+v want %v", day, got, want)
			}
		}
		has(t, body, "Currency details", "20.00 EUR", "10000.00 INR", "16.00 USD", "2.00 USD", "14.00 USD", "Native totals")
		lacks(t, body, "stored daily rate", "estimated conversions")
		// Choosing INR shows only its exact native ledger values.
		body = e.pageOK(vw, "/dashboard/overview?range=7d&currency=INR")
		if tableAttr(t, body, "data-currency") != "INR" || revByDay(body)["2027-01-15"].gross != 1000000 {
			t.Errorf("INR view: %s", body)
		}
	})
}

func tableOnly(body string) string {
	return regexp.MustCompile(`(?s)<table id="revenue-data".*?</table>`).FindString(body)
}

func TestRevenueDefaultCurrencyMajority(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 10000, e.d15(14)}, ledRow{"pay_1", "charge", "USD", 100000, e.d15(14)})
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		v := vw
		body := e.pageOK(v, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-currency") != "USD" || tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 100000 {
			t.Errorf("mostly USD should open on USD unconverted: %s", tableOnly(body))
		}
		// Asking for INR still shows only native INR; the stored FX rate is not
		// used to inflate the total.
		body = e.pageOK(v, "/dashboard/overview?range=7d&currency=INR")
		if tableAttr(t, body, "data-currency") != "INR" || tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 10000 {
			t.Errorf("INR view: %s", tableOnly(body))
		}
	})
}

// reToken matches random attribute values (CSP nonces, CSRF tokens): one can
// spell "Inf" or "NaN" ("6Infw2y..."), so they are removed before the page is
// searched for a broken number.
var reToken = regexp.MustCompile(`="[A-Za-z0-9_+/=-]{16,}"`)

func withoutTokens(body string) string { return reToken.ReplaceAllString(body, `=""`) }

func TestRevenuePreviousPeriodChange(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		v := vw
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 15000, e.d15(12)}, ledRow{"pay_1", "refund", "INR", -1000, e.d15(12)},
			ledRow{"pay_1", "charge", "INR", 10000, e.d15(5)}) // previous 7 days: Jan 2..8
		body := e.pageOK(v, "/dashboard/overview?range=7d")
		has(t, html.UnescapeString(body), `data-metric="gross">+50.0%<`)
		pts := revByDay(body)
		if p := pts["2027-01-12"]; p.gross != 15000 || p.refunds != 1000 {
			t.Errorf("point %+v", p)
		}
		// previous period values line up by position: Jan 5 is the 4th previous day, Jan 12 the 4th current day
		if p := pts["2027-01-12"]; p.prevGross != 10000 {
			t.Errorf("prev gross %+v", p)
		}
		lacks(t, withoutTokens(body), `data-metric="refunds">+`, "NaN", "Inf")
		// 30d: previous 30 days hold only Jan 5's 10000? no: Dec 17..Jan 15 / Nov 17..Dec 16 holds nothing
		body = e.pageOK(v, "/dashboard/overview?range=30d")
		has(t, body, `data-metric="gross"></`)
	})
}

func TestRevenueEmpty(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		body := e.pageOK(vw, "/dashboard/overview?range=all")
		if n := len(revPoints(body)); n != 0 {
			t.Errorf("%d points for a new org", n)
		}
		lacks(t, withoutTokens(body), "NaN", "Inf", "undefined")
		has(t, body, `id="revenue-data"`)
	})
}

func TestOverviewRendersRevenueTable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 12345, e.d15(14)})
		body := e.pageOK(vw, "/dashboard/overview")
		has(t, body, `<nav class="range-presets"`, `href="/dashboard/overview?range=7d"`, `aria-current="page"`, `class="revenue-summary"`,
			`id="revenue-data"`, `data-gross="12345"`, "123.45 INR", "/dashboard/overview/revenue.csv?", "USD preferred", "Currency details")
		lacks(t, body, `id="period-today"`)
		if tableAttr(t, body, "data-bucket") != "day" {
			t.Error("30d is daily")
		}
		// app filter and unknown app
		has(t, e.pageOK(vw, "/dashboard/overview?app="+app.ID), `data-gross="12345"`)
		if r, _ := e.get(vw, "/dashboard/overview?app=app_nope"); r.StatusCode != 400 {
			t.Errorf("unknown app: %d", r.StatusCode)
		}
		if r, _ := e.get(vw, "/dashboard/overview?range=custom&from=2027-02-01&to=2027-01-01"); r.StatusCode != 400 {
			t.Errorf("bad custom: %d", r.StatusCode)
		}
	})
}

func TestRevenueCSV(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 12345, e.d15(14)}, ledRow{"pay_1", "refund", "INR", -345, e.d15(14)})
		r, body := e.get(vw, "/dashboard/overview/revenue.csv?range=7d")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), `filename="revenue-7d-INR.csv"`) {
			t.Fatalf("%d %v", r.StatusCode, r.Header)
		}
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if lines[0] != "start,label,currency,gross,refunds,net" || len(lines) != 8 {
			t.Fatalf("csv:\n%s", body)
		}
		has(t, body, "2027-01-14T00:00:00Z,14 Jan,INR,123.45,3.45,120.00")
		if r, _ := e.get(vw, "/dashboard/overview/revenue.csv?range=bogus"); r.StatusCode != 400 {
			t.Errorf("bad range: %d", r.StatusCode)
		}
	})
}

func TestParseRevenueQueryBounds(t *testing.T) {
	now := time.Date(2026, 10, 25, 15, 30, 0, 0, ist)
	for q, ok := range map[string]bool{
		"range=custom&from=2000-01-01&to=2026-10-25": true,
		"range=custom&from=2026-01-01&to=2027-10-25": true,
		"range=custom&from=1999-12-31&to=2026-10-25": false,
		"range=custom&from=1900-01-01&to=2100-12-31": false,
		"range=custom&from=2026-01-01&to=2027-10-27": false,
	} {
		v, _ := url.ParseQuery(q)
		_, err := parseRevenueQuery(v, now, ist, time.Time{}, false)
		if (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", q, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "out of bounds") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestRevenueCapsPoints(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		for bucket, want := range map[string]string{"day": "month", "week": "month", "hour": "month", "month": "month", "quarter": "quarter"} {
			body := e.pageOK(vw, "/dashboard/overview?range=custom&from=2000-01-01&to=2027-01-15&bucket="+bucket)
			if got := tableAttr(t, body, "data-bucket"); got != want {
				t.Errorf("%s: bucket %s want %s", bucket, got, want)
			}
			if n := len(revPoints(body)); n > 400 || n == 0 {
				t.Errorf("%s: %d points", bucket, n)
			}
		}
		// a small range keeps the bucket asked for
		body := e.pageOK(vw, "/dashboard/overview?range=custom&from=2027-01-01&to=2027-01-15&bucket=day")
		if tableAttr(t, body, "data-bucket") != "day" || len(revPoints(body)) != 15 {
			t.Error("small range was coarsened")
		}
		if r, _ := e.get(vw, "/dashboard/overview?range=custom&from=1900-01-01&to=2100-12-31"); r.StatusCode != 400 {
			t.Errorf("out of bounds: %d", r.StatusCode)
		}
		r, body := e.get(vw, "/dashboard/overview/revenue.csv?range=custom&from=2000-01-01&to=2027-01-15&bucket=day")
		if n := strings.Count(body, "\n"); r.StatusCode != 200 || n > 401 {
			t.Errorf("csv: %d, %d lines", r.StatusCode, n)
		}
	})
}

func (e *env) setDefaultCurrency(code string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "default_currency", `"`+code+`"`, e.clk.Now())
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestRevenueConvertsToDefaultCurrency(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(
			ledRow{"pay_1", "charge", "USD", 1000, e.d15(13)},
			ledRow{"pay_1", "refund", "USD", -200, e.d15(13)},
			ledRow{"pay_1", "charge", "INR", 800000, e.d15(14)},
			ledRow{"pay_1", "charge", "EUR", 2000, e.d15(12)},
			ledRow{"pay_1", "charge", "INR", 800000, e.d15(5)}, // previous period
		)
		e.seedRates(
			store.FXRate{Currency: "USD", Date: "2027-01-05", Rate: "80"},
			store.FXRate{Currency: "USD", Date: "2027-01-12", Rate: "80"},
			store.FXRate{Currency: "USD", Date: "2027-01-14", Rate: "80"},
			store.FXRate{Currency: "EUR", Date: "2027-01-12", Rate: "90"},
		)
		e.setDefaultCurrency("USD")

		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if c, conv := tableAttr(t, body, "data-currency"), tableAttr(t, body, "data-converted"); c != "USD" || conv != "true" {
			t.Fatalf("currency %s converted %s", c, conv)
		}
		pts := revByDay(body)
		for day, want := range map[string][2]int64{"2027-01-12": {2250, 0}, "2027-01-13": {1000, 200}, "2027-01-14": {10000, 0}} {
			if got := pts[day]; got.gross != want[0] || got.refunds != want[1] {
				t.Errorf("%s: %+v want %v", day, got, want)
			}
		}
		if p := pts["2027-01-12"]; p.prevGross != 10000 {
			t.Errorf("previous period is converted too: %+v", p)
		}
		// The headline is the sum in USD; the breakdown stays native.
		has(t, body, "132.50 USD", "2.00 USD", "130.50 USD", "converted to USD", "Currency details", "8000.00 INR", "20.00 EUR", "All, converted to USD")
		lacks(t, body, "USD preferred", "Each currency is reported separately", "Not included")

		// The CSV follows the page.
		_, csv := e.get(vw, "/dashboard/overview/revenue.csv?range=7d")
		has(t, csv, "USD,100.00,0.00,100.00")

		// Picking a currency is a native view, including the default one.
		body = e.pageOK(vw, "/dashboard/overview?range=7d&currency=INR")
		if tableAttr(t, body, "data-currency") != "INR" || tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 800000 {
			t.Errorf("INR view: %s", tableOnly(body))
		}
		body = e.pageOK(vw, "/dashboard/overview?range=7d&currency=USD")
		if tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 0 {
			t.Errorf("native USD view: %s", tableOnly(body))
		}
	})
}

func TestRevenueDefaultCurrencyINRNeedsOnlyTheSourceRate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 5000, e.d15(14)}, ledRow{"pay_1", "charge", "USD", 1000, e.d15(14)})
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		e.setDefaultCurrency("INR")
		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-currency") != "INR" || tableAttr(t, body, "data-estimated") != "false" || revByDay(body)["2027-01-14"].gross != 85000 {
			t.Errorf("INR default: %s", tableOnly(body))
		}
	})
}

func TestRevenueConversionFlagsEstimatesAndExclusions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(
			ledRow{"pay_1", "charge", "USD", 1000, e.d15(14)},
			ledRow{"pay_1", "charge", "INR", 800000, e.d15(14)}, // only an earlier USD rate
			ledRow{"pay_1", "charge", "EUR", 2000, e.d15(13)},   // no EUR rate at all
		)
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-10", Rate: "80"})
		e.setDefaultCurrency("USD")
		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-estimated") != "true" {
			t.Errorf("a non-same-day rate is an estimate: %s", tableOnly(body))
		}
		pts := revByDay(body)
		if pts["2027-01-14"].gross != 11000 || pts["2027-01-13"].gross != 0 {
			t.Errorf("points: %+v", pts)
		}
		has(t, body, "estimated conversions", "Not included: 20.00 EUR", `href="/dashboard/fx"`)

		// No rate for the default currency: only its own money can be shown.
		e.setDefaultCurrency("GBP")
		body = e.pageOK(vw, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-currency") != "GBP" || revByDay(body)["2027-01-14"].gross != 0 {
			t.Errorf("GBP default: %s", tableOnly(body))
		}
		has(t, body, "Not included:", "20.00 EUR", "8000.00 INR", "10.00 USD")
	})
}

func TestRevenueDefaultCurrencyOnlyItsOwnMoneyIsNotConverted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "USD", 1000, e.d15(14)})
		e.setDefaultCurrency("USD")
		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-converted") != "false" || tableAttr(t, body, "data-estimated") != "false" {
			t.Errorf("nothing to convert: %s", tableOnly(body))
		}
		lacks(t, body, "at the stored daily rate", "Not included")
	})
}

func TestDefaultCurrencySettingIsEditable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/settings")
		has(t, body, "Default currency", `action="/dashboard/settings/default_currency"`)
		tok := regexpFind(t, `name="csrf" value="([^"]+)"`, body)
		if r, _ := e.post(admin, "/dashboard/settings/default_currency", url.Values{"csrf": {tok}, "value": {"QQQ"}}); r.StatusCode != 400 {
			t.Errorf("unknown currency: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/settings/default_currency", url.Values{"csrf": {tok}, "value": {"EUR"}}); r.StatusCode != 303 {
			t.Errorf("save: %d", r.StatusCode)
		}
		has(t, e.pageOK(admin, "/dashboard/overview"), `data-currency="EUR"`, "All, converted to EUR")
	})
}

func (e *env) seedMarketRates(rates ...store.MarketRate) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		for _, r := range rates {
			if r.Source == "" {
				r.Source = "frankfurter"
			}
			if err := store.UpsertMarketRate(ctx, tx, r, e.clk.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestRevenueConvertsWithMarketRatesWhenNoneStored(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 800000, e.d15(14)})
		e.seedMarketRates(store.MarketRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		e.setDefaultCurrency("USD")

		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if got := revByDay(body)["2027-01-14"]; got.gross != 10000 {
			t.Errorf("converted gross %+v", got)
		}
		has(t, body, "100.00 USD", "indicative market rates")
		lacks(t, body, "Not included", "estimated conversions")
	})
}

func TestRevenueStoredRateBeatsMarketRateOnTheSameDay(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 800000, e.d15(14)})
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		e.seedMarketRates(store.MarketRate{Currency: "USD", Date: "2027-01-14", Rate: "40"})
		e.setDefaultCurrency("USD")

		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if got := revByDay(body)["2027-01-14"]; got.gross != 10000 {
			t.Errorf("converted gross %+v, want the stored rate", got)
		}
		lacks(t, body, "indicative market rates")
	})
}

// A newer market rate is closer to the day than an old stored one.
func TestRevenueMarketRateBeatsAnOlderStoredRate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 800000, e.d15(14)})
		e.seedRates(store.FXRate{Currency: "USD", Date: "2026-01-14", Rate: "40"})
		e.seedMarketRates(store.MarketRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		e.setDefaultCurrency("USD")

		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		if got := revByDay(body)["2027-01-14"]; got.gross != 10000 {
			t.Errorf("converted gross %+v, want the market rate", got)
		}
	})
}

func TestFXPageShowsLatestMarketRatesApartFromStoredOnes(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		lacks(t, e.pageOK(vw, "/dashboard/fx"), "Market rates")

		e.seedMarketRates(
			store.MarketRate{Currency: "USD", Date: "2027-01-13", Rate: "70.5"},
			store.MarketRate{Currency: "USD", Date: "2027-01-14", Rate: "80.5"},
		)
		body := e.pageOK(vw, "/dashboard/fx")
		has(t, body, "Market rates", "80.5", "2027-01-14", "frankfurter", "never used for invoices", "No rates yet.")
		lacks(t, body, "70.5")
	})
}

func TestFXProviderOffHidesMarketRates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		vw := e.roleClient("viewer")
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "USD", 1000, e.d15(14)})
		e.seedMarketRates(store.MarketRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		e.setDefaultCurrency("INR")
		err := e.db.Write(ctx, func(tx store.Tx) error {
			return store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "fx_provider", `"off"`, e.clk.Now())
		})
		if err != nil {
			t.Fatal(err)
		}

		body := e.pageOK(vw, "/dashboard/overview?range=7d")
		has(t, body, "Not included: 10.00 USD")
		lacks(t, body, "indicative market rates")
		lacks(t, e.pageOK(vw, "/dashboard/fx"), "Market rates")
	})
}

func TestFXProviderSettingIsEditable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/settings")
		has(t, body, "Market rate provider", `action="/dashboard/settings/fx_provider"`)
		tok := csrfOf(t, body)
		if r, _ := e.post(admin, "/dashboard/settings/fx_provider", url.Values{"csrf": {tok}, "value": {"google"}}); r.StatusCode != 400 {
			t.Errorf("bad value: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/settings/fx_provider", url.Values{"csrf": {tok}, "value": {"off"}}); r.StatusCode != 303 {
			t.Errorf("save: %d", r.StatusCode)
		}
	})
}
