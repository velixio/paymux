package dashboard

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Spellings of one address share one budget: the key is the canonical form the
// user lookup uses (lower case, trimmed).
func TestAddressSpellingsShareTheLockoutBudget(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		spellings := []string{"alice@example.com", "Alice@Example.com", " alice@example.com", "ALICE@EXAMPLE.COM ", "\talice@example.com\n"}
		for i := 0; i < 20; i++ {
			e.pwStep(c, spellings[i%len(spellings)], "wrong", fmt.Sprintf("198.51.%d.%d", i, i))
		}
		for _, sp := range spellings {
			if r, _ := e.pwStep(c, sp, goodPW, "192.0.2.50"); r.StatusCode != http.StatusTooManyRequests {
				t.Errorf("spelling %q escaped the shared budget: %d", sp, r.StatusCode)
			}
		}
		_ = secret
	})
}

// The same holds for wrong codes: every spelling of the account is one counter,
// and the per-address key is built from the same canonical form.
func TestTOTPBudgetIsNotForkedBySpelling(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		spellings := []string{"alice@example.com", "Alice@Example.com", " ALICE@example.com "}
		attempt := 0
		for ticket := 0; ticket < 2; ticket++ {
			c := e.client()
			if r, _ := e.pwStep(c, spellings[ticket%len(spellings)], goodPW, fmt.Sprintf("203.0.113.%d", 150+ticket)); r.StatusCode != 303 {
				t.Fatalf("password step: %d", r.StatusCode)
			}
			for i := 0; i < 5; i++ {
				attempt++
				e.codeStep(c, fmt.Sprintf("22222%d", i), fmt.Sprintf("203.0.113.%d", attempt))
			}
		}
		if r, _ := e.pwStep(e.client(), spellings[2], goodPW, "192.0.2.90"); r.StatusCode != http.StatusTooManyRequests {
			t.Errorf("ten wrong codes under different spellings did not lock the account: %d", r.StatusCode)
		}
	})
}
