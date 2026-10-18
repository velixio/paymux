package whop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Disputes, from Whop's API reference: GET /disputes (needs account_id, the
// company id, and the payment:dispute:read scope of the key), GET /disputes/{id},
// POST /disputes/{id}/update_evidence and POST /disputes/{id}/submit_evidence.
// Whop has no call to accept a dispute: not responding concedes it.
//
// TODO(spec): written from the documentation. The sandbox key had no
// payment:dispute:read scope (403 "Company API key is not authorized for the
// payment:dispute:read scope"), so the listing and the evidence calls were not
// run; the Files API (POST /files, answered 201) was.

type disputeEntity struct {
	ID            string `json:"id"`
	Amount        number `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	NeedsResponse string `json:"needs_response_by"`
	CreatedAt     string `json:"created_at"`
	Payment       *struct {
		ID string `json:"id"`
	} `json:"payment"`
}

func mapDisputeStatus(raw string) connector.DisputeStatus {
	switch strings.TrimPrefix(raw, "warning_") {
	case "needs_response":
		return connector.DisputeNeedsResponse
	case "under_review":
		return connector.DisputeUnderReview
	case "won":
		return connector.DisputeWon
	case "lost":
		return connector.DisputeLost
	}
	return connector.DisputeClosed // closed, other, anything new
}

func (d disputeEntity) snapshot() (connector.DisputeSnapshot, error) {
	cur := strings.ToUpper(d.Currency)
	amt, err := minor(d.Amount, firstNonEmpty(cur, "USD"))
	if err != nil {
		return connector.DisputeSnapshot{}, err
	}
	phase := "chargeback"
	if strings.HasPrefix(d.Status, "warning_") {
		phase = "inquiry" // an early warning, not yet a chargeback
	}
	snap := connector.DisputeSnapshot{ProviderDisputeID: d.ID, Status: mapDisputeStatus(d.Status), RawStatus: d.Status, Phase: phase, Reason: d.Reason,
		Amount: amt, Currency: cur, DueBy: parseTime(d.NeedsResponse), CreatedAt: parseTime(d.CreatedAt)}
	if d.Payment != nil {
		snap.ProviderPaymentID = d.Payment.ID
	}
	return snap, nil
}

// ListDisputes lists the company's disputes created in the range, one page at a time.
func (w *whop) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	q := url.Values{"account_id": {w.cfg.CompanyID}, "first": {"50"}, "direction": {"desc"}}
	if tr.From > 0 {
		q.Set("created_after", time.UnixMilli(tr.From).UTC().Format(time.RFC3339))
	}
	if tr.To > 0 {
		q.Set("created_before", time.UnixMilli(tr.To).UTC().Format(time.RFC3339))
	}
	if cur != "" {
		q.Set("after", string(cur))
	}
	var out struct {
		Data     []disputeEntity `json:"data"`
		PageInfo struct {
			EndCursor   string `json:"end_cursor"`
			HasNextPage bool   `json:"has_next_page"`
		} `json:"page_info"`
	}
	if err := w.do(ctx, "GET", "/disputes", q, nil, &out, nil); err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	pg := connector.Page[connector.DisputeSnapshot]{Items: make([]connector.DisputeSnapshot, 0, len(out.Data))}
	for _, d := range out.Data {
		s, err := d.snapshot()
		if err != nil {
			return connector.Page[connector.DisputeSnapshot]{}, err
		}
		pg.Items = append(pg.Items, s)
	}
	if out.PageInfo.HasNextPage && out.PageInfo.EndCursor != "" {
		pg.Next = connector.Cursor(out.PageInfo.EndCursor)
	}
	return pg, nil
}

// FetchDispute returns one dispute.
func (w *whop) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	if ref.ProviderDisputeID == "" {
		return connector.DisputeSnapshot{}, errors.New("whop: FetchDispute needs a dispute id")
	}
	var d disputeEntity
	if err := w.do(ctx, "GET", "/disputes/"+url.PathEscape(ref.ProviderDisputeID), nil, nil, &d, nil); err != nil {
		return connector.DisputeSnapshot{}, err
	}
	return d.snapshot()
}

// AcceptDispute is not supported: Whop has no call to accept a dispute, and
// leaving it unanswered concedes it.
func (w *whop) AcceptDispute(context.Context, connector.DisputeRef) error {
	return fmt.Errorf("%w: Whop has no call to accept a dispute; not responding concedes it", connector.ErrUnsupported)
}

// evidenceSlots are the attachment slots of update_evidence; every other file
// goes to uncategorized_attachment, which takes one.
var evidenceSlots = map[string]string{
	"cancellation_policy": "cancellation_policy_attachment", "customer_communication": "customer_communication_attachment",
	"refund_policy": "refund_policy_attachment",
}

// uploadEvidence makes a file (POST /files) and PUTs its bytes to the upload URL.
func (w *whop) uploadEvidence(ctx context.Context, name string, content []byte) (string, error) {
	ct := http.DetectContentType(content)
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "application/pdf": ".pdf"}[ct]
	if ext == "" {
		ct, ext = "application/octet-stream", ".bin"
	}
	var f struct {
		ID            string            `json:"id"`
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
	}
	if err := w.do(ctx, "POST", "/files", nil, map[string]string{"filename": name + ext}, &f, nil); err != nil {
		return "", err
	}
	if f.ID == "" || f.UploadURL == "" {
		return "", errors.New("whop: file response lacks an id or an upload_url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, f.UploadURL, bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	for k, v := range f.UploadHeaders {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: uploading evidence: %v", connector.ErrTemporary, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("whop: uploading evidence: HTTP %d", resp.StatusCode)
	}
	return f.ID, nil
}

// SubmitEvidence saves the evidence on the dispute and submits it: Text becomes
// the notes; each file is uploaded and attached by id. A key naming one of
// cancellation_policy, customer_communication or refund_policy fills that slot;
// the one other file goes to uncategorized_attachment (a second is an error).
func (w *whop) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("whop: SubmitEvidence needs a dispute id")
	}
	if ev.Text == "" && len(ev.Files) == 0 {
		return errors.New("whop: evidence needs a summary or at least one file")
	}
	keys := make([]string, 0, len(ev.Files))
	for k := range ev.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	slotOf := map[string]string{}
	taken := map[string]bool{}
	for _, k := range keys {
		slot := evidenceSlots[k]
		if slot == "" {
			slot = "uncategorized_attachment"
		}
		if taken[slot] {
			return fmt.Errorf("whop: two files for %s (Whop takes one)", slot)
		}
		taken[slot], slotOf[k] = true, slot
	}
	body := map[string]any{}
	if ev.Text != "" {
		body["notes"] = ev.Text
	}
	for _, k := range keys {
		id, err := w.uploadEvidence(ctx, k, ev.Files[k])
		if err != nil {
			return fmt.Errorf("whop: evidence file %s: %w", k, err)
		}
		body[slotOf[k]] = map[string]string{"id": id}
	}
	path := "/disputes/" + url.PathEscape(ref.ProviderDisputeID)
	if err := w.do(ctx, "POST", path+"/update_evidence", nil, body, nil, nil); err != nil {
		return err
	}
	return w.do(ctx, "POST", path+"/submit_evidence", nil, nil, nil, nil)
}

var (
	_ connector.DisputeReader    = (*whop)(nil)
	_ connector.DisputeResponder = (*whop)(nil)
)
