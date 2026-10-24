package dashboard

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
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

func TestRevenueINRConversionAndEstimates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(
			ledRow{"pay_1", "charge", "INR", 1000000, e.d15(15)},
			ledRow{"pay_1", "charge", "USD", 1000, e.d15(13)}, // same-day rate: 10.00 x 80
			ledRow{"pay_1", "charge", "USD", 500, e.d15(14)},  // no rate that day: earlier rate, estimated
			ledRow{"pay_1", "charge", "USD", 100, e.d15(10)},  // before the first rate: later rate, estimated
			ledRow{"pay_1", "refund", "USD", -200, e.d15(13)}, // refund converts the same way
			ledRow{"pay_1", "charge", "EUR", 2000, e.d15(12)}, // no rate at all
		)
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-13", Rate: "80"})
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/overview?range=7d")
		if c, conv, est := tableAttr(t, body, "data-currency"), tableAttr(t, body, "data-converted"), tableAttr(t, body, "data-estimated"); c != "INR" || conv != "true" || est != "true" {
			t.Fatalf("currency %s converted %s estimated %s", c, conv, est)
		}
		if tableAttr(t, body, "data-exp") != "2" || tableAttr(t, body, "data-bucket") != "day" {
			t.Errorf("exp/bucket: %s %s", tableAttr(t, body, "data-exp"), tableAttr(t, body, "data-bucket"))
		}
		pts := revByDay(body)
		if len(pts) != 7 {
			t.Fatalf("want 7 daily points, got %d", len(pts))
		}
		for day, want := range map[string][2]int64{"2027-01-13": {80000, 16000}, "2027-01-14": {40000, 0}, "2027-01-10": {8000, 0}, "2027-01-15": {1000000, 0}, "2027-01-12": {0, 0}} {
			if got := pts[day]; got.gross != want[0] || got.refunds != want[1] {
				t.Errorf("%s: %+v want %v", day, got, want)
			}
		}
		has(t, body, "20.00 EUR", "/dashboard/fx", "estimated")
		// a window with only exact rates is not estimated
		body = e.pageOK(e.roleClient("viewer"), "/dashboard/overview?range=custom&from=2027-01-13&to=2027-01-13")
		if tableAttr(t, body, "data-estimated") != "false" {
			t.Error("same-day conversion marked estimated")
		}
		// the picker view is unconverted
		body = e.pageOK(e.roleClient("viewer"), "/dashboard/overview?range=7d&currency=USD")
		if tableAttr(t, body, "data-currency") != "USD" || tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 500 {
			t.Errorf("USD view: %s", body)
		}
	})
}

func tableOnly(body string) string {
	return regexp.MustCompile(`(?s)<table id="revenue-data".*?</table>`).FindString(body)
}

func TestRevenueDefaultCurrencyMajority(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 10000, e.d15(14)}, ledRow{"pay_1", "charge", "USD", 100000, e.d15(14)})
		e.seedRates(store.FXRate{Currency: "USD", Date: "2027-01-14", Rate: "80"})
		v := e.roleClient("viewer")
		body := e.pageOK(v, "/dashboard/overview?range=7d")
		if tableAttr(t, body, "data-currency") != "USD" || tableAttr(t, body, "data-converted") != "false" || revByDay(body)["2027-01-14"].gross != 100000 {
			t.Errorf("mostly USD should open on USD unconverted: %s", tableOnly(body))
		}
		// asking for INR gives the converted view
		body = e.pageOK(v, "/dashboard/overview?range=7d&currency=INR")
		if tableAttr(t, body, "data-currency") != "INR" || tableAttr(t, body, "data-converted") != "true" || revByDay(body)["2027-01-14"].gross != 8010000 {
			t.Errorf("INR view: %s", tableOnly(body))
		}
	})
}

func TestRevenuePreviousPeriodChange(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		v := e.roleClient("viewer")
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 15000, e.d15(12)}, ledRow{"pay_1", "refund", "INR", -1000, e.d15(12)},
			ledRow{"pay_1", "charge", "INR", 10000, e.d15(5)}) // previous 7 days: Jan 2..8
		body := e.pageOK(v, "/dashboard/overview?range=7d")
		has(t, body, `data-metric="gross">+50.0%<`)
		pts := revByDay(body)
		if p := pts["2027-01-12"]; p.gross != 15000 || p.refunds != 1000 {
			t.Errorf("point %+v", p)
		}
		// previous period values line up by position: Jan 5 is the 4th previous day, Jan 12 the 4th current day
		if p := pts["2027-01-12"]; p.prevGross != 10000 {
			t.Errorf("prev gross %+v", p)
		}
		lacks(t, body, `data-metric="refunds">+`, "NaN", "Inf")
		// 30d: previous 30 days hold only Jan 5's 10000? no: Dec 17..Jan 15 / Nov 17..Dec 16 holds nothing
		body = e.pageOK(v, "/dashboard/overview?range=30d")
		has(t, body, `data-metric="gross"></`)
	})
}

func TestRevenueEmpty(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/overview?range=all")
		if n := len(revPoints(body)); n != 0 {
			t.Errorf("%d points for a new org", n)
		}
		lacks(t, body, "NaN", "Inf", "undefined")
		has(t, body, `id="revenue-data"`)
	})
}

func TestOverviewRendersRevenueTable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 12345, e.d15(14)})
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/overview")
		has(t, body, `<nav class="range-presets"`, `href="/dashboard/overview?range=7d"`, `aria-current="page"`, `class="revenue-summary"`,
			`id="revenue-data"`, `data-gross="12345"`, "123.45 INR", "/dashboard/overview/revenue.csv?", `id="period-today"`)
		if tableAttr(t, body, "data-bucket") != "day" {
			t.Error("30d is daily")
		}
		// app filter and unknown app
		has(t, e.pageOK(e.roleClient("viewer"), "/dashboard/overview?app="+app.ID), `data-gross="12345"`)
		if r, _ := e.get(e.roleClient("viewer"), "/dashboard/overview?app=app_nope"); r.StatusCode != 400 {
			t.Errorf("unknown app: %d", r.StatusCode)
		}
		if r, _ := e.get(e.roleClient("viewer"), "/dashboard/overview?range=custom&from=2027-02-01&to=2027-01-01"); r.StatusCode != 400 {
			t.Errorf("bad custom: %d", r.StatusCode)
		}
	})
}

func TestRevenueCSV(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		e.seedPayment("pay_1", app.ID, "live", "succeeded", "", 1, e.d15(10))
		e.seedLedger(ledRow{"pay_1", "charge", "INR", 12345, e.d15(14)}, ledRow{"pay_1", "refund", "INR", -345, e.d15(14)})
		r, body := e.get(e.roleClient("viewer"), "/dashboard/overview/revenue.csv?range=7d")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), `filename="revenue-7d-INR.csv"`) {
			t.Fatalf("%d %v", r.StatusCode, r.Header)
		}
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if lines[0] != "start,label,currency,gross,refunds,net" || len(lines) != 8 {
			t.Fatalf("csv:\n%s", body)
		}
		has(t, body, "2027-01-14T00:00:00Z,14 Jan,INR,123.45,3.45,120.00")
		if r, _ := e.get(e.roleClient("viewer"), "/dashboard/overview/revenue.csv?range=bogus"); r.StatusCode != 400 {
			t.Errorf("bad range: %d", r.StatusCode)
		}
	})
}
