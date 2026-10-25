package payouts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/payouts"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	db     *store.DB
	svc    *payouts.Service
	q      *jobs.Queue
	fake   *fakeconn.State
	acct   string
	scope  core.Scope
	now    time.Time
	mu     sync.Mutex
	events []events.Event
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
	e.run()
}

func (e *env) run() {
	e.t.Helper()
	for i := 0; i < 40; i++ {
		n, err := e.q.RunOnce(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatal("jobs keep coming")
}

func (e *env) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
}

func newEnv(t *testing.T, db *store.DB, purposes []string) *env {
	t.Helper()
	e := &env{t: t, db: db, now: t0, fake: fakeconn.Register("pay")}
	box, err := secrets.NewBox(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://pay.example.com")
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: e.clock})
	e.q = jobs.New(db, jobs.Options{Clock: e.clock, Jitter: func() float64 { return 0 }})
	emit := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		e.events = append(e.events, ev)
		e.mu.Unlock()
		return nil
	})
	e.svc = payouts.New(payouts.Options{DB: db, Box: box, Connectors: payouts.ConnectorsFunc(func(ctx context.Context, id string) (connector.Connector, error) {
		live, err := conns.Get(ctx, id)
		return live.Connector, err
	}), Jobs: e.q, Emitter: emit, Now: e.clock})
	e.svc.RegisterJobs(e.q)
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", t0); err != nil {
			return err
		}
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: t0})
	})
	if err != nil {
		t.Fatal(err)
	}
	pa, err := conns.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
		Connector: e.fake.Name, Name: "main", Mode: "live", Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1",
		Currencies: []string{"INR"}, Purposes: purposes})
	if err != nil {
		t.Fatal(err)
	}
	e.acct = pa.ID
	e.scope = core.Scope{AppID: "app_1", Mode: "live"}
	return e
}

func bank() connector.Beneficiary {
	return connector.Beneficiary{Name: "Asha Rao", AccountNumber: "123456789012", IFSC: "HDFC0001234"}
}

func (e *env) create(mut func(*payouts.CreateInput)) (store.Payout, error) {
	in := payouts.CreateInput{Scope: e.scope, Amount: 250000, Currency: "inr", Beneficiary: bank(), Narration: "October payout", Metadata: map[string]string{"k": "v"}}
	if mut != nil {
		mut(&in)
	}
	return e.svc.Create(ctx, in)
}

func (e *env) get(id string) store.Payout {
	e.t.Helper()
	p, err := e.svc.Get(ctx, e.scope, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestPayoutIsSubmittedWithItsReferenceAndFollowedToSuccess(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"one_time", "payout"})
		p, err := e.create(nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != "pending" || p.Currency != "INR" || p.ProviderAccountID != e.acct || !strings.HasPrefix(p.ID, "po_") || p.BeneficiaryLabel != "••••9012 HDFC" {
			t.Fatalf("created = %+v", p)
		}
		if len(e.fake.PayoutCalls) != 0 {
			t.Fatal("the provider was called inside the request")
		}

		e.run()
		if len(e.fake.PayoutCalls) != 1 {
			t.Fatalf("provider calls = %d", len(e.fake.PayoutCalls))
		}
		call := e.fake.PayoutCalls[0]
		if call.ProviderRef != p.ProviderRef || call.Amount != 250000 || call.Currency != "INR" || call.Beneficiary != bank() || call.Narration != "October payout" {
			t.Fatalf("request = %+v", call)
		}
		if got := e.get(p.ID); got.Status != "processing" || got.ProviderPayoutID != "pout_"+p.ProviderRef {
			t.Fatalf("after submit = %+v", got)
		}

		// The provider completes it; the next poll books it.
		e.fake.Locked(func() {
			s := e.fake.Payouts[p.ProviderRef]
			s.Status, s.RawStatus, s.UTR = connector.PayoutSucceeded, "processed", "UTR123"
			e.fake.Payouts[p.ProviderRef] = s
		})
		e.advance(61 * time.Second)
		got := e.get(p.ID)
		if got.Status != "succeeded" || got.CompletedAt.IsZero() || got.RawStatus != "processed" {
			t.Fatalf("after poll = %+v", got)
		}
		want := []string{"payout.created", "payout.processing", "payout.succeeded"}
		if strings.Join(e.types(), ",") != strings.Join(want, ",") {
			t.Fatalf("events = %v", e.types())
		}
		// Further polls change nothing.
		e.advance(48 * time.Hour)
		if len(e.types()) != 3 {
			t.Errorf("events after completion: %v", e.types())
		}
	})
}

func TestPayoutNeverPaysTwiceWhenTheProviderTimesOut(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		n := 0
		e.fake.Locked(func() {
			e.fake.PayoutFn = func(r connector.PayoutRequest) (connector.PayoutSnapshot, error) {
				n++
				if n == 1 { // the provider took it but the answer was lost
					e.fake.Payouts[r.ProviderRef] = connector.PayoutSnapshot{ProviderRef: r.ProviderRef, ProviderPayoutID: "po_x", Status: connector.PayoutProcessing, Amount: r.Amount, Currency: r.Currency}
					return connector.PayoutSnapshot{}, connector.ErrTemporary
				}
				return connector.PayoutSnapshot{}, errors.New("the fake should answer repeats from its own memory")
			}
		})
		p, _ := e.create(nil)
		e.run()
		if got := e.get(p.ID); got.Status != "pending" || got.SubmitAttempts != 1 {
			t.Fatalf("after a timeout: %+v", got)
		}
		e.advance(10 * time.Minute) // the queue retries with backoff
		e.advance(time.Hour)
		got := e.get(p.ID)
		if got.Status != "processing" || got.ProviderPayoutID != "po_x" {
			t.Fatalf("after the retry: %+v", got)
		}
		refs := map[string]bool{}
		for _, c := range e.fake.PayoutCalls {
			refs[c.ProviderRef] = true
		}
		if len(e.fake.PayoutCalls) < 2 || len(refs) != 1 {
			t.Errorf("every attempt must carry the same reference: %+v", e.fake.PayoutCalls)
		}
	})
}

func TestPayoutRejectedByTheProviderFails(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		e.fake.Locked(func() {
			e.fake.PayoutFn = func(connector.PayoutRequest) (connector.PayoutSnapshot, error) {
				return connector.PayoutSnapshot{}, errors.New("invalid beneficiary account")
			}
		})
		p, _ := e.create(nil)
		e.run()
		got := e.get(p.ID)
		if got.Status != "failed" || got.FailureCode != "rejected" || !strings.Contains(got.FailureMessage, "invalid beneficiary") || got.CompletedAt.IsZero() {
			t.Fatalf("rejected payout = %+v", got)
		}
		if strings.Join(e.types(), ",") != "payout.created,payout.failed" {
			t.Errorf("events = %v", e.types())
		}
		e.advance(48 * time.Hour)
		if len(e.fake.PayoutCalls) != 1 {
			t.Errorf("a rejected payout was retried (%d calls)", len(e.fake.PayoutCalls))
		}
	})
}

func TestPayoutStatusMappingFailureAndReversal(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		failedLater, _ := e.create(nil)
		reversed, _ := e.create(func(in *payouts.CreateInput) {
			in.Beneficiary = connector.Beneficiary{Name: "Ravi", UPI: "ravi@okbank"}
		})
		unknown, _ := e.create(nil)
		e.run()
		set := func(p store.Payout, st connector.PayoutStatus, code string) {
			e.fake.Locked(func() {
				s := e.fake.Payouts[p.ProviderRef]
				s.Status, s.RawStatus, s.ErrorCode, s.ErrorMessage = st, string(st), code, "bank said no"
				e.fake.Payouts[p.ProviderRef] = s
			})
		}
		set(failedLater, connector.PayoutFailed, "ACCOUNT_CLOSED")
		set(reversed, connector.PayoutSucceeded, "")
		set(unknown, "some_new_provider_status", "")
		e.advance(61 * time.Second)
		if got := e.get(failedLater.ID); got.Status != "failed" || got.FailureCode != "ACCOUNT_CLOSED" || got.FailureMessage != "bank said no" {
			t.Errorf("failed = %+v", got)
		}
		if got := e.get(reversed.ID); got.Status != "succeeded" || got.BeneficiaryLabel != "ra•••@okbank" {
			t.Errorf("succeeded = %+v", got)
		}
		if got := e.get(unknown.ID); got.Status != "processing" {
			t.Errorf("an unknown provider status must stay in flight, got %s", got.Status)
		}
		// A succeeded payout can still come back; a failed one never changes.
		set(reversed, connector.PayoutReversed, "")
		set(failedLater, connector.PayoutSucceeded, "")
		e.advance(25 * time.Hour)
		if got := e.get(reversed.ID); got.Status != "reversed" {
			t.Errorf("reversal not seen: %+v", got)
		}
		if got := e.get(failedLater.ID); got.Status != "failed" {
			t.Errorf("a failed payout moved to %s", got.Status)
		}
	})
}

func TestPayoutRoutingAndValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		// An account that did not opt in with the payout purpose is never used.
		noPurpose := newEnv(t, db, []string{"one_time"})
		if _, err := noPurpose.create(nil); !errors.Is(err, payouts.ErrNoRoute) {
			t.Fatalf("account without the payout purpose: %v", err)
		}
	})
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		if _, err := e.create(func(in *payouts.CreateInput) { in.Currency = "USD" }); !errors.Is(err, payouts.ErrNoRoute) {
			t.Errorf("currency the account does not accept: %v", err)
		}
		if _, err := e.create(func(in *payouts.CreateInput) { in.ProviderAccountID = "pa_other" }); !errors.Is(err, payouts.ErrNoRoute) {
			t.Errorf("unknown account: %v", err)
		}
		if _, err := e.create(func(in *payouts.CreateInput) { in.Scope.Mode = "test" }); !errors.Is(err, payouts.ErrNoRoute) {
			t.Errorf("the live account used in test mode: %v", err)
		}
		for name, mut := range map[string]func(*payouts.CreateInput){
			"zero amount":    func(in *payouts.CreateInput) { in.Amount = 0 },
			"negative":       func(in *payouts.CreateInput) { in.Amount = -5 },
			"bad currency":   func(in *payouts.CreateInput) { in.Currency = "rupees" },
			"no name":        func(in *payouts.CreateInput) { in.Beneficiary.Name = " " },
			"no destination": func(in *payouts.CreateInput) { in.Beneficiary = connector.Beneficiary{Name: "A"} },
			"bad ifsc":       func(in *payouts.CreateInput) { in.Beneficiary.IFSC = "HDFC1234567" },
			"bad account":    func(in *payouts.CreateInput) { in.Beneficiary.AccountNumber = "12ab" },
			"bad upi":        func(in *payouts.CreateInput) { in.Beneficiary = connector.Beneficiary{Name: "A", UPI: "nope"} },
			"bank and upi":   func(in *payouts.CreateInput) { in.Beneficiary.UPI = "a@okbank" },
			"long narration": func(in *payouts.CreateInput) { in.Narration = strings.Repeat("x", 101) },
			"too much metadata": func(in *payouts.CreateInput) {
				in.Metadata = map[string]string{}
				for i := 0; i < 21; i++ {
					in.Metadata[strings.Repeat("k", i+1)] = "v"
				}
			},
		} {
			if _, err := e.create(mut); !errors.Is(err, payouts.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if n := e.count("SELECT COUNT(*) FROM payouts"); n != 0 {
			t.Errorf("rejected requests left %d payouts", n)
		}
	})
}

func TestBeneficiaryIsStoredSealedAndScopesAreEnforced(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		p, _ := e.create(nil)
		var raw []byte
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT beneficiary_enc FROM payouts WHERE id = ?", p.ID).Scan(&raw)
		})
		if len(raw) == 0 || bytes.Contains(raw, []byte("123456789012")) || bytes.Contains(raw, []byte("HDFC")) || bytes.Contains(raw, []byte("Asha")) {
			t.Error("the beneficiary is stored readable")
		}
		view, _ := json.Marshal(payouts.ViewPayout(p))
		if bytes.Contains(view, []byte("123456789012")) || !bytes.Contains(view, []byte("••••9012")) {
			t.Errorf("the API view shows %s", view)
		}
		// Other app and mode cannot see it.
		for _, sc := range []core.Scope{{AppID: "app_2", Mode: "live"}, {AppID: "app_1", Mode: "test"}} {
			if _, err := e.svc.Get(ctx, sc, p.ID); !errors.Is(err, payouts.ErrNotFound) {
				t.Errorf("scope %+v: %v", sc, err)
			}
		}
		// Listing is newest first, filtered and paged.
		time.Sleep(3 * time.Millisecond) // ids sort by creation time to the millisecond
		p2, _ := e.create(nil)
		l, err := e.svc.List(ctx, e.scope, "", "", 10)
		if err != nil || len(l) != 2 || l[0].ID != p2.ID {
			t.Fatalf("list = %+v %v", l, err)
		}
		if l, _ := e.svc.List(ctx, e.scope, "", p2.ID, 10); len(l) != 1 || l[0].ID != p.ID {
			t.Errorf("after cursor = %+v", l)
		}
		if l, _ := e.svc.List(ctx, e.scope, "failed", "", 10); len(l) != 0 {
			t.Errorf("status filter = %+v", l)
		}
	})
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A request that died in transit may have reached the provider. Only an answer
// from the provider can say the payout was refused; a timeout, a cut connection
// or a cancelled context says nothing, and failing the payout then would let a
// caller send it again while the first is paid.
func TestPayoutIsNotFailedByAnAmbiguousTransportError(t *testing.T) {
	cases := map[string]error{
		"deadline":    fmt.Errorf("payu: POST: %w", context.DeadlineExceeded),
		"cancelled":   fmt.Errorf("razorpay: %w", context.Canceled),
		"eof":         &url.Error{Op: "Post", URL: "https://x.example.com", Err: io.ErrUnexpectedEOF},
		"net timeout": &net.OpError{Op: "read", Err: timeoutErr{}},
	}
	for name, cause := range cases {
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			e := newEnv(t, db, []string{"payout"})
			e.fake.Locked(func() {
				e.fake.PayoutFn = func(connector.PayoutRequest) (connector.PayoutSnapshot, error) {
					return connector.PayoutSnapshot{}, cause
				}
			})
			p, _ := e.create(nil)
			e.run()
			if got := e.get(p.ID); got.Status != "pending" {
				t.Errorf("%s: payout is %s after an ambiguous error, want pending (retried)", name, got.Status)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// R-A5 / H-1. Concurrent requests with one Idempotency-Key make one payout and
// all answer with it; the key cannot be reused for another payout.
func TestPayoutIdempotencyKeyMakesOnePayout(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := map[string]bool{}
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := e.create(func(in *payouts.CreateInput) { in.IdempotencyKey = "k-1" })
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				mu.Lock()
				ids[p.ID] = true
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(ids) != 1 || e.count("SELECT COUNT(*) FROM payouts") != 1 {
			t.Fatalf("one key made %d payouts", len(ids))
		}
		_, err := e.create(func(in *payouts.CreateInput) { in.IdempotencyKey = "k-1"; in.Amount = 1 })
		if !errors.Is(err, payouts.ErrKeyReuse) {
			t.Errorf("the key reused for another amount: %v", err)
		}
	})
}
