package dashboard

import (
	"net/url"
	"testing"
	"time"
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
