// Package fxmarket keeps indicative exchange rates from a public provider, for
// display on the dashboard. It writes only to market_rates: invoices and GST
// read fx_rates (the RBI / FBIL reference rates) and never see these.
package fxmarket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector/money"
)

// JobRefresh is the job kind that refreshes the rates.
const JobRefresh = "fx.market_refresh"

// ProviderFrankfurter names the Frankfurter provider, in the fx_provider setting
// and in stored rows.
const ProviderFrankfurter = "frankfurter"

// Off is the fx_provider value that neither fetches nor shows market rates.
const Off = "off"

// refreshAfter is how old stored rates may get before the provider is asked
// again. The job wakes hourly, because a new schedule first runs at its next
// slot and a new install should not wait long for its first rates; most wake-ups
// find the rates fresh and do nothing.
const refreshAfter = 6 * time.Hour

// maxRateLen is the longest rate the conversion code accepts (invoice.parseRate).
const maxRateLen = 20

// rateDecimals keeps weak currencies meaningful: 1 IDR is about 0.0055 INR.
const rateDecimals = 8

// maxBody bounds what is read from the provider.
const maxBody = 1 << 16

// Quote is a provider's rates of one day. Rates are INR per 1 unit, by currency.
type Quote struct {
	Date, Source string
	Rates        map[string]string
}

// Provider fetches the current rates.
type Provider interface {
	Fetch(ctx context.Context) (Quote, error)
}

// Options configures Service.
type Options struct {
	DB        *store.DB
	Providers map[string]Provider // by fx_provider setting value
	Clock     func() time.Time    // default time.Now
	Logger    *slog.Logger        // default slog.Default()
}

// Service stores the provider's rates.
type Service struct{ o Options }

// New builds a Service.
func New(o Options) *Service {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{o: o}
}

// Register installs the refresh job and its schedule on q.
func (s *Service) Register(q *jobs.Queue) error {
	q.Register(JobRefresh, s.runJob)
	return q.AddSchedule(jobs.Schedule{Name: JobRefresh, Kind: JobRefresh, Every: time.Hour})
}

// errFetch marks a failure to get rates from the provider.
var errFetch = errors.New("fetch failed")

// runJob is the job handler. A failed fetch is not a failed job: the rates are
// only for display and the next hourly run tries again, while queue retries
// would fill the logs of a host that has no outbound access.
func (s *Service) runJob(ctx context.Context, _ json.RawMessage) error {
	err := s.Refresh(ctx)
	if errors.Is(err, errFetch) {
		s.o.Logger.Warn("market rates not refreshed", "err", err)
		return nil
	}
	return err
}

// Selected is the org's fx_provider setting.
func Selected(ctx context.Context, q store.Queryer, org string) (string, error) {
	val, err := settings.ResolveOrgWith(ctx, q, org, "fx_provider")
	if err != nil {
		return "", err
	}
	var name string
	if err := val.Decode(&name); err != nil {
		return "", err
	}
	return name, nil
}

// Refresh fetches and stores the rates of the provider chosen in the fx_provider
// setting, unless it is off or the stored rates are still fresh.
func (s *Service) Refresh(ctx context.Context) error {
	now := s.o.Clock()
	var name string
	var last time.Time
	var fetched bool
	err := s.o.DB.Read(ctx, func(q store.Querier) error {
		var err error
		if name, err = Selected(ctx, q, store.DefaultOrgID); err != nil {
			return err
		}
		last, fetched, err = store.LastMarketFetch(ctx, q)
		return err
	})
	if err != nil {
		return err
	}
	if name == Off || fetched && now.Sub(last) < refreshAfter {
		return nil
	}
	provider, ok := s.o.Providers[name]
	if !ok {
		return fmt.Errorf("fxmarket: unknown provider %q", name)
	}
	quote, err := provider.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("fxmarket: %w: %w", errFetch, err)
	}
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		for currency, rate := range quote.Rates {
			r := store.MarketRate{Currency: currency, Date: quote.Date, Rate: rate, Source: quote.Source}
			if err := store.UpsertMarketRate(ctx, tx, r, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// Frankfurter reads European Central Bank reference rates from
// https://frankfurter.dev. It needs no key and is updated once per business day.
type Frankfurter struct {
	BaseURL string       // default https://api.frankfurter.dev
	Client  *http.Client // default a client with a 10 second timeout
}

// plainDecimal is the only number form accepted, so a hostile exponent such as
// 1e999999 is never expanded.
var plainDecimal = regexp.MustCompile(`^[0-9]{1,15}(\.[0-9]{1,15})?$`)

// Fetch asks for rates based on EUR, the ECB's own quote currency, and derives
// INR per unit from them; asking for INR as the base would invert rounded values.
func (p *Frankfurter) Fetch(ctx context.Context) (Quote, error) {
	base, client := p.BaseURL, p.Client
	if base == "" {
		base = "https://api.frankfurter.dev"
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/latest?base=EUR", nil)
	if err != nil {
		return Quote{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Quote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("frankfurter: status %d", resp.StatusCode)
	}
	var body struct {
		Base  string                 `json:"base"`
		Date  string                 `json:"date"`
		Rates map[string]json.Number `json:"rates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return Quote{}, fmt.Errorf("frankfurter: %w", err)
	}
	if body.Base != "EUR" {
		return Quote{}, fmt.Errorf("frankfurter: base %q, want EUR", body.Base)
	}
	if _, err := time.Parse("2006-01-02", body.Date); err != nil {
		return Quote{}, fmt.Errorf("frankfurter: date %q", body.Date)
	}
	inrPerEUR, ok := positive(body.Rates["INR"])
	if !ok {
		return Quote{}, errors.New("frankfurter: no INR rate")
	}
	q := Quote{Date: body.Date, Source: ProviderFrankfurter, Rates: map[string]string{}}
	if s, ok := rateString(inrPerEUR); ok {
		q.Rates["EUR"] = s
	}
	for currency, n := range body.Rates {
		if currency == "INR" {
			continue
		}
		if _, err := money.Exponent(currency); err != nil {
			continue
		}
		perEUR, ok := positive(n)
		if !ok {
			continue
		}
		if s, ok := rateString(new(big.Rat).Quo(inrPerEUR, perEUR)); ok {
			q.Rates[currency] = s
		}
	}
	return q, nil
}

func positive(n json.Number) (*big.Rat, bool) {
	if !plainDecimal.MatchString(n.String()) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(n.String())
	return r, ok && r.Sign() > 0
}

// rateString formats a rate, refusing one that rounds to zero or that the
// conversion code would reject.
func rateString(r *big.Rat) (string, bool) {
	s := r.FloatString(rateDecimals)
	if len(s) > maxRateLen || strings.Trim(s, "0.") == "" {
		return "", false
	}
	return s, true
}
