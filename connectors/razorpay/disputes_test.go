package razorpay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
var pdfBytes = append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 32)...)

func responder(t *testing.T, f *fake) connector.DisputeResponder {
	t.Helper()
	c := newConn(t, f)
	r, ok := c.(connector.DisputeResponder)
	if !ok {
		t.Fatal("razorpay must be a DisputeResponder")
	}
	if !c.Info().Has(connector.CapDisputeRespond) {
		t.Error("CapDisputeRespond not declared")
	}
	return r
}

func TestAcceptDispute(t *testing.T) {
	f := newFake("captured")
	r := responder(t, f)
	if err := r.AcceptDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "disp_Nx3"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("POST", "/v1/disputes/disp_Nx3/accept")); n != 1 {
		t.Errorf("accept calls = %d", n)
	}
	f.acceptErr = 400
	err := r.AcceptDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "disp_Nx3"})
	var ae *apiError
	if err == nil || !errors.As(err, &ae) || ae.Status != 400 {
		t.Errorf("a refused accept must surface the provider's error: %v", err)
	}
	if err := r.AcceptDispute(context.Background(), connector.DisputeRef{}); err == nil {
		t.Error("accepting without an id")
	}
}

// parts decodes a recorded multipart document upload.
func parts(t *testing.T, c call) (fields map[string]string, filename, contentType string, data []byte) {
	t.Helper()
	mt, params, err := mime.ParseMediaType(c.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("content type %q: %v", c.Header.Get("Content-Type"), err)
	}
	fields = map[string]string{}
	mr := multipart.NewReader(bytes.NewReader(c.Body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			filename, contentType, data = p.FileName(), p.Header.Get("Content-Type"), b
		} else {
			fields[p.FormName()] = string(b)
		}
	}
}

func TestSubmitEvidenceUploadsDocumentsThenContests(t *testing.T) {
	f := newFake("captured")
	r := responder(t, f)
	err := r.SubmitEvidence(context.Background(), connector.DisputeRef{ProviderDisputeID: "disp_Nx3"}, connector.Evidence{
		Text:  "Delivered on 2026-10-01",
		Files: map[string][]byte{"shipping_proof": pdfBytes, "shipping_proof_2": pngBytes, "billing_proof": pdfBytes, "weird_type": pdfBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	docs := f.callsTo("POST", "/v1/documents")
	if len(docs) != 4 {
		t.Fatalf("documents uploaded = %d", len(docs))
	}
	seen := map[string]bool{}
	for _, d := range docs {
		fields, name, ct, data := parts(t, d)
		if fields["purpose"] != "dispute_evidence" || name == "" || len(data) == 0 {
			t.Errorf("upload fields=%v file=%q", fields, name)
		}
		switch {
		case bytes.HasPrefix(data, []byte("%PDF")):
			if ct != "application/pdf" || !strings.HasSuffix(name, ".pdf") {
				t.Errorf("pdf upload %q %q", name, ct)
			}
		case bytes.HasPrefix(data, []byte("\x89PNG")):
			if ct != "image/png" || !strings.HasSuffix(name, ".png") {
				t.Errorf("png upload %q %q", name, ct)
			}
		}
		seen[name] = true
	}
	if len(seen) != 4 {
		t.Errorf("file names must be distinct: %v", seen)
	}
	contests := f.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest")
	if len(contests) != 2 {
		t.Fatalf("contest calls = %d, want a draft and a submit", len(contests))
	}
	var first, second map[string]any
	_ = json.Unmarshal(contests[0].Body, &first)
	_ = json.Unmarshal(contests[1].Body, &second)
	if first["action"] != "draft" || second["action"] != "submit" {
		t.Errorf("actions %v then %v", first["action"], second["action"])
	}
	if first["summary"] != "Delivered on 2026-10-01" {
		t.Errorf("summary %v", first["summary"])
	}
	ship, _ := first["shipping_proof"].([]any)
	bill, _ := first["billing_proof"].([]any)
	others, _ := first["others"].([]any)
	if len(ship) != 2 || len(bill) != 1 || len(others) != 1 {
		t.Fatalf("evidence payload %v", first)
	}
	if id := ship[0].(map[string]any)["id"].(string); !strings.HasPrefix(id, "doc_Nx") {
		t.Errorf("document ref %v", ship[0])
	}
	o := others[0].(map[string]any)
	if o["type"] != "weird_type" || len(o["document_ids"].([]any)) != 1 {
		t.Errorf("unknown evidence types are sent as others: %v", o)
	}
	// Both calls carry the same evidence.
	delete(first, "action")
	delete(second, "action")
	if len(first) != len(second) {
		t.Errorf("draft %v / submit %v", first, second)
	}
}

func TestSubmitEvidenceTextOnlyAndFailures(t *testing.T) {
	f := newFake("captured")
	r := responder(t, f)
	ref := connector.DisputeRef{ProviderDisputeID: "disp_Nx3"}
	if err := r.SubmitEvidence(context.Background(), ref, connector.Evidence{Text: "We delivered it"}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsTo("POST", "/v1/documents")) != 0 || len(f.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest")) != 2 {
		t.Error("text-only evidence needs no uploads")
	}
	if err := r.SubmitEvidence(context.Background(), ref, connector.Evidence{}); err == nil {
		t.Error("empty evidence accepted")
	}
	// A failed upload stops before anything is contested.
	g := newFake("captured")
	g.docFail = 400
	before := len(g.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest"))
	err := responder(t, g).SubmitEvidence(context.Background(), ref, connector.Evidence{Files: map[string][]byte{"billing_proof": pdfBytes}})
	if err == nil || len(g.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest")) != before {
		t.Errorf("upload failure: %v", err)
	}
	// A refused contest (period over) is an error, and the draft is not followed by a submit.
	h := newFake("captured")
	h.contestErr = 400
	err = responder(t, h).SubmitEvidence(context.Background(), ref, connector.Evidence{Text: "x"})
	if err == nil || len(h.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest")) != 1 {
		t.Errorf("refused contest: %v calls=%d", err, len(h.callsTo("PATCH", "/v1/disputes/disp_Nx3/contest")))
	}
}
