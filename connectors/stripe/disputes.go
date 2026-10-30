package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"

	"github.com/velixio/paymux/pkg/connector"
)

type disputeWire struct {
	ID              string               `json:"id"`
	Charge          expandable[struct{}] `json:"charge"`
	PaymentIntent   expandable[struct{}] `json:"payment_intent"`
	Status          string               `json:"status"`
	Reason          string               `json:"reason"`
	Amount          int64                `json:"amount"`
	Currency        string               `json:"currency"`
	Created         int64                `json:"created"`
	EvidenceDetails struct {
		DueBy int64 `json:"due_by"`
	} `json:"evidence_details"`
}

func mapDisputeStatus(raw string) connector.DisputeStatus {
	switch raw {
	case "needs_response", "warning_needs_response":
		return connector.DisputeNeedsResponse
	case "won":
		return connector.DisputeWon
	case "lost":
		return connector.DisputeLost
	case "warning_closed":
		return connector.DisputeClosed
	}
	return connector.DisputeUnderReview // "under_review", "warning_under_review" and anything new
}

func (d disputeWire) snapshot() connector.DisputeSnapshot {
	phase := "chargeback"
	if len(d.Status) > 8 && d.Status[:8] == "warning_" {
		phase = "inquiry" // an early warning, not yet a chargeback
	}
	return connector.DisputeSnapshot{ProviderDisputeID: d.ID, ProviderPaymentID: firstNonEmpty(d.PaymentIntent.ID, d.Charge.ID),
		Status: mapDisputeStatus(d.Status), RawStatus: d.Status, Phase: phase, Reason: d.Reason,
		Amount: fromStripe(d.Amount, d.Currency), Currency: upper(d.Currency), DueBy: ms(d.EvidenceDetails.DueBy), CreatedAt: ms(d.Created)}
}

// ListDisputes lists disputes created in the range, one page at a time.
func (s *stripe) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	var l list[disputeWire]
	if err := s.api.do(ctx, "GET", "/v1/disputes", listQuery(tr, cur, "created"), nil, &l, ""); err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	pg := connector.Page[connector.DisputeSnapshot]{Items: make([]connector.DisputeSnapshot, 0, len(l.Data))}
	for _, d := range l.Data {
		pg.Items = append(pg.Items, d.snapshot())
	}
	if l.HasMore && len(l.Data) > 0 {
		pg.Next = connector.Cursor(l.Data[len(l.Data)-1].ID)
	}
	return pg, nil
}

// FetchDispute returns one dispute.
func (s *stripe) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	var d disputeWire
	if err := s.api.do(ctx, "GET", "/v1/disputes/"+url.PathEscape(ref.ProviderDisputeID), nil, nil, &d, ""); err != nil {
		return connector.DisputeSnapshot{}, err
	}
	return d.snapshot(), nil
}

// AcceptDispute concedes the dispute: closing it forfeits the amount.
func (s *stripe) AcceptDispute(ctx context.Context, ref connector.DisputeRef) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("stripe: AcceptDispute needs a dispute id")
	}
	return s.api.do(ctx, "POST", "/v1/disputes/"+url.PathEscape(ref.ProviderDisputeID)+"/close", nil, url.Values{}, nil, "")
}

// evidenceFileFields take a file id; every other evidence key is text.
var evidenceFileFields = map[string]bool{
	"cancellation_policy": true, "customer_communication": true, "customer_signature": true, "duplicate_charge_documentation": true,
	"receipt": true, "refund_policy": true, "service_documentation": true, "shipping_documentation": true, "uncategorized_file": true,
}

var dupSuffix = regexp.MustCompile(`_[0-9]+$`)

// SubmitEvidence contests the dispute in two steps, as Stripe wants it: each
// file is uploaded (purpose dispute_evidence) and the evidence saved on the
// dispute without submitting, then the dispute is submitted. The evidence is
// therefore kept even if the submission is refused. Evidence.Files keys are
// Stripe evidence types ("receipt", "shipping_documentation", ...); a key
// that is not a file type is sent as uncategorized_file. Stripe takes one file
// per type, so two files of one type are an error. Evidence.Text becomes
// uncategorized_text.
//
// Verified live: update then submit=true as a second update is accepted, and the
// Files API takes only PDF, JPEG and PNG.
func (s *stripe) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("stripe: SubmitEvidence needs a dispute id")
	}
	if ev.Text == "" && len(ev.Files) == 0 {
		return errors.New("stripe: evidence needs a summary or at least one file")
	}
	path := "/v1/disputes/" + url.PathEscape(ref.ProviderDisputeID)
	keys := make([]string, 0, len(ev.Files))
	for k := range ev.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fieldOf := map[string]string{}
	taken := map[string]bool{}
	for _, k := range keys {
		field := dupSuffix.ReplaceAllString(k, "")
		if !evidenceFileFields[field] {
			field = "uncategorized_file"
		}
		if taken[field] {
			return fmt.Errorf("stripe: two files for the evidence type %q (Stripe takes one per type)", field)
		}
		if _, err := evidenceType(ev.Files[k]); err != nil {
			return fmt.Errorf("%w (%s)", err, k)
		}
		taken[field], fieldOf[k] = true, field
	}
	f := url.Values{}
	if ev.Text != "" {
		f.Set("evidence[uncategorized_text]", ev.Text)
	}
	for _, k := range keys {
		id, err := s.api.upload(ctx, k, ev.Files[k], "dispute_evidence")
		if err != nil {
			return fmt.Errorf("stripe: uploading %s: %w", k, err)
		}
		f.Set("evidence["+fieldOf[k]+"]", id)
	}
	f.Set("submit", "false")
	if err := s.api.do(ctx, "POST", path, nil, f, nil, ""); err != nil {
		return err
	}
	return s.api.do(ctx, "POST", path, nil, url.Values{"submit": {"true"}}, nil, "")
}

var (
	_ connector.DisputeReader    = (*stripe)(nil)
	_ connector.DisputeResponder = (*stripe)(nil)
)
