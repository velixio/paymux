package dashboard

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/store"
)

type analyticsQuery struct {
	revenueQuery
	Country string
}

type analyticsPoint struct {
	Label, StartS                              string
	Created, Paid, Failed, Waiting, Incomplete int64
	CreatedWidth, PaidWidth, FailedWidth       int
}

type analyticsCountryRow struct {
	Code, Label, PaidAmount, RefundedAmount, SuccessRate, RefundRate, Href, PaymentsHref string
	Created, Paid, Failed, Waiting, Incomplete                                           int64
	Width                                                                                int
}

type analyticsDimensionRow struct {
	Key, Label, SuccessRate string
	Attempts, Paid, Failed  int64
}

type analyticsFailureRow struct {
	Code, Message string
	Count         int64
}

type analyticsView struct {
	Query analyticsQuery

	FromS, ToS, Bucket, Currency string
	Apps                         []appOpt
	Currencies, Countries        []string
	Presets                      []revenuePreset
	Points                       []analyticsPoint
	CountryRows                  []analyticsCountryRow
	Providers, Methods           []analyticsDimensionRow
	Failures                     []analyticsFailureRow
	CountriesCSV                 string
	Href                         string

	Created, Paid, Failed, Waiting, Incomplete int64
	CreatedChange, PaidChange, FailedChange    string
	SuccessRate, PreviousSuccessRate           string
	PaidAmount, RefundedAmount                 string
}

func (d *Dashboard) routesAnalytics() {
	d.mux.HandleFunc("GET "+prefix+"/analytics", d.authed(authz.PaymentRead, d.analyticsPage))
	d.mux.HandleFunc("GET "+prefix+"/analytics/countries.csv", d.authed(authz.PaymentRead, d.analyticsCountriesCSV))
}

func analyticsRate(n, total int64) string {
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(total))
}

// analyticsPercent scales a chart value while keeping nonzero values visible.
func analyticsPercent(n, max int64) int {
	if n <= 0 || max <= 0 {
		return 0
	}
	p := int(n * 100 / max)
	if p < 2 {
		return 2
	}
	return p
}

// analyticsCSVSafe prevents spreadsheet programs from interpreting exported
// untrusted text as a formula.
func analyticsCSVSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// sumAnalyticsPoints computes payment-level totals across time buckets.
func sumAnalyticsPoints(points []store.PaymentOutcomePoint) (created, paid, failed, waiting, incomplete int64) {
	for _, p := range points {
		created += p.Created
		paid += p.Paid
		failed += p.Failed
		waiting += p.Waiting
		incomplete += p.Incomplete
	}
	return
}

// analyticsFilter converts a validated dashboard query into a store filter.
func analyticsFilter(org, mode string, aq analyticsQuery, bucket string, loc *time.Location) store.AnalyticsFilter {
	_, off := aq.From.In(loc).Zone()
	return store.AnalyticsFilter{OrgID: org, Mode: mode, AppID: aq.App, Currency: aq.Currency, Country: aq.Country,
		From: aq.From, To: aq.To, Hourly: bucket == "hour", OffsetSeconds: off}
}

// analyticsData validates filters and assembles all payment- and attempt-level
// datasets used by the Analytics page and the compact Overview panels.
func (d *Dashboard) analyticsData(ctx context.Context, r *http.Request, org, mode string) (v analyticsView, bad string, err error) {
	err = d.o.DB.Read(ctx, func(q store.Querier) error {
		first, hasFirst, err := store.FirstPaymentAt(ctx, q, org, mode)
		if err != nil {
			return err
		}
		rq, perr := parseRevenueQuery(r.URL.Query(), d.o.Now(), d.o.Location, first, hasFirst)
		if perr != nil {
			bad = perr.Error()
			return nil
		}
		country := strings.TrimSpace(r.URL.Query().Get("country"))
		if !strings.EqualFold(country, "Unknown") {
			country = strings.ToUpper(country)
		} else {
			country = "Unknown"
		}
		aq := analyticsQuery{revenueQuery: rq, Country: country}
		if aq.Country != "" && aq.Country != "Unknown" && (len(aq.Country) != 2 || aq.Country[0] < 'A' || aq.Country[0] > 'Z' || aq.Country[1] < 'A' || aq.Country[1] > 'Z') {
			bad = "country must be a two-letter code or Unknown"
			return nil
		}
		apps, err := store.ListApps(ctx, q, org)
		if err != nil {
			return err
		}
		knownApp := aq.App == ""
		for _, app := range apps {
			v.Apps = append(v.Apps, appOpt{ID: app.ID, Name: app.Name})
			knownApp = knownApp || app.ID == aq.App
		}
		if !knownApp {
			bad = "unknown app"
			return nil
		}

		picker := store.AnalyticsFilter{OrgID: org, Mode: mode, AppID: aq.App, Country: aq.Country, From: aq.From, To: aq.To}
		if v.Currencies, err = store.AnalyticsCurrencies(ctx, q, picker); err != nil {
			return err
		}
		if aq.Currency == "" {
			for _, currency := range v.Currencies {
				if currency == "USD" {
					aq.Currency = currency
				}
			}
			if aq.Currency == "" && len(v.Currencies) > 0 {
				aq.Currency = v.Currencies[0]
			}
			if aq.Currency == "" {
				aq.Currency = "USD"
			}
		}
		knownCurrency := false
		for _, currency := range v.Currencies {
			knownCurrency = knownCurrency || currency == aq.Currency
		}
		if len(v.Currencies) > 0 && !knownCurrency {
			bad = "currency has no payments in this range"
			return nil
		}
		if len(v.Currencies) == 0 {
			v.Currencies = []string{aq.Currency}
		}

		bucket := aq.Bucket
		if bucket == "auto" {
			bucket = autoBucket(aq.Range, aq.From, aq.To)
		}
		if bucket == "hour" && aq.To.Sub(aq.From) > 7*24*time.Hour {
			bucket = "day"
		}
		bucket = capBucket(aq.From, aq.To, bucket, d.o.Location)
		filter := analyticsFilter(org, mode, aq, bucket, d.o.Location)
		points, err := store.PaymentOutcomeSeries(ctx, q, filter)
		if err != nil {
			return err
		}
		prevFilter := filter
		prevFilter.To = filter.From
		prevFilter.From = filter.From.Add(-filter.To.Sub(filter.From))
		previous, err := store.PaymentOutcomeSeries(ctx, q, prevFilter)
		if err != nil {
			return err
		}
		countries, err := store.CountryOutcomes(ctx, q, filter, 50)
		if err != nil {
			return err
		}
		countryPicker := filter
		countryPicker.Country = ""
		if v.Countries, err = store.AnalyticsCountries(ctx, q, countryPicker); err != nil {
			return err
		}
		providers, err := store.ProviderOutcomes(ctx, q, filter, 8)
		if err != nil {
			return err
		}
		methods, err := store.MethodOutcomes(ctx, q, filter, 8)
		if err != nil {
			return err
		}
		failures, err := store.FailureOutcomes(ctx, q, filter, 8)
		if err != nil {
			return err
		}

		v.Query, v.Bucket, v.Currency = aq, bucket, aq.Currency
		v.FromS, v.ToS = aq.From.Format("2006-01-02"), aq.To.AddDate(0, 0, -1).Format("2006-01-02")
		v.Created, v.Paid, v.Failed, v.Waiting, v.Incomplete = sumAnalyticsPoints(points)
		pc, pp, pf, _, _ := sumAnalyticsPoints(previous)
		v.CreatedChange, v.PaidChange, v.FailedChange = pctChange(v.Created, pc), pctChange(v.Paid, pp), pctChange(v.Failed, pf)
		v.SuccessRate, v.PreviousSuccessRate = analyticsRate(v.Paid, v.Created), analyticsRate(pp, pc)
		pointIndex := map[int64]int{}
		for start := bucketStart(aq.From, bucket, d.o.Location); start.Before(aq.To); start = nextBucket(start, bucket) {
			pointIndex[start.Unix()] = len(v.Points)
			v.Points = append(v.Points, analyticsPoint{Label: bucketLabel(start, bucket, aq.revenueQuery), StartS: start.Format(time.RFC3339)})
		}
		for _, point := range points {
			start := bucketStart(slotTime(point.Slot, bucket == "hour", filter.OffsetSeconds, d.o.Location), bucket, d.o.Location)
			if i, ok := pointIndex[start.Unix()]; ok {
				p := &v.Points[i]
				p.Created += point.Created
				p.Paid += point.Paid
				p.Failed += point.Failed
				p.Waiting += point.Waiting
				p.Incomplete += point.Incomplete
			}
		}
		maxCreated := int64(0)
		for _, point := range v.Points {
			if point.Created > maxCreated {
				maxCreated = point.Created
			}
		}
		for i := range v.Points {
			p := &v.Points[i]
			p.CreatedWidth = analyticsPercent(p.Created, maxCreated)
			p.PaidWidth = analyticsPercent(p.Paid, maxCreated)
			p.FailedWidth = analyticsPercent(p.Failed, maxCreated)
		}
		maxPaid := int64(0)
		for _, country := range countries {
			if country.Paid > maxPaid {
				maxPaid = country.Paid
			}
		}
		var paidAmount, refundedAmount int64
		for _, country := range countries {
			paidAmount += country.PaidAmount
			refundedAmount += country.RefundedAmount
			qv := r.URL.Query()
			qv.Set("country", country.Country)
			paymentsQuery := url.Values{"from": {v.FromS}, "to": {v.ToS}, "currency": {aq.Currency}, "country": {country.Country}}
			if aq.App != "" {
				paymentsQuery.Set("app", aq.App)
			}
			v.CountryRows = append(v.CountryRows, analyticsCountryRow{Code: country.Country, Label: country.Country,
				Created: country.Created, Paid: country.Paid, Failed: country.Failed, Waiting: country.Waiting, Incomplete: country.Incomplete,
				PaidAmount: fmtMoney(country.PaidAmount, aq.Currency), RefundedAmount: fmtMoney(country.RefundedAmount, aq.Currency),
				SuccessRate: analyticsRate(country.Paid, country.Created), RefundRate: analyticsRate(country.RefundedAmount, country.PaidAmount),
				Width: analyticsPercent(country.Paid, maxPaid), Href: prefix + "/analytics?" + qv.Encode(),
				PaymentsHref: prefix + "/payments?" + paymentsQuery.Encode()})
		}
		v.PaidAmount, v.RefundedAmount = fmtMoney(paidAmount, aq.Currency), fmtMoney(refundedAmount, aq.Currency)
		for _, row := range providers {
			v.Providers = append(v.Providers, analyticsDimensionRow{Key: row.Key, Label: row.Label, Attempts: row.Attempts,
				Paid: row.Paid, Failed: row.Failed, SuccessRate: analyticsRate(row.Paid, row.Paid+row.Failed)})
		}
		for _, row := range methods {
			v.Methods = append(v.Methods, analyticsDimensionRow{Key: row.Key, Label: strings.ToUpper(row.Label), Attempts: row.Attempts,
				Paid: row.Paid, Failed: row.Failed, SuccessRate: analyticsRate(row.Paid, row.Paid+row.Failed)})
		}
		for _, row := range failures {
			v.Failures = append(v.Failures, analyticsFailureRow{Code: row.Code, Message: row.Message, Count: row.Count})
		}
		for _, preset := range revenueRanges {
			qv := url.Values{"range": {preset.ID}}
			for key, value := range map[string]string{"app": aq.App, "currency": aq.Currency, "country": aq.Country} {
				if value != "" {
					qv.Set(key, value)
				}
			}
			v.Presets = append(v.Presets, revenuePreset{ID: preset.ID, Label: preset.Label,
				Href: prefix + "/analytics?" + qv.Encode(), Current: preset.ID == aq.Range})
		}
		qv := r.URL.Query()
		qv.Set("currency", aq.Currency)
		v.CountriesCSV = prefix + "/analytics/countries.csv?" + qv.Encode()
		v.Href = prefix + "/analytics?" + qv.Encode()
		return nil
	})
	return v, bad, err
}

func (d *Dashboard) analyticsPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	analytics, bad, err := d.analyticsData(ctx, r, org, d.mode(r, s))
	if err != nil {
		d.fail(w, err)
		return
	}
	if bad != "" {
		badRequest(w, bad)
		return
	}
	v := d.shell(r, s, "analytics", "Analytics")
	v.Analytics = &analytics
	d.render(w, http.StatusOK, "dash_analytics", v)
}

func (d *Dashboard) analyticsCountriesCSV(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	v, bad, err := d.analyticsData(ctx, r, org, d.mode(r, s))
	if err != nil {
		d.fail(w, err)
		return
	}
	if bad != "" {
		badRequest(w, bad)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="payment-analytics-by-country.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"country", "created", "paid", "failed", "waiting", "expired_or_canceled", "success_rate", "paid_amount", "refunded_amount", "currency"})
	for _, row := range v.CountryRows {
		_ = cw.Write([]string{analyticsCSVSafe(row.Code), strconv.FormatInt(row.Created, 10), strconv.FormatInt(row.Paid, 10), strconv.FormatInt(row.Failed, 10),
			strconv.FormatInt(row.Waiting, 10), strconv.FormatInt(row.Incomplete, 10), row.SuccessRate, row.PaidAmount, row.RefundedAmount, v.Currency})
	}
	cw.Flush()
}
