package razorpay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

// evidenceFields are the evidence types the contest endpoint takes as named
// fields. Any other type is sent under "others".
//
// TODO(spec): re-verify the field list, the document reference shape
// ({"id": ...}) and the "others" shape against Razorpay's Dispute APIs.
var evidenceFields = map[string]bool{
	"billing_proof": true, "shipping_proof": true, "cancellation_proof": true, "customer_communication": true,
	"proof_of_service": true, "explanation_letter": true, "refund_confirmation": true, "access_activity_log": true,
	"refund_cancellation_policy": true, "term_and_conditions": true,
}

var dupSuffix = regexp.MustCompile(`_[0-9]+$`)

// AcceptDispute concedes the dispute (POST /v1/disputes/{id}/accept).
func (r *razorpay) AcceptDispute(ctx context.Context, ref connector.DisputeRef) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("razorpay: AcceptDispute needs a dispute id")
	}
	return r.api.do(ctx, "POST", "/v1/disputes/"+url.PathEscape(ref.ProviderDisputeID)+"/accept", nil, nil, nil, nil)
}

// SubmitEvidence contests the dispute. Each file is uploaded to the Documents
// API (purpose dispute_evidence); the contest endpoint is then called twice
// with the same evidence: action "draft" to save it, and action "submit" to
// send it to the bank, so evidence is kept even if the submission is refused.
// Evidence.Files keys are evidence types ("shipping_proof"); a numeric suffix
// ("shipping_proof_2") names a further file of the same type.
func (r *razorpay) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	if ref.ProviderDisputeID == "" {
		return errors.New("razorpay: SubmitEvidence needs a dispute id")
	}
	if ev.Text == "" && len(ev.Files) == 0 {
		return errors.New("razorpay: evidence needs a summary or at least one file")
	}
	keys := make([]string, 0, len(ev.Files))
	for k := range ev.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable upload order

	body := map[string]any{}
	if ev.Text != "" {
		body["summary"] = ev.Text
	}
	others := map[string][]string{}
	var otherOrder []string
	for _, k := range keys {
		id, err := r.uploadDocument(ctx, k, ev.Files[k])
		if err != nil {
			return fmt.Errorf("razorpay: uploading %s: %w", k, err)
		}
		typ := dupSuffix.ReplaceAllString(k, "")
		if evidenceFields[typ] {
			list, _ := body[typ].([]map[string]string)
			body[typ] = append(list, map[string]string{"id": id})
			continue
		}
		if _, seen := others[typ]; !seen {
			otherOrder = append(otherOrder, typ)
		}
		others[typ] = append(others[typ], id)
	}
	if len(otherOrder) > 0 {
		list := make([]map[string]any, 0, len(otherOrder))
		for _, typ := range otherOrder {
			list = append(list, map[string]any{"type": typ, "document_ids": others[typ]})
		}
		body["others"] = list
	}
	path := "/v1/disputes/" + url.PathEscape(ref.ProviderDisputeID) + "/contest"
	for _, action := range []string{"draft", "submit"} {
		payload := map[string]any{"action": action}
		for k, v := range body {
			payload[k] = v
		}
		if err := r.api.do(ctx, "PATCH", path, nil, payload, nil, nil); err != nil {
			return fmt.Errorf("razorpay: contest (%s): %w", action, err)
		}
	}
	return nil
}

// uploadDocument sends one evidence file to POST /v1/documents (multipart)
// and returns the document id.
func (r *razorpay) uploadDocument(ctx context.Context, name string, data []byte) (string, error) {
	ctype := http.DetectContentType(data)
	ext := ".bin"
	switch {
	case ctype == "application/pdf":
		ext = ".pdf"
	case ctype == "image/png":
		ext = ".png"
	case ctype == "image/jpeg":
		ext = ".jpg"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("purpose", "dispute_evidence"); err != nil {
		return "", err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s%s"`, strings.NewReplacer(`"`, "", "\\", "", "\r", "", "\n", "").Replace(name), ext))
	h.Set("Content-Type", ctype)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := r.api.send(ctx, "POST", "/v1/documents", nil, &buf, w.FormDataContentType(), &doc, nil); err != nil {
		return "", err
	}
	if doc.ID == "" {
		return "", errors.New("the upload response has no document id")
	}
	return doc.ID, nil
}

var _ connector.DisputeResponder = (*razorpay)(nil)
