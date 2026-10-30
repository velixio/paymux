package payouts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
)

// Every object the API and the events show says whether it is a live one.
func TestLivemode(t *testing.T) {
	for mode, want := range map[string]string{"live": `"livemode":true`, "test": `"livemode":false`} {
		b, err := json.Marshal(ViewPayout(store.Payout{Mode: mode}))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("mode %s: %s (%v)", mode, b, err)
		}
	}
}

var _ = store.DefaultOrgID
