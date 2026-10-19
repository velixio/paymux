package invoice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
)

// Every object the API and the events show says whether it is a live one.
func TestLivemode(t *testing.T) {
	for mode, want := range map[string]string{"live": `"livemode":true`, "test": `"livemode":false`} {
		b, err := json.Marshal(ViewInvoice(store.Invoice{Mode: mode}))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("mode %s: %s (%v)", mode, b, err)
		}
	}
}

func TestCreditNoteLivemode(t *testing.T) {
	for mode, want := range map[string]string{"live": `"livemode":true`, "test": `"livemode":false`} {
		b, err := json.Marshal(ViewCreditNote(store.CreditNote{Mode: mode}))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("mode %s: %s (%v)", mode, b, err)
		}
	}
}

var _ = store.DefaultOrgID
