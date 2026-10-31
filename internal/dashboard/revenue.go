package dashboard

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector/money"
)

// revenueQuery is the parsed, resolved query of the overview revenue chart.
type revenueQuery struct {
	Range    string // today|7d|30d|3m|6m|12m|ytd|all|custom; default 30d
	Bucket   string // auto|hour|day|week|month|quarter; default auto
	App      string
	Currency string    // "" = default rule (spec §6), else an ISO code
	From, To time.Time // resolved, local midnight boundaries; To is exclusive
}

var (
	revenueRanges = []struct{ ID, Label string }{{"today", "Today"}, {"7d", "7D"}, {"30d", "30D"}, {"3m", "3M"}, {"6m", "6M"},
		{"12m", "12M"}, {"ytd", "YTD"}, {"all", "All time"}}
)

// parseRevenueQuery resolves the URL query against now in loc. first is the
// time of the earliest ledger entry (hasFirst false when there is none); it
// anchors the all-time range.
func parseRevenueQuery(v url.Values, now time.Time, loc *time.Location, first time.Time, hasFirst bool) (revenueQuery, error) {
	rq := revenueQuery{Range: v.Get("range"), Bucket: v.Get("bucket"), App: v.Get("app"), Currency: v.Get("currency")}
	if rq.Range == "" {
		rq.Range = "30d"
	}
	if rq.Bucket == "" {
		rq.Bucket = "auto"
	}
	switch rq.Bucket {
	case "auto", "hour", "day", "week", "month", "quarter":
	default:
		return rq, fmt.Errorf("unknown bucket %q", rq.Bucket)
	}
	if rq.Currency != "" && !currencyRE.MatchString(rq.Currency) {
		return rq, fmt.Errorf("currency %q must be a three-letter code", rq.Currency)
	}
	n := now.In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	rq.To = today.AddDate(0, 0, 1)
	switch rq.Range {
	case "today":
		rq.From = today
	case "7d":
		rq.From = rq.To.AddDate(0, 0, -7)
	case "30d":
		rq.From = rq.To.AddDate(0, 0, -30)
	case "3m":
		rq.From = rq.To.AddDate(0, -3, 0)
	case "6m":
		rq.From = rq.To.AddDate(0, -6, 0)
	case "12m":
		rq.From = rq.To.AddDate(-1, 0, 0)
	case "ytd":
		rq.From = time.Date(n.Year(), 1, 1, 0, 0, 0, 0, loc)
	case "all":
		if !hasFirst {
			rq.From, rq.To = today, today
			break
		}
		f := first.In(loc)
		rq.From = time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	case "custom":
		from, err := time.ParseInLocation("2006-01-02", v.Get("from"), loc)
		if err != nil {
			return rq, fmt.Errorf("from must be YYYY-MM-DD")
		}
		to, err := time.ParseInLocation("2006-01-02", v.Get("to"), loc)
		if err != nil {
			return rq, fmt.Errorf("to must be YYYY-MM-DD")
		}
		if to.Before(from) {
			return rq, fmt.Errorf("from must not be after to")
		}
		// The ledger cannot reach before 2000, and nothing happens later than
		// next year; this keeps the window (and its series) bounded.
		if from.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, loc)) || to.After(today.AddDate(1, 0, 0)) {
			return rq, fmt.Errorf("range out of bounds: from 2000-01-01 to one year ahead")
		}
		rq.From, rq.To = from, to.AddDate(0, 0, 1)
	default:
		return rq, fmt.Errorf("unknown range %q", rq.Range)
	}
	return rq, nil
}

// autoBucket is the bucket used when none is asked for (spec §6).
func autoBucket(rangeID string, from, to time.Time) string {
	switch rangeID {
	case "today":
		return "hour"
	case "7d", "30d":
		return "day"
	case "3m", "6m":
		return "week"
	case "12m", "ytd":
		return "month"
	case "all":
		if from.AddDate(0, 36, 0).Before(to) {
			return "quarter"
		}
		return "month"
	}
	// custom: by length
	days := int(to.Sub(from).Hours() / 24)
	switch {
	case days <= 1:
		return "hour"
	case days <= 45:
		return "day"
	case days <= 200:
		return "week"
	}
	return autoBucket("all", from, to)
}

// bucketStart is the start of the bucket containing t in loc. Weeks start on
// Monday, quarters on Jan, Apr, Jul and Oct 1.
func bucketStart(t time.Time, bucket string, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	switch bucket {
	case "hour":
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	case "week":
		back := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc)
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case "quarter":
		return time.Date(y, m-(m-1)%3, 1, 0, 0, 0, 0, loc)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

type revenuePoint struct {
	Start                                  time.Time
	Label                                  string
	Gross, Refunds, PrevGross, PrevRefunds int64
	StartS, GrossS, RefundsS, NetS         string // RFC 3339 start and formatted amounts, for the template and CSV
}

type revenuePreset struct {
	ID, Label, Href string
	Current         bool
}

type revenueView struct {
	Query                                 revenueQuery
	Bucket                                string // the bucket in use (auto resolved)
	Currency                              string // currency of the amounts shown
	Exp                                   int    // its minor-unit exponent
	Converted                             bool   // retained in chart metadata; native views are always false
	Estimated                             bool   // retained in chart metadata; native views are always false
	Points                                []revenuePoint
	Gross, Refunds, Net                   int64
	GrossS, RefundsS, NetS                string
	GrossChange, NetChange, RefundsChange string
	Currencies                            []string
	OtherCurrencies                       []string
	Presets                               []revenuePreset
	Apps                                  []appOpt
	CSVHref                               string
	FromS, ToS                            string // custom range inputs, YYYY-MM-DD (To inclusive)
}

// revenue builds the revenue chart data of one org and mode.
func (d *Dashboard) revenue(ctx context.Context, q store.Querier, org, mode string, rq revenueQuery) (revenueView, error) {
	loc := d.o.Location
	v := revenueView{Query: rq, Bucket: rq.Bucket}
	if v.Bucket == "auto" {
		v.Bucket = autoBucket(rq.Range, rq.From, rq.To)
	}
	if v.Bucket == "hour" && rq.To.Sub(rq.From) > 7*24*time.Hour {
		v.Bucket = "day" // an hourly series over weeks is unreadable and large
	}
	v.Bucket = capBucket(rq.From, rq.To, v.Bucket, loc)
	hourly := v.Bucket == "hour"
	_, off := rq.From.In(loc).Zone()

	var rows, prevRows []store.SeriesRow
	prevFrom := rq.From
	if rq.To.After(rq.From) {
		days := int(math.Round(rq.To.Sub(rq.From).Hours() / 24))
		prevFrom = rq.From.AddDate(0, 0, -days)
		f := store.SeriesFilter{OrgID: org, Mode: mode, AppID: rq.App, From: rq.From, To: rq.To, Hourly: hourly, OffsetSeconds: off}
		var err error
		if rows, err = store.LedgerSeries(ctx, q, f); err != nil {
			return v, err
		}
		f.From, f.To = prevFrom, rq.From
		if prevRows, err = store.LedgerSeries(ctx, q, f); err != nil {
			return v, err
		}
	}

	// Revenue is kept in its native currency. Adding amounts denominated in
	// different currencies would make the headline totals depend on a stored
	// (and potentially stale) FX rate rather than the provider's settlement.
	present := map[string]bool{}
	for _, r := range rows {
		present[r.Currency] = true
	}
	shown := rq.Currency
	if shown == "" {
		activity := map[string]int64{}
		for _, r := range rows {
			activity[r.Currency] += abs64(r.Gross) + abs64(r.Refunds)
		}
		for currency, amount := range activity {
			if shown == "" || amount > activity[shown] || amount == activity[shown] && currency < shown {
				shown = currency
			}
		}
		if shown == "" {
			shown = "INR"
		}
	}
	v.Currency = shown
	v.Exp = 2
	if e, err := money.Exponent(shown); err == nil {
		v.Exp = e
	}
	cur, prev := native(rows, shown), native(prevRows, shown)

	// Points.
	if rq.To.After(rq.From) {
		idx := map[int64]int{}
		for s := bucketStart(rq.From, v.Bucket, loc); s.Before(rq.To); s = nextBucket(s, v.Bucket) {
			idx[s.Unix()] = len(v.Points)
			v.Points = append(v.Points, revenuePoint{Start: s, Label: bucketLabel(s, v.Bucket, rq)})
		}
		for _, r := range cur {
			if i, ok := idx[bucketStart(slotTime(r.Slot, hourly, off, loc), v.Bucket, loc).Unix()]; ok {
				v.Points[i].Gross += r.Gross
				v.Points[i].Refunds += r.Refunds
			}
		}
		pidx := map[int64]int{}
		n := 0
		for s := bucketStart(prevFrom, v.Bucket, loc); s.Before(rq.From); s = nextBucket(s, v.Bucket) {
			pidx[s.Unix()] = n
			n++
		}
		for _, r := range prev {
			if i, ok := pidx[bucketStart(slotTime(r.Slot, hourly, off, loc), v.Bucket, loc).Unix()]; ok && i < len(v.Points) {
				v.Points[i].PrevGross += r.Gross
				v.Points[i].PrevRefunds += r.Refunds
			}
		}
	}
	var pg, pr int64
	for _, r := range cur {
		v.Gross += r.Gross
		v.Refunds += r.Refunds
	}
	for _, r := range prev {
		pg += r.Gross
		pr += r.Refunds
	}
	v.Net = v.Gross - v.Refunds
	v.GrossChange, v.RefundsChange, v.NetChange = pctChange(v.Gross, pg), pctChange(v.Refunds, pr), pctChange(v.Net, pg-pr)
	v.GrossS, v.RefundsS, v.NetS = fmtSigned(v.Gross, shown), fmtSigned(v.Refunds, shown), fmtSigned(v.Net, shown)
	for i := range v.Points {
		p := &v.Points[i]
		p.StartS = p.Start.Format(time.RFC3339)
		p.GrossS, p.RefundsS, p.NetS = fmtSigned(p.Gross, shown), fmtSigned(p.Refunds, shown), fmtSigned(p.Gross-p.Refunds, shown)
	}

	// Pickers and links.
	present[shown] = shown != ""
	v.Currencies = sortedKeys(present)
	for _, currency := range v.Currencies {
		if currency != shown {
			v.OtherCurrencies = append(v.OtherCurrencies, currency)
		}
	}
	v.FromS, v.ToS = rq.From.Format("2006-01-02"), rq.To.AddDate(0, 0, -1).Format("2006-01-02")
	for _, p := range revenueRanges {
		qv := url.Values{"range": {p.ID}}
		if rq.App != "" {
			qv.Set("app", rq.App)
		}
		if rq.Currency != "" {
			qv.Set("currency", rq.Currency)
		}
		v.Presets = append(v.Presets, revenuePreset{ID: p.ID, Label: p.Label, Href: prefix + "/overview?" + qv.Encode(), Current: p.ID == rq.Range})
	}
	qv := url.Values{"range": {rq.Range}}
	if rq.Bucket != "auto" {
		qv.Set("bucket", rq.Bucket)
	}
	if rq.App != "" {
		qv.Set("app", rq.App)
	}
	if rq.Currency != "" {
		qv.Set("currency", rq.Currency)
	}
	if rq.Range == "custom" {
		qv.Set("from", v.FromS)
		qv.Set("to", v.ToS)
	}
	v.CSVHref = prefix + "/overview/revenue.csv?" + qv.Encode()
	return v, nil
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// native is the rows of one currency, unconverted.
func native(rows []store.SeriesRow, currency string) []convRow {
	var out []convRow
	for _, r := range rows {
		if r.Currency == currency {
			out = append(out, convRow{Slot: r.Slot, Gross: r.Gross, Refunds: r.Refunds})
		}
	}
	return out
}

// convRow is a normalized series row in one native currency.
type convRow struct {
	Slot           int64
	Gross, Refunds int64
}

// slotTime is the local start of a LedgerSeries slot.
func slotTime(slot int64, hourly bool, off int, loc *time.Location) time.Time {
	if hourly {
		return time.UnixMilli(slot*3_600_000 - int64(off)*1000).In(loc)
	}
	u := time.UnixMilli(slot * 86_400_000).UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, loc)
}

const maxPoints = 400

// capBucket steps the bucket up (hour, day, week, month, quarter) until the
// window has at most maxPoints buckets. Quarter is never capped.
func capBucket(from, to time.Time, bucket string, loc *time.Location) string {
	order := []string{"hour", "day", "week", "month", "quarter"}
	i := slices.Index(order, bucket)
	for ; i >= 0 && i < len(order)-1; i++ {
		n := 0
		for s := bucketStart(from, order[i], loc); s.Before(to) && n <= maxPoints; s = nextBucket(s, order[i]) {
			n++
		}
		if n <= maxPoints {
			return order[i]
		}
	}
	if i >= 0 {
		return order[i]
	}
	return bucket
}

func nextBucket(t time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		return t.Add(time.Hour)
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	case "quarter":
		return t.AddDate(0, 3, 0)
	}
	return t.AddDate(0, 0, 1)
}

func bucketLabel(t time.Time, bucket string, rq revenueQuery) string {
	switch bucket {
	case "hour":
		return t.Format("15:04")
	case "month":
		return t.Format("Jan 2006")
	case "quarter":
		return fmt.Sprintf("Q%d %d", (int(t.Month())-1)/3+1, t.Year())
	}
	if rq.From.Year() != rq.To.AddDate(0, 0, -1).Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// pctChange is "+12.4%" or "−3.0%"; "" when the previous value is zero.
func pctChange(cur, prev int64) string {
	if prev == 0 {
		return ""
	}
	p := math.Round(float64(cur-prev)/math.Abs(float64(prev))*1000) / 10
	if p < 0 {
		return fmt.Sprintf("−%.1f%%", -p)
	}
	return fmt.Sprintf("+%.1f%%", p)
}

// fmtSigned is fmtMoney for values that may be negative (a net).
func fmtSigned(minor int64, currency string) string {
	if minor < 0 {
		return "-" + fmtMoney(-minor, currency)
	}
	return fmtMoney(minor, currency)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// revenueData parses the request query and builds the view. bad is a message
// for a 400 response.
func (d *Dashboard) revenueData(ctx context.Context, r *http.Request, org, mode string) (v revenueView, bad string, err error) {
	err = d.o.DB.Read(ctx, func(q store.Querier) error {
		first, hasFirst, err := store.FirstLedgerAt(ctx, q, org, mode)
		if err != nil {
			return err
		}
		rq, perr := parseRevenueQuery(r.URL.Query(), d.o.Now(), d.o.Location, first, hasFirst)
		if perr != nil {
			bad = perr.Error()
			return nil
		}
		apps, err := store.ListApps(ctx, q, org)
		if err != nil {
			return err
		}
		known := rq.App == ""
		var opts []appOpt
		for _, a := range apps {
			opts = append(opts, appOpt{ID: a.ID, Name: a.Name})
			known = known || a.ID == rq.App
		}
		if !known {
			bad = "unknown app"
			return nil
		}
		if v, err = d.revenue(ctx, q, org, mode, rq); err != nil {
			return err
		}
		v.Apps = opts
		return nil
	})
	return v, bad, err
}

func (d *Dashboard) revenueCSV(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	v, bad, err := d.revenueData(ctx, r, org, d.mode(r, s))
	if err != nil {
		d.fail(w, err)
		return
	}
	if bad != "" {
		badRequest(w, bad)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="revenue-%s-%s.csv"`, v.Query.Range, v.Currency))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"start", "label", "currency", "gross", "refunds", "net"})
	for _, p := range v.Points {
		_ = cw.Write([]string{p.StartS, p.Label, v.Currency, majorUnits(p.Gross, v.Currency), majorUnits(p.Refunds, v.Currency), majorUnits(p.Gross-p.Refunds, v.Currency)})
	}
	cw.Flush()
}

// majorUnits renders minor units as a plain decimal such as "-120.00".
func majorUnits(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	out, err := money.FormatDecimal(minor, currency)
	if err != nil {
		return fmt.Sprint(minor)
	}
	return sign + out
}
