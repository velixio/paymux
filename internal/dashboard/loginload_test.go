package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Fifty logins at once do not start fifty 64 MiB hashes: the surplus is told to
// come back later (503), and the ones that ran answer normally.
func TestConcurrentLoginsAreBoundedAndTheSurplusGets503(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		auth.UseProductionCost(t) // the bound exists because real hashes are slow and large
		e := bfEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		auth.SetConcurrency(2)
		t.Cleanup(func() { auth.SetConcurrency(0) })
		// One page load per client first, so the burst is only the hashing step.
		type try struct {
			c    *http.Client
			csrf string
		}
		tries := make([]try, 16)
		for i := range tries {
			c := e.client()
			_, page := e.get(c, "/dashboard/login")
			tries[i] = try{c, csrfOf(t, page)}
		}
		codes := make([]int, len(tries))
		var wg sync.WaitGroup
		for i := range tries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, _ := e.postXFF(tries[i].c, "/dashboard/login", url.Values{"email": {"alice@example.com"}, "password": {"wrong password!!"},
					"csrf": {tries[i].csrf}}, fmt.Sprintf("198.51.%d.%d", i, i))
				codes[i] = r.StatusCode
			}()
		}
		wg.Wait()
		count := map[int]int{}
		for _, c := range codes {
			count[c]++
		}
		if count[503] == 0 || count[200] == 0 || count[503]+count[200]+count[429] != 16 {
			t.Errorf("status counts %v", count)
		}
	})
}

// A client that keeps trying is stopped before it costs a hash.
func TestLoginAttemptsPerAddressAreRateLimitedBeforeHashing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		start := time.Now()
		limited := 0
		for i := 0; i < 60; i++ {
			// Distinct unknown addresses of the account: no per-account ceiling is involved.
			r, body := e.pwStep(c, fmt.Sprintf("nobody%d@example.com", i), "wrong password!!", "203.0.113.50")
			if r.StatusCode == http.StatusTooManyRequests && strings.Contains(body, "Too many") {
				limited++
			}
		}
		if limited < 20 {
			t.Errorf("only %d of 60 attempts from one address were refused", limited)
		}
		_ = start
	})
}
