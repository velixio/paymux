package stripe

import (
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func disputeJSON(id, status string) map[string]any {
	return map[string]any{"id": id, "object": "dispute", "status": status, "reason": "product_not_received", "amount": 49900, "currency": "inr",
		"charge": "ch_3A1", "payment_intent": "pi_3A1", "created": nowUnix - 100, "evidence_details": map[string]any{"due_by": nowUnix + 86400}}
}

func TestDisputeReader(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/disputes/dp_1", 200, disputeJSON("dp_1", "needs_response"))
	f.reply("GET /v1/disputes", 200, map[string]any{"data": []any{disputeJSON("dp_1", "needs_response"), disputeJSON("dp_2", "warning_under_review")}, "has_more": true})
	c := newConn(t, f)
	d, err := c.FetchDispute(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"})
	if err != nil || d.Status != connector.DisputeNeedsResponse || d.ProviderPaymentID != "pi_3A1" || d.Amount != 49900 || d.DueBy != (nowUnix+86400)*1000 || d.Phase != "chargeback" {
		t.Fatalf("dispute = %+v %v", d, err)
	}
	pg, err := c.ListDisputes(bg, connector.TimeRange{From: 1760000000000, To: 1760086400000}, "")
	if err != nil || len(pg.Items) != 2 || pg.Next != "dp_2" || pg.Items[1].Status != connector.DisputeUnderReview || pg.Items[1].Phase != "inquiry" {
		t.Fatalf("page = %+v %v", pg, err)
	}
}

func TestAcceptDisputeClosesIt(t *testing.T) {
	f := newFake()
	f.reply("POST /v1/disputes/dp_1/close", 200, disputeJSON("dp_1", "lost"))
	c := newConn(t, f)
	if err := c.AcceptDispute(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.to("POST /v1/disputes/dp_1/close")) != 1 {
		t.Error("close not called")
	}
	if err := c.AcceptDispute(bg, connector.DisputeRef{}); err == nil {
		t.Error("an empty id was accepted")
	}
}

// Evidence is saved without submitting, then submitted, so it is kept even if
// the submission is refused.
func TestSubmitEvidenceUpdatesThenSubmits(t *testing.T) {
	f := newFake()
	var uploads [][]byte
	var order []string
	pdf := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF")
	wantTypes := map[string]string{"shipping_documentation": "application/pdf", "receipt_2": "image/png", "mystery": "image/jpeg"}
	f.on("POST /v1/files", func(w http.ResponseWriter, c call) {
		_, params, _ := mime.ParseMediaType(c.Header.Get("Content-Type"))
		mr := multipart.NewReader(strings.NewReader(string(c.Body)), params["boundary"])
		var name string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			buf := make([]byte, 1<<10)
			n, _ := p.Read(buf)
			switch p.FormName() {
			case "purpose":
				if string(buf[:n]) != "dispute_evidence" {
					t.Errorf("purpose = %q", buf[:n])
				}
			case "file":
				name = p.FileName()
				uploads = append(uploads, buf[:n])
				// The real Files API accepts only PDF, JPEG and PNG and rejects
				// application/octet-stream (verified against the live API).
				if ct := p.Header.Get("Content-Type"); ct != wantTypes[name] {
					t.Errorf("file %s sent as %q, want %q", name, ct, wantTypes[name])
				}
			}
		}
		order = append(order, "upload:"+name)
		write(w, 200, map[string]any{"id": "file_" + name})
	})
	f.on("POST /v1/disputes/dp_1", func(w http.ResponseWriter, c call) {
		if c.Form.Get("submit") == "true" {
			order = append(order, "submit")
		} else {
			order = append(order, "update")
		}
		write(w, 200, disputeJSON("dp_1", "under_review"))
	})
	c := newConn(t, f)
	err := c.SubmitEvidence(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"}, connector.Evidence{
		Text:  "The customer received the goods on 2026-10-12.",
		Files: map[string][]byte{"shipping_documentation": pdf, "receipt_2": png, "mystery": jpg}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "upload:mystery,upload:receipt_2,upload:shipping_documentation,update,submit" {
		t.Errorf("order = %v", order)
	}
	upd := f.to("POST /v1/disputes/dp_1")
	if len(upd) != 2 {
		t.Fatalf("updates = %d", len(upd))
	}
	for k, v := range map[string]string{"evidence[uncategorized_text]": "The customer received the goods on 2026-10-12.", "evidence[shipping_documentation]": "file_shipping_documentation",
		"evidence[receipt]": "file_receipt_2", "evidence[uncategorized_file]": "file_mystery", "submit": "false"} {
		if upd[0].Form.Get(k) != v {
			t.Errorf("update %s = %q, want %q", k, upd[0].Form.Get(k), v)
		}
	}
	if upd[1].Form.Get("submit") != "true" || len(upd[1].Form) != 1 {
		t.Errorf("submit form = %v", upd[1].Form)
	}
	if len(uploads) != 3 {
		t.Errorf("uploads = %d", len(uploads))
	}

	// Two files of one type, or nothing at all, are refused before anything is sent.
	before := f.count()
	if err := c.SubmitEvidence(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"}, connector.Evidence{Files: map[string][]byte{"receipt": {1}, "receipt_2": {2}}}); err == nil {
		t.Error("two receipts accepted")
	}
	if err := c.SubmitEvidence(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"}, connector.Evidence{}); err == nil {
		t.Error("empty evidence accepted")
	}
	// Stripe takes only PDF, JPEG and PNG: anything else is refused up front, and
	// before an earlier file of the same call has been uploaded.
	if err := c.SubmitEvidence(bg, connector.DisputeRef{ProviderDisputeID: "dp_1"}, connector.Evidence{Files: map[string][]byte{"receipt": pdf, "shipping_documentation": []byte("plain text")}}); err == nil {
		t.Error("a text file was accepted as evidence")
	}
	if f.count() != before {
		t.Errorf("%d requests for refused evidence", f.count()-before)
	}
}
