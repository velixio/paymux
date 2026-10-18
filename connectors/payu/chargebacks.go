package payu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// PayU's Chargeback API, from its documentation (API Reference: Chargeback). It
// is a separate service with its own credential: a token made on the Chargeback
// dashboard (Profile, Tokens) and sent as X-Optimus-API-Key, plus the PayU
// merchant id. Test: chbuat.payu.in, production: bankportal.payu.in.
//
//	GET   /api/v1/chargebacks?merchant_id=&from_date=dd-mm-yyyy&to_date=  (at most 20 days)
//	GET   /api/v1/chargebacks/{id}
//	PATCH /api/v1/chargebacks/dispute   (JSON; dispute_type accept or contest;
//	      files base64 in the body, at most 5 MB)
//
// TODO(spec): written from the documentation; the PayU test account has no
// chargeback token, so none of this has run against the real service. Open
// questions: that GET /chargebacks/{id} takes the chargeback id (the sample
// path uses a PayU id), the reason and form-field uuids below (from the
// "Read Reasons" table), and the response when a window holds more than 500.

const (
	chargebackWindow = 20 * 24 * time.Hour
	maxEvidenceBytes = 5 << 20

	// Reason uuids from PayU's Read Reasons table.
	reasonAcceptOthers    = "96ad8e74-c417-4ea7-a6ab-f768078d2c5c" // accept: Others
	reasonRejectDelivered = "9c2c093c-648a-40b5-8b79-461466578b0c" // reject: Product/Services delivered
	fieldEvidenceFile     = "200edfd3-84b6-4311-8486-23887f167772"
	chargebackErrNoToken  = "payu: the Chargeback API needs a chargeback token and the PayU merchant id (chargeback_token, chargeback_merchant_id): make the token on PayU's Chargeback dashboard (profile, tokens)"
	chargebackDateLayout  = "02-Jan-2006"
	chargebackQueryLayout = "02-01-2006"
)

func (c *client) chargebackBase() string {
	if c.test {
		return "https://chbuat.payu.in"
	}
	return "https://bankportal.payu.in"
}

type chargebackEntity struct {
	ID         json.Number `json:"id"`
	Amount     flexStr     `json:"chargeback-amount"`
	Date       string      `json:"chargeback-date"`
	Status     string      `json:"status"`
	ReplyBy    string      `json:"reply-before"`
	Reason     string      `json:"chargeback-reason"`
	Transation struct {
		PayuID flexStr `json:"payu-id"`
	} `json:"transaction-details"`
}

type chargebackData struct {
	ID         string           `json:"id"`
	Attributes chargebackEntity `json:"attributes"`
}

func parseChargebackDate(s string) int64 {
	t, err := time.ParseInLocation(chargebackDateLayout, s, ist)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func mapChargebackStatus(raw string) connector.DisputeStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending response", "insufficient document":
		return connector.DisputeNeedsResponse
	case "closed in merchant favour":
		return connector.DisputeWon
	case "closed customer favour", "closed under fraud":
		return connector.DisputeLost
	}
	return connector.DisputeUnderReview // pending doc review, submitted to bank, anything new
}

func (d chargebackData) snapshot() (connector.DisputeSnapshot, error) {
	a := d.Attributes
	amt, err := money.ParseDecimal(string(a.Amount), "INR")
	if err != nil {
		return connector.DisputeSnapshot{}, fmt.Errorf("payu: chargeback amount %q: %w", a.Amount, err)
	}
	id := firstNonEmpty(d.ID, string(a.ID))
	return connector.DisputeSnapshot{ProviderDisputeID: id, ProviderPaymentID: string(a.Transation.PayuID), Status: mapChargebackStatus(a.Status), RawStatus: a.Status,
		Phase: "chargeback", Reason: a.Reason, Amount: amt, Currency: "INR", DueBy: parseChargebackDate(a.ReplyBy), CreatedAt: parseChargebackDate(a.Date)}, nil
}

// chargebackCall makes one Chargeback API call.
func (p *payu) chargebackCall(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if p.cfg.ChargebackToken == "" || p.cfg.ChargebackMerchantID == "" {
		return fmt.Errorf("%w: %s", connector.ErrInvalidCredentials, chargebackErrNoToken)
	}
	u := p.api.chargebackBase() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Optimus-API-Key", p.cfg.ChargebackToken)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.api.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %v", connector.ErrTemporary, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", connector.ErrTemporary, err)
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("%w: the Chargeback API refused the token (HTTP %d)", connector.ErrInvalidCredentials, resp.StatusCode)
	case resp.StatusCode == 404:
		return connector.ErrNotFound
	case resp.StatusCode == 429:
		after := 5 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return connector.ErrRateLimited{RetryAfter: after}
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: HTTP %d", connector.ErrTemporary, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("payu: Chargeback API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("payu: decoding the Chargeback API response: %w", err)
	}
	return nil
}

// ListDisputes lists the chargebacks received in the range. The API reads at
// most 20 days at a time, so a longer range comes in pages, each the next window;
// the cursor is the start of the next window.
func (p *payu) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	to := time.Now()
	if tr.To > 0 {
		to = time.UnixMilli(tr.To)
	}
	from := to.Add(-chargebackWindow)
	if tr.From > 0 {
		from = time.UnixMilli(tr.From)
	}
	if cur != "" {
		ms, err := strconv.ParseInt(string(cur), 10, 64)
		if err != nil {
			return connector.Page[connector.DisputeSnapshot]{}, fmt.Errorf("payu: bad cursor %q", cur)
		}
		from = time.UnixMilli(ms)
	}
	end := from.Add(chargebackWindow - 24*time.Hour) // the window includes both dates
	next := end.Add(24 * time.Hour)
	if end.After(to) || !next.Before(to) {
		end, next = to.Add(-time.Millisecond), time.Time{} // the range is half-open: its last day is the one before To
	}
	q := url.Values{"merchant_id": {p.cfg.ChargebackMerchantID}, "from_date": {from.In(ist).Format(chargebackQueryLayout)}, "to_date": {end.In(ist).Format(chargebackQueryLayout)}}
	var out struct {
		Data []chargebackData `json:"data"`
	}
	if err := p.chargebackCall(ctx, "GET", "/api/v1/chargebacks", q, nil, &out); err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	pg := connector.Page[connector.DisputeSnapshot]{Items: make([]connector.DisputeSnapshot, 0, len(out.Data))}
	for _, d := range out.Data {
		s, err := d.snapshot()
		if err != nil {
			return pg, err
		}
		pg.Items = append(pg.Items, s)
	}
	if !next.IsZero() {
		pg.Next = connector.Cursor(strconv.FormatInt(next.UnixMilli(), 10))
	}
	return pg, nil
}

// FetchDispute returns one chargeback by its chargeback id.
func (p *payu) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	if ref.ProviderDisputeID == "" {
		return connector.DisputeSnapshot{}, errors.New("payu: FetchDispute needs a chargeback id")
	}
	var out struct {
		Data chargebackData `json:"data"`
	}
	if err := p.chargebackCall(ctx, "GET", "/api/v1/chargebacks/"+url.PathEscape(ref.ProviderDisputeID), nil, nil, &out); err != nil {
		return connector.DisputeSnapshot{}, err
	}
	s, err := out.Data.snapshot()
	if err != nil {
		return s, err
	}
	if s.ProviderDisputeID != ref.ProviderDisputeID {
		return connector.DisputeSnapshot{}, fmt.Errorf("%w: chargeback %s is not %s", connector.ErrNotFound, s.ProviderDisputeID, ref.ProviderDisputeID)
	}
	return s, nil
}

type reasonField struct {
	Identifier  string `json:"identifier"`
	Value       string `json:"value"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type reasonCode struct {
	UUID     string        `json:"uuid"`
	FormData []reasonField `json:"form_data"`
}

func (p *payu) dispute(ctx context.Context, id, kind string, reasons []reasonCode) error {
	body := map[string]any{"chargeback_id": id, "merchant_key": p.cfg.Key, "dispute_type": kind, "reason_code": reasons}
	return p.chargebackCall(ctx, "PATCH", "/api/v1/chargebacks/dispute", nil, body, nil)
}

// AcceptDispute concedes the chargeback (reason: Others).
func (p *payu) AcceptDispute(ctx context.Context, ref connector.DisputeRef) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("payu: AcceptDispute needs a chargeback id")
	}
	return p.dispute(ctx, ref.ProviderDisputeID, "accept", []reasonCode{{UUID: reasonAcceptOthers,
		FormData: []reasonField{{Identifier: reasonAcceptOthers, Value: "Accepted by the merchant"}}}})
}

// SubmitEvidence contests the chargeback (reason: Product/Services delivered).
// Text goes in as the comments; one file, at most 5 MB, goes in base64.
func (p *payu) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("payu: SubmitEvidence needs a chargeback id")
	}
	if ev.Text == "" && len(ev.Files) == 0 {
		return errors.New("payu: evidence needs a summary or a file")
	}
	if len(ev.Files) > 1 {
		return errors.New("payu: the Chargeback API takes one evidence file")
	}
	form := []reasonField{{Identifier: reasonRejectDelivered, Value: firstNonEmpty(ev.Text, "Evidence attached")}}
	names := make([]string, 0, len(ev.Files))
	for k := range ev.Files {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		content := ev.Files[k]
		if len(content) > maxEvidenceBytes {
			return fmt.Errorf("payu: evidence file %s is %d bytes, the limit is 5 MB", k, len(content))
		}
		ct := http.DetectContentType(content)
		ext := map[string]string{"application/pdf": ".pdf", "image/png": ".png", "image/jpeg": ".jpg"}[ct]
		if ext == "" {
			return errors.New("payu: evidence files must be PDF, PNG or JPEG")
		}
		form = append(form, reasonField{Identifier: fieldEvidenceFile, Value: base64.StdEncoding.EncodeToString(content), Filename: k + ext, ContentType: ct})
	}
	return p.dispute(ctx, ref.ProviderDisputeID, "contest", []reasonCode{{UUID: reasonRejectDelivered, FormData: form}})
}

var (
	_ connector.DisputeReader    = (*payu)(nil)
	_ connector.DisputeResponder = (*payu)(nil)
)
