package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

// A Connect that Zoho refuses leaves its trace in the error log (the token
// request and the answer), without the client secret, the grant code or any
// token.
func TestOAuthFailuresLeaveNoSecretInTheDatabaseLog(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsOf(t, db)
		red := redact.New()
		rec := dblog.New(dblog.Options{DB: l, Redactor: red})
		rec.Start(ctx)
		t.Cleanup(rec.Close)
		oauthRecorder = func(pa store.ProviderAccount) connector.CallRecorder {
			return rec.ProviderCall(dblog.ProviderMeta{Connector: pa.Connector, AccountID: pa.ID, OrgID: pa.OrgID})
		}
		oauthKnown = func(owner string, v []string) { red.SetKnown(owner, v...) }
		t.Cleanup(func() { oauthRecorder, oauthKnown = nil, nil })
		e := newOAuthEnv(t, db)
		admin := e.roleClient("admin")
		const grant = "1000.GRANTCODE-0123456789abcdef"
		for _, body := range []map[string]any{
			{"error": "invalid_code", "access_token": secretAccess, "client_secret": secretClient, "refresh_token": secretRefresh},
			{"access_token": secretAccess, "scope": "ZohoPay.payments.READ", "token_type": "Bearer"}, // no refresh token
		} {
			e.zoho.mu.Lock()
			e.zoho.body = body
			e.zoho.mu.Unlock()
			state := authStateOf(t, e.connect(admin, url.Values{"oauth_client_secret": {secretClient}}))
			if r, _ := e.callback(admin, "code="+grant+"&state="+url.QueryEscape(state)); r.StatusCode == http.StatusSeeOther {
				t.Fatalf("the exchange should have failed: %d", r.StatusCode)
			}
		}
		rec.Flush(ctx)
		var rows []store.LogRow
		if err := l.Read(ctx, func(q store.Querier) (err error) {
			rows, err = store.SearchLogs(ctx, q, store.LogFilter{WithBodies: true, Limit: 100})
			return
		}); err != nil {
			t.Fatal(err)
		}
		if len(rows) < 2 {
			t.Fatalf("the refused token requests were not recorded: %+v", rows)
		}
		for _, r := range rows {
			all := strings.Join([]string{r.Msg, r.Attrs, r.IDs, r.Request, r.Response, r.Component}, "\n")
			for _, secret := range []string{secretClient, secretRefresh, secretAccess, grant, "GRANTCODE"} {
				if strings.Contains(all, secret) {
					t.Errorf("row %s holds %q:\n%s", r.ID, secret, all)
				}
			}
			if r.Kind != "provider" || r.Connector != "zoho" || !strings.Contains(r.Msg, "/oauth/v2/token") {
				t.Errorf("unexpected row: %+v", r)
			}
		}
	})
}
