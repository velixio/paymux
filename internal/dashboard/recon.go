package dashboard

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/recon"
	"github.com/velixio/paymux/internal/reconcile"
	"github.com/velixio/paymux/internal/store"
)

var reconKinds = []string{recon.KindStatusMismatch, recon.KindAmountMismatch, recon.KindCurrencyMismatch, recon.KindUnknownProviderObject,
	recon.KindRenewalToLink, recon.KindFeeMismatch}

type reconFilter struct{ State, Kind, Account string }

type reconAcct struct {
	ID, Name, Connector, Mode, Status string
	LastRun, LastStatus, LastError    string
	Open                              int
}

type reconRunRow struct {
	ID, Account, Window, Started, Status, Error string
	Checked, Items, Fixed                       int
}

type reconItemRow struct {
	ID, Account, Kind, ProviderID, Ours, Theirs, State, Note, At string
	Resolvable                                                   bool
}

func (d *Dashboard) routesRecon() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/reconciliation", d.authed(authz.SettingsRead, d.reconPage))
	// Resolving an item and running a reconciliation are operator actions.
	m.HandleFunc("POST "+prefix+"/reconciliation/items/{id}/resolve", d.authed(authz.DisputeManage, d.reconResolve))
	m.HandleFunc("POST "+prefix+"/reconciliation/run", d.authed(authz.DisputeManage, d.reconRun))
}

func (d *Dashboard) reconPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderRecon(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderRecon(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	qv := r.URL.Query()
	f := reconFilter{State: qv.Get("state"), Kind: qv.Get("kind"), Account: qv.Get("account")}
	if f.State != "" && f.State != "open" && f.State != "resolved" && f.State != "all" {
		badRequest(w, "state must be open, resolved or all")
		return
	}
	if f.Kind != "" && !slices.Contains(reconKinds, f.Kind) {
		badRequest(w, "unknown kind")
		return
	}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	var (
		accts []store.ProviderAccountInfo
		runs  []store.ReconRun
		items []store.ReconItem
	)
	err = d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if accts, err = store.ListProviderAccounts(ctx, q, org, d.mode(r, s)); err != nil {
			return err
		}
		if runs, err = store.ListReconRuns(ctx, q, org, 30); err != nil {
			return err
		}
		items, err = store.ListReconItems(ctx, q, org, false)
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	name := map[string]string{}
	for _, a := range accts {
		name[a.ID] = a.Name
	}
	if f.Account != "" && name[f.Account] == "" {
		badRequest(w, "unknown provider account")
		return
	}

	v := d.shell(r, s, "reconciliation", "Reconciliation")
	v.Error, v.CanManage, v.ReconFilter, v.ReconKinds = errMsg, s.can(authz.DisputeManage), f, reconKinds
	for _, a := range accts {
		row := reconAcct{ID: a.ID, Name: a.Name, Connector: a.Connector, Mode: a.Mode, Status: a.Status, LastStatus: "never run"}
		for _, run := range runs { // newest first
			if run.ProviderAccountID == a.ID {
				row.LastRun, row.LastStatus, row.LastError = d.fmtTime(run.StartedAt), run.Status, run.Error
				break
			}
		}
		for _, it := range items {
			if it.ProviderAccountID == a.ID && (it.State == "open" || it.State == "to_link") {
				row.Open++
			}
		}
		v.ReconAccounts = append(v.ReconAccounts, row)
	}
	for _, run := range runs {
		if name[run.ProviderAccountID] == "" {
			continue
		}
		v.ReconRuns = append(v.ReconRuns, reconRunRow{ID: run.ID, Account: name[run.ProviderAccountID], Started: d.fmtTime(run.StartedAt),
			Window: d.fmtTime(run.WindowFrom) + " to " + d.fmtTime(run.WindowTo), Status: run.Status, Error: run.Error,
			Checked: run.Checked, Items: run.Items, Fixed: run.Fixed})
	}
	for _, it := range items {
		if name[it.ProviderAccountID] == "" {
			continue
		}
		openItem := it.State == "open" || it.State == "to_link"
		switch f.State {
		case "", "open":
			if !openItem {
				continue
			}
		case "resolved":
			if it.State != "resolved" {
				continue
			}
		}
		if f.Kind != "" && it.Kind != f.Kind || f.Account != "" && it.ProviderAccountID != f.Account {
			continue
		}
		v.ReconItems = append(v.ReconItems, reconItemRow{ID: it.ID, Account: name[it.ProviderAccountID], Kind: it.Kind, ProviderID: it.ProviderID,
			Ours: it.Ours, Theirs: it.Theirs, State: it.State, Note: it.Note, At: d.fmtTime(it.CreatedAt), Resolvable: openItem})
	}
	d.render(w, status, "dash_recon", v)
}

func (d *Dashboard) reconResolve(w http.ResponseWriter, r *http.Request, s *session) {
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	var item store.ReconItem
	err = d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		if item, err = store.GetReconItem(r.Context(), q, r.PathValue("id")); err != nil {
			return err
		}
		pa, err := store.GetProviderAccount(r.Context(), q, item.ProviderAccountID)
		if err != nil {
			return err
		}
		if item.OrgID != scope.OrgID || pa.Mode != d.mode(r, s) {
			return store.ErrNotFound
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	err = d.o.Recon.Resolve(r.Context(), s.auditActor(), scope, r.PathValue("id"), strings.TrimSpace(r.PostForm.Get("note")))
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/reconciliation", http.StatusSeeOther)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, core.ErrInvalid):
		d.renderRecon(w, r, s, http.StatusBadRequest, err.Error())
	default:
		d.fail(w, err)
	}
}

// reconRun queues a reconciliation of one account. The job and its audit row
// are written together.
func (d *Dashboard) reconRun(w http.ResponseWriter, r *http.Request, s *session) {
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	id := r.PostForm.Get("account_id")
	pa, err := d.o.Connectors.Account(r.Context(), scope, id)
	if errors.Is(err, core.ErrNotFound) || err == nil && pa.Mode != d.mode(r, s) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		d.fail(w, err)
		return
	}
	err = d.o.DB.Write(r.Context(), func(tx store.Tx) error {
		if err := d.o.Jobs.Enqueue(r.Context(), tx, reconcile.AccountJob(id)); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Entry{OrgID: scope.OrgID, Actor: s.auditActor(), Action: "recon.run", Target: id, IP: d.clientIP(r)})
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/reconciliation", http.StatusSeeOther)
}
