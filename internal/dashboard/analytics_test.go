package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestAnalyticsPageSeparatesModesAndRanksCountries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		viewer := e.roleClient("viewer")
		app := e.seedApp("analytics")
		seed := func(id, mode, status, country string, amount int64) {
			p := store.Payment{ID: id, OrgID: store.DefaultOrgID, AppID: app.ID, Mode: mode, Kind: "one_time", Amount: amount,
				Currency: "USD", Country: country, Status: status, Metadata: map[string]string{}, ClientSecretHash: "h",
				ExpiresAt: e.d15(10).AddDate(0, 0, 1), Version: 1, CreatedAt: e.d15(10), UpdatedAt: e.d15(10)}
			if status == "succeeded" {
				p.AmountReceived = amount
			}
			if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertPayment(ctx, tx, p) }); err != nil {
				t.Fatal(err)
			}
		}
		seed("paid_us", "live", "succeeded", "US", 2500)
		seed("paid_in", "live", "succeeded", "IN", 1000)
		seed("failed_us", "live", "failed", "US", 500)
		seed("test_only", "test", "succeeded", "DE", 9999)
		if err := db.Write(ctx, func(tx store.Tx) error {
			at := e.d15(10)
			if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_analytics", OrgID: store.DefaultOrgID,
				Mode: "live", Connector: "simulator", Name: "Analytics provider", Status: "active", CreatedAt: at}); err != nil {
				return err
			}
			for _, a := range []store.Attempt{
				{ID: "att_paid", OrgID: store.DefaultOrgID, Mode: "live", PaymentID: "paid_us", ProviderAccountID: "pa_analytics", ProviderRef: "provider_paid", Status: "captured", Method: "card", CreatedAt: at, UpdatedAt: at},
				{ID: "att_failed", OrgID: store.DefaultOrgID, Mode: "live", PaymentID: "failed_us", ProviderAccountID: "pa_analytics", ProviderRef: "provider_failed", Status: "failed", Method: "card", FailureCode: "declined", FailureMessage: "Card declined", CreatedAt: at, UpdatedAt: at},
			} {
				if err := store.InsertAttempt(ctx, tx, a); err != nil {
					return err
				}
			}
			for _, attempt := range []store.ProviderPaymentAttempt{
				{ProviderAccountID: "pa_analytics", ProviderPaymentID: "provider_retry_failed", AttemptID: "att_paid", Status: "failed", Method: "card", FailureCode: "retry_declined", FailureMessage: "First retry declined", CreatedAt: at, UpdatedAt: at},
				{ProviderAccountID: "pa_analytics", ProviderPaymentID: "provider_retry_paid", AttemptID: "att_paid", Status: "captured", Method: "card", CreatedAt: at.Add(time.Minute), UpdatedAt: at.Add(time.Minute)},
			} {
				if err := store.UpsertProviderPaymentAttempt(ctx, tx, attempt); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		body := e.pageOK(viewer, "/dashboard/analytics?range=7d&currency=USD")
		for _, want := range []string{"Payments intelligence", "Country ranking", "Provider performance", "Top failure reasons", "US", "IN", "35.00 USD", "Payments", "simulator", "CARD", "declined", "Card declined", "retry_declined", "First retry declined"} {
			if !strings.Contains(body, want) {
				t.Errorf("analytics page missing %q", want)
			}
		}
		if strings.Contains(body, ">DE<") || strings.Contains(body, "99.99 USD") {
			t.Error("live analytics leaked test-mode payment")
		}
		if got := strings.Count(body, `class="analytics-chart-row"`); got != 7 {
			t.Errorf("7-day chart has %d buckets, want 7", got)
		}
		filtered := e.pageOK(viewer, "/dashboard/analytics?range=7d&currency=USD&country=US")
		if strings.Contains(filtered, ">10.00 USD<") || !strings.Contains(filtered, ">25.00 USD<") {
			t.Error("country filter did not isolate US payments")
		}

		resp, csv := e.get(viewer, "/dashboard/analytics/countries.csv?range=7d&currency=USD")
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(csv, "US,2,1,1") || strings.Contains(csv, "DE,") {
			t.Fatalf("unexpected country CSV (%d):\n%s", resp.StatusCode, csv)
		}
		overview := e.pageOK(viewer, "/dashboard/overview?range=7d&currency=USD")
		if !strings.Contains(overview, "Payment outcomes") || !strings.Contains(overview, "Top countries") {
			t.Error("overview is missing analytics summary")
		}
	})
}

func TestAnalyticsRejectsInvalidCountry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		resp, _ := e.get(e.roleClient("viewer"), "/dashboard/analytics?country=USA")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
	})
}

func TestAnalyticsCSVSafe(t *testing.T) {
	for _, value := range []string{"=cmd", "+cmd", "-cmd", "@cmd", "\tcmd", "\rcmd"} {
		if got := analyticsCSVSafe(value); got != "'"+value {
			t.Errorf("analyticsCSVSafe(%q) = %q", value, got)
		}
	}
	for _, value := range []string{"US", "Unknown", ""} {
		if got := analyticsCSVSafe(value); got != value {
			t.Errorf("analyticsCSVSafe(%q) = %q", value, got)
		}
	}
}
