package dashboard

import (
	"errors"
	"net/http"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
)

type domainRow struct {
	ID, Host, Status, Token, WellKnown, TXTName, TXTValue, LastCheck, LastError string
	Primary                                                                     bool
}

// whitelistReminder is one provider's note on registering a custom domain.
type whitelistReminder struct{ Provider, Hint string }

// providerWhitelists reminds the operator that payment providers want to
// know (and sometimes review) the websites that take payments. The wording is
// deliberately general: provider dashboards change their menus.
var providerWhitelists = []whitelistReminder{
	{"Razorpay", "Razorpay reviews merchant websites: add the domain to your website details in the Razorpay dashboard and wait for approval."},
	{"PayU", "PayU reviews merchant websites: add the domain in the PayU merchant dashboard's website or integration settings and wait for approval."},
	{"Zoho Payments", "Add the domain as an allowed domain in the developer settings of your Zoho Payments account."},
	{"Whop", "Add the domain to the allowed origins or redirect domains of your Whop app."},
}

type checklistItem struct {
	Provider, Hint string
	Hosts          string
}

func (d *Dashboard) routesDomains() {
	d.mux.HandleFunc("POST "+prefix+"/apps/{id}/domains", d.authed(authz.SettingsWrite, d.recent(d.domainAdd)))
}

func (d *Dashboard) domainRows(ds []store.Domain) ([]domainRow, []checklistItem) {
	var rows []domainRow
	var hosts []string
	for _, dm := range ds {
		txtName, txtValue := core.DomainChallenge(dm)
		rows = append(rows, domainRow{ID: dm.ID, Host: dm.Host, Status: dm.Status, Token: dm.Token, Primary: dm.IsPrimary,
			TXTName: txtName, TXTValue: txtValue, WellKnown: "https://" + dm.Host + "/.well-known/paymux/" + dm.Token, LastCheck: d.fmtTime(dm.LastCheckAt), LastError: dm.LastError})
		hosts = append(hosts, dm.Host)
	}
	var list []checklistItem
	if len(hosts) > 0 {
		for _, p := range providerWhitelists {
			list = append(list, checklistItem{Provider: p.Provider, Hint: p.Hint, Hosts: strings.Join(hosts, ", ")})
		}
	}
	return rows, list
}

func (d *Dashboard) domainAdd(w http.ResponseWriter, r *http.Request, s *session) {
	app, ok := d.loadApp(w, r)
	if !ok {
		return
	}
	_, err := d.o.Core.AddDomain(r.Context(), s.auditActor(), core.OrgScope{OrgID: app.OrgID}, app.ID, r.PostForm.Get("host"))
	switch {
	case err == nil:
		d.appRedirect(w, r, app, "domains")
	case errors.Is(err, core.ErrConflict):
		d.renderApp(w, r, s, app, http.StatusConflict, "domains", err.Error(), nil)
	case errors.Is(err, core.ErrInvalid):
		d.renderApp(w, r, s, app, http.StatusBadRequest, "domains", err.Error(), nil)
	default:
		d.fail(w, err)
	}
}
