package einvoice_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/invoice/einvoice"
)

func TestBatchIsAJSONArrayOfValidDocuments(t *testing.T) {
	var docs []*einvoice.Invoice
	for _, s := range []einvoice.Source{intraB2B(), interB2B(), exportLUT()} {
		d, err := einvoice.Build(s)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	raw, err := einvoice.MarshalBatch(docs)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil || len(back) != 3 {
		t.Fatalf("%v %d", err, len(back))
	}
	if err := einvoice.ValidateBatch(raw); err != nil {
		t.Fatal(err)
	}
	if raw, _ := einvoice.MarshalBatch(nil); strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("empty batch = %q", raw)
	}
	bad := strings.Replace(string(raw), `"Pin": 560025`, `"Pin": 56002`, 1)
	if err := einvoice.ValidateBatch([]byte(bad)); err == nil || !strings.Contains(err.Error(), "document 2") {
		t.Errorf("a bad document should be named by position: %v", err)
	}
	if err := einvoice.ValidateBatch([]byte(`{}`)); err == nil {
		t.Error("an object is not a batch")
	}
}
