package dashboard

import (
	"fmt"
	"net/url"
	"time"
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
