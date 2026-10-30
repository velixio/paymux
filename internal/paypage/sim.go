package paypage

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/velixio/paymux/connectors/simulator"
	"github.com/velixio/paymux/internal/web"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// RegisterSim mounts the simulator's hosted checkout page, /sim/{ref}. It is
// separate from Register because it belongs to the base host only: custom
// domains serve the customer routes and nothing else.
func (h *Handler) RegisterSim(m Mux) {
	m.HandleFunc("GET /sim/{ref}", withLook(h.simPage))
	m.HandleFunc("POST /sim/{ref}", withLook(h.simDecide))
}

// simTarget finds the simulator controller of the provider account named in
// the query. Only a test-mode simulator account answers; anything else is a
// plain 404, so the page cannot be used to poke at another connector.
func (h *Handler) simTarget(w http.ResponseWriter, r *http.Request) (simulator.Controller, string, string, bool) {
	ref, pa := r.PathValue("ref"), r.URL.Query().Get("pa")
	if ref == "" || pa == "" {
		h.notFound(w)
		return nil, "", "", false
	}
	live, err := h.o.Flow.LiveConnector(r.Context(), pa)
	if err != nil {
		h.notFound(w)
		return nil, "", "", false
	}
	ctl, ok := live.Connector.(simulator.Controller)
	if !ok || live.Account.Mode != "test" {
		h.notFound(w)
		return nil, "", "", false
	}
	return ctl, ref, pa, true
}

func (h *Handler) simPage(w http.ResponseWriter, r *http.Request) {
	ctl, ref, pa, ok := h.simTarget(w, r)
	if !ok {
		return
	}
	st, err := ctl.State(r.Context(), ref)
	if errors.Is(err, connector.ErrNotFound) {
		h.notFound(w)
		return
	}
	if err != nil {
		h.serverError(w, "", err)
		return
	}
	amount, err := money.FormatDecimal(st.Amount, st.Currency)
	if err != nil {
		amount = strconv.FormatInt(st.Amount, 10)
	}
	kind := map[string]string{"payment": "Payment", "mandate_setup": "Mandate authorisation (UPI AutoPay style)", "sub_checkout": "Subscription authorisation"}[st.Kind]
	sp := &web.SimPage{Kind: kind, Status: st.Status, Currency: st.Currency, Amount: amount, Late: st.Late,
		Action:    "/sim/" + url.PathEscape(ref) + "?pa=" + url.QueryEscape(pa),
		ReturnURL: "/return/" + url.PathEscape(pa) + "/" + url.PathEscape(ref)}
	switch st.Status {
	case "created":
		sp.Open = true
	case "authorized":
		sp.Authorized = true
	case "pending":
		sp.Waiting = true
	default:
		sp.Done = true
	}
	h.page(w, http.StatusOK, "sim_checkout", web.PayPage{Sim: sp})
}

// simDelay reads a delay in seconds from the form, with a default.
func simDelay(r *http.Request, def time.Duration) (time.Duration, bool) {
	raw := r.PostForm.Get("delay")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 3600 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func (h *Handler) simDecide(w http.ResponseWriter, r *http.Request) {
	ctl, ref, pa, ok := h.simTarget(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // three small fields; Go's own default is 10 MiB
	if err := r.ParseForm(); err != nil {
		h.result(w, http.StatusBadRequest, "", "result.link_invalid.title", "result.link_invalid.msg", "")
		return
	}
	action := simulator.Action(r.PostForm.Get("action"))
	def := 10 * time.Second
	if action == simulator.ActionLateSucceed {
		def = 90 * time.Second
	}
	delay, okDelay := simDelay(r, def)
	switch action {
	case simulator.ActionSucceed, simulator.ActionFail, simulator.ActionPendingSucceed, simulator.ActionAuthorize,
		simulator.ActionLateSucceed, simulator.ActionCapture, simulator.ActionCancel:
	default:
		okDelay = false
	}
	if !okDelay {
		h.result(w, http.StatusBadRequest, "", "result.link_invalid.title", "result.link_invalid.msg", "")
		return
	}
	switch err := ctl.Decide(r.Context(), ref, action, delay); {
	case errors.Is(err, connector.ErrNotFound):
		h.notFound(w)
		return
	case errors.Is(err, simulator.ErrAlreadyDecided):
		h.page(w, http.StatusConflict, "pay_result", web.PayPage{Title: "Already decided", Message: err.Error(),
			ReturnURL: "/return/" + url.PathEscape(pa) + "/" + url.PathEscape(ref)})
		return
	case err != nil:
		h.serverError(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if action == simulator.ActionAuthorize {
		http.Redirect(w, r, "/sim/"+url.PathEscape(ref)+"?pa="+url.QueryEscape(pa), http.StatusSeeOther)
		return
	}
	// Back to the merchant through the normal return path, which fetches the
	// provider's truth like it does for any provider.
	http.Redirect(w, r, "/return/"+url.PathEscape(pa)+"/"+url.PathEscape(ref), http.StatusSeeOther)
}
