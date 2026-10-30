package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/selfupdate"
	"github.com/velixio/paymux/internal/store"
)

// Updater installs a newer release of the hub (*selfupdate.Manager).
type Updater interface {
	Info(ctx context.Context, refresh bool) selfupdate.Info
	Start(target string, force bool, done func(selfupdate.Status)) error
	Status() selfupdate.Status
}

// updateView is the data of the Update page.
type updateView struct {
	Info      selfupdate.Info
	Status    selfupdate.Status
	State     string
	Running   bool // an install is under way: the page follows it
	Checked   string
	Offer     bool // an update to Info.Latest can be started
	NeedsTick bool // the release is not newer: installing it needs "force"
}

func (d *Dashboard) updateRoutes() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/update", d.authed(authz.SystemUpdate, d.updatePage))
	m.HandleFunc("GET "+prefix+"/update/status", d.authed(authz.SystemUpdate, d.updateStatus))
	m.HandleFunc("POST "+prefix+"/update", d.authed(authz.SystemUpdate, d.recent(d.updateStart)))
}

func (d *Dashboard) updatePage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderUpdate(w, r, s, http.StatusOK, "", r.URL.Query().Get("refresh") != "")
}

func (d *Dashboard) renderUpdate(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string, refresh bool) {
	if d.o.Updater == nil {
		http.NotFound(w, r)
		return
	}
	info := d.o.Updater.Info(r.Context(), refresh)
	st := d.o.Updater.Status()
	uv := &updateView{Info: info, Status: st, State: string(st.State),
		Running: st.State == selfupdate.StateRunning || st.State == selfupdate.StateRestarting}
	if !info.CheckedAt.IsZero() {
		uv.Checked = d.fmtTime(info.CheckedAt)
	}
	uv.Offer = info.Latest != "" && info.Blocked == "" && !uv.Running
	uv.NeedsTick = uv.Offer && !info.Newer
	v := d.shell(r, s, "update", "Update")
	v.Update, v.Error = uv, errMsg
	d.render(w, status, "dash_update", v)
}

// updateStatus is polled by the page while an install runs.
func (d *Dashboard) updateStatus(w http.ResponseWriter, r *http.Request, s *session) {
	if d.o.Updater == nil {
		http.NotFound(w, r)
		return
	}
	st := d.o.Updater.Status()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"state": string(st.State), "from": st.From, "to": st.To, "log": st.Log, "error": st.Error})
}

// updateStart starts the install of the release the page offered. It returns at
// once: the install runs in the background and the page follows it.
func (d *Dashboard) updateStart(w http.ResponseWriter, r *http.Request, s *session) {
	if d.o.Updater == nil {
		http.NotFound(w, r)
		return
	}
	target, force := r.PostForm.Get("target"), r.PostForm.Get("force") != ""
	info := d.o.Updater.Info(r.Context(), false)
	if target == "" || target != info.Latest {
		d.renderUpdate(w, r, s, http.StatusBadRequest, "The latest release is "+orNone(info.Latest)+", not what the page showed. Check the version below and try again.", false)
		return
	}
	from := info.Current
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	actor, ip := s.auditActor(), d.clientIP(r)
	write := func(action string, after map[string]any) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := d.o.DB.Write(ctx, func(tx store.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{OrgID: org, Actor: actor, Action: action, Target: target, After: after, IP: ip})
		}); err != nil {
			d.o.Logger.Error("dashboard: audit of an update", "action", action, "err", err)
		}
	}
	err = d.o.Updater.Start(target, force, func(st selfupdate.Status) {
		result := "ok"
		if st.State == selfupdate.StateFailed {
			result = "failed"
		}
		write("system.update", map[string]any{"from": from, "to": target, "force": force, "result": result, "error": st.Error})
	})
	switch {
	case err == nil:
		write("system.update.start", map[string]any{"from": from, "to": target, "force": force})
		http.Redirect(w, r, prefix+"/update", http.StatusSeeOther)
	case errors.Is(err, selfupdate.ErrBusy):
		d.renderUpdate(w, r, s, http.StatusConflict, err.Error(), false)
	default:
		d.renderUpdate(w, r, s, http.StatusBadRequest, err.Error(), false)
	}
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
