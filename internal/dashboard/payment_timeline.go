package dashboard

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// glanceView is the row of tiles at the top of the payment page.
type glanceView struct {
	LastFailure, LastFailureTone string
	Tries, TriesTone             string
	RiskText, RiskTone           string
	Webhooks, WebhooksTone       string
	Received                     string
}

// timelineItem is one row of the payment's timeline. System rows stay hidden
// until "All events" is opened.
type timelineItem struct {
	Kind, At, Title, Meta, Tone string
	System                      bool
	Try                         *tryRow
	Refund                      *refundRow
	Webhook                     *webhookGroup

	when time.Time
}

// tryRow is one go at the provider. Fields the provider did not send stay empty.
type tryRow struct {
	Card, Bank, Name, ThreeDS, ThreeDSTone    string
	Declined                                  bool
	FailureCode, Provider, OrderID, PaymentID string
	AttemptID, ProviderRef, Status, Method    string
	TryID                                     string
	MethodMeta                                string
	Duplicate                                 bool
	Risk                                      *riskView
}

// webhookGroup is one event and where it went. SendCount is what "Redeliver to
// all" will send right now.
type webhookGroup struct {
	EventID, EventType string
	Targets            []webhookTarget
	SendCount          int
	// Failing opens the group on the page; Summary stands for it when closed.
	Failing bool
	Summary string
}

type webhookTarget struct {
	DeliveryID, Endpoint, Result, Tone, NextTry, Error string
	New, CanRetry                                      bool

	// history marks a delivery to an endpoint that can no longer be sent to.
	history bool
}

// timelineData is everything the page's timeline and tiles are built from.
type timelineData struct {
	Payment    store.Payment
	Attempts   []store.Attempt
	Tries      []store.ProviderPaymentAttempt
	Ledger     []store.LedgerEntry
	Refunds    []store.Refund
	Events     []store.EventRow
	Deliveries []store.DeliveryListRow
	// Targets holds outbound.ResendTargets of each event, by event ID.
	Targets map[string][]outbound.ResendTarget
	// Provider names provider accounts, by account ID.
	Provider map[string]string
	FmtTime  func(time.Time) string
}

const (
	toneNone = "none"
	tonePass = "pass"
	toneFail = "fail"
)

// allTries is every try in time order, plus a stand-in for each attempt with an
// outcome but no try: a checkout error, or a payment from before tries were kept.
func allTries(in timelineData) []store.ProviderPaymentAttempt {
	out := append([]store.ProviderPaymentAttempt(nil), in.Tries...)
	hasTry := map[string]bool{}
	for _, t := range in.Tries {
		hasTry[t.AttemptID] = true
	}
	for _, a := range in.Attempts {
		if hasTry[a.ID] {
			continue
		}
		switch a.Status {
		case string(state.AttemptFailed), string(state.AttemptCaptured), string(state.AttemptAuthorized):
		default:
			continue
		}
		at := a.UpdatedAt
		if at.IsZero() {
			at = a.CreatedAt
		}
		out = append(out, store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: a.ProviderPaymentID,
			AttemptID: a.ID, ProviderOrderID: a.ProviderOrderID, Status: a.Status, Method: a.Method, FailureCode: a.FailureCode,
			FailureMessage: a.FailureMessage, CreatedAt: at, MethodDetails: a.MethodDetails, RiskDetails: a.RiskDetails})
	}
	// The table keeps some providers' own wording; everything here reads meaning.
	for i := range out {
		out[i].Status = string(state.TryStatus(out[i].Status))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// noTries reports that nobody has tried to pay: a checkout alone is not a try.
func noTries(in timelineData) bool { return len(allTries(in)) == 0 }

// shownFailureCode drops the code ToSnapshot fills in from the status, which
// says nothing beyond "it failed".
func shownFailureCode(code string) string {
	if code == string(state.AttemptFailed) {
		return ""
	}
	return code
}

const noReason = "No reason given"

func tryFailureReason(t store.ProviderPaymentAttempt) string {
	if t.FailureMessage != "" {
		return t.FailureMessage
	}
	if t.ThreeDS == connector.ThreeDSFailed {
		return "3D Secure failed"
	}
	if code := shownFailureCode(t.FailureCode); code != "" {
		return humanWords(code)
	}
	return noReason
}

func humanWords(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func methodLabel(method string) string {
	switch method {
	case "":
		return ""
	case "upi":
		return "UPI"
	case "emi":
		return "EMI"
	}
	return humanWords(method)
}

// tryInstrument reads what was paid with: what we have, or just the method.
func tryInstrument(method, details string) (card, bank, name string) {
	var d connector.PaymentMethodDetails
	if json.Unmarshal([]byte(details), &d) != nil {
		return methodLabel(method), "", ""
	}
	if c := d.Card; c != nil {
		card = humanWords(c.Network)
		if c.Last4 != "" {
			card = strings.TrimSpace(card + " •••• " + c.Last4)
		}
		if card == "" {
			card = "Card"
		}
		return card, c.Issuer, c.Name
	}
	if d.Wallet != "" {
		return d.Wallet, d.Bank, ""
	}
	return methodLabel(method), d.Bank, ""
}

func (in timelineData) providerName(accountID string) string {
	if name := in.Provider[accountID]; name != "" {
		return name
	}
	return accountID
}

func (in timelineData) tryItem(t store.ProviderPaymentAttempt) timelineItem {
	row := &tryRow{FailureCode: shownFailureCode(t.FailureCode), Provider: in.providerName(t.ProviderAccountID),
		OrderID: t.ProviderOrderID, PaymentID: t.ProviderPaymentID, AttemptID: t.AttemptID,
		Risk: tryRiskView(t.RiskDetails, t.Status, t.ThreeDS)}
	row.Card, row.Bank, row.Name = tryInstrument(t.Method, t.MethodDetails)
	row.Status, row.Method, row.TryID = t.Status, t.Method, t.TryID
	_, row.MethodMeta = paymentMethodSummary(store.Attempt{MethodDetails: t.MethodDetails})
	for _, a := range in.Attempts {
		if a.ID == t.AttemptID {
			row.ProviderRef, row.Duplicate = a.ProviderRef, a.Duplicate
		}
	}
	switch t.ThreeDS {
	case connector.ThreeDSPassed:
		row.ThreeDS, row.ThreeDSTone = "3D Secure", tonePass
	case connector.ThreeDSFailed:
		row.ThreeDS, row.ThreeDSTone = "3D Secure", toneFail
	case connector.ThreeDSNotVerified:
		row.ThreeDS, row.ThreeDSTone = "3D Secure not verified", toneNone
	}
	item := timelineItem{Kind: "try", At: in.FmtTime(t.CreatedAt), Try: row, when: t.CreatedAt, Tone: toneNone}
	switch t.Status {
	case string(state.AttemptFailed):
		item.Title, item.Tone = tryFailureReason(t), toneFail
		if item.Title == noReason {
			item.Title = "Payment failed"
		}
		// Only a provider's stated reason makes it a decline: a failure we know
		// nothing about, or an error before the bank, must not read as one.
		row.Declined = t.ThreeDS != connector.ThreeDSFailed && t.FailureCode != "checkout_error" &&
			(t.FailureMessage != "" || row.FailureCode != "")
	case string(state.AttemptCaptured):
		item.Title, item.Tone = "Paid", tonePass
	case string(state.AttemptAuthorized):
		item.Title, item.Tone = "Authorized", tonePass
	default:
		item.Title = "In progress"
	}
	return item
}

// endpointLabel names an endpoint without its query string, which can hold a secret.
func endpointLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host + strings.TrimSuffix(u.Path, "/")
}

func (in timelineData) targetRow(t outbound.ResendTarget) webhookTarget {
	row := webhookTarget{DeliveryID: t.DeliveryID, Endpoint: endpointLabel(t.URL), Error: t.LastError, New: t.New, Tone: toneNone}
	failedBefore := t.LastStatus != 0 || t.LastError != ""
	switch {
	case t.New:
		row.Result = "Not sent"
	case t.InFlight:
		row.Result = "Sending"
	case t.Status == "delivered":
		row.Tone, row.Result = tonePass, statusOr(t.LastStatus, "Delivered")
	case t.Status == "pending" && !failedBefore:
		row.Result = "Queued"
	default:
		row.Tone, row.Result, row.CanRetry = toneFail, statusOr(t.LastStatus, "Failed"), true
		if t.Status == "pending" {
			row.NextTry = in.FmtTime(t.NextAttemptAt)
		}
	}
	return row
}

func statusOr(status int, fallback string) string {
	if status == 0 {
		return fallback
	}
	return fmt.Sprintf("%d", status)
}

// webhookRows lists where an event went: the endpoints it can be sent to now,
// then deliveries to endpoints since disabled or deleted, which stay on record.
func (in timelineData) webhookRows(eventID string) []webhookTarget {
	var rows []webhookTarget
	current := map[string]bool{}
	for _, t := range in.Targets[eventID] {
		current[t.EndpointID] = true
		rows = append(rows, in.targetRow(t))
	}
	for _, d := range in.Deliveries {
		if d.EventID != eventID || current[d.EndpointID] {
			continue
		}
		row := in.targetRow(outbound.ResendTarget{DeliveryID: d.DeliveryID, URL: d.EndpointURL, Status: d.Status, LastStatus: d.LastStatus, LastError: d.LastError})
		row.CanRetry, row.NextTry, row.history = false, "", true
		rows = append(rows, row)
	}
	return rows
}

func (in timelineData) eventItem(ev store.EventRow) timelineItem {
	item := timelineItem{Kind: "event", At: in.FmtTime(ev.CreatedAt), Title: ev.Type, Meta: strings.TrimSpace(ev.ObjectType + " " + ev.ObjectID), Tone: toneNone, System: true, when: ev.CreatedAt}
	rows := in.webhookRows(ev.ID)
	if len(rows) == 0 {
		return item
	}
	group := &webhookGroup{EventID: ev.ID, EventType: ev.Type, Targets: rows}
	for _, t := range in.Targets[ev.ID] {
		if !t.InFlight {
			group.SendCount++
		}
	}
	item.Kind, item.Title, item.Meta, item.System, item.Webhook = "webhook", "Webhook", "", false, group
	var failing, delivered, added, other int
	for _, r := range rows {
		switch {
		case r.New:
			added++
		case r.Tone == toneFail && !r.history:
			failing++
		case r.Tone == tonePass:
			delivered++
		default:
			other++
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		text string
	}{{failing, "failing"}, {delivered, "delivered"}, {added, "not sent"}, {other, "other"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.text))
		}
	}
	group.Summary, group.Failing = strings.Join(parts, " · "), failing > 0
	switch {
	case failing > 0:
		item.Tone = toneFail
	case delivered > 0:
		item.Tone = tonePass
	}
	return item
}

// buildTimeline merges everything that happened to the payment in time order.
func buildTimeline(in timelineData) []timelineItem {
	p := in.Payment
	items := []timelineItem{{Kind: "created", At: in.FmtTime(p.CreatedAt), Title: "Created", Tone: toneNone, when: p.CreatedAt}}
	for _, a := range in.Attempts {
		// We only know a checkout was created, not that anyone opened it, and a
		// subscription's renewal has no checkout at all.
		title := "Checkout started"
		if p.SubscriptionID != "" {
			title = "Charge started"
		}
		items = append(items, timelineItem{Kind: "checkout", At: in.FmtTime(a.CreatedAt), Title: title, Meta: in.providerName(a.ProviderAccountID), Tone: toneNone, when: a.CreatedAt})
	}
	for _, t := range allTries(in) {
		items = append(items, in.tryItem(t))
	}
	for _, rf := range in.Refunds {
		tone := toneNone
		switch rf.Status {
		case string(state.RefundSucceeded):
			tone = tonePass
		case string(state.RefundFailed):
			tone = toneFail
		}
		items = append(items, timelineItem{Kind: "refund", At: in.FmtTime(rf.CreatedAt), Title: "Refund " + fmtMoney(rf.Amount, rf.Currency), Meta: rf.Status, Tone: tone, when: rf.CreatedAt,
			Refund: &refundRow{ID: rf.ID, Amount: fmtMoney(rf.Amount, rf.Currency), Status: rf.Status, Reason: rf.Reason, InitiatedBy: rf.InitiatedBy, Actor: rf.Actor, Created: in.FmtTime(rf.CreatedAt)}})
	}
	for _, ev := range in.Events {
		items = append(items, in.eventItem(ev))
	}
	for _, l := range in.Ledger {
		items = append(items, timelineItem{Kind: "ledger", At: in.FmtTime(l.OccurredAt), Title: humanWords(l.Kind), Meta: fmtMoney(l.Amount, l.Currency), Tone: toneNone, System: true, when: l.OccurredAt})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].when.Before(items[j].when) })
	return items
}

// latestRisk is the risk review of the latest try that has one, or, for
// payments recorded before tries kept risk, of the payment's attempt.
func latestRisk(in timelineData) (*riskView, string) {
	tries := allTries(in)
	for i := len(tries) - 1; i >= 0; i-- {
		if v := tryRiskView(tries[i].RiskDetails, tries[i].Status, tries[i].ThreeDS); v != nil {
			return v, tries[i].ProviderAccountID
		}
	}
	if a, ok := paymentListAttempt(in.Payment, in.Attempts); ok {
		return paymentRiskView(a), a.ProviderAccountID
	}
	return nil, ""
}

// buildRiskPanel is the Risk panel's view: the score belongs to the top tile
// only, and a review with nothing else to list has no panel.
func buildRiskPanel(in timelineData) *riskView {
	v, _ := latestRisk(in)
	if v == nil || len(v.Checks) == 0 && v.Location == "" && v.IPAddress == "" && v.AuthorizationCode == "" {
		return nil
	}
	v.Score = ""
	return v
}

func buildGlance(in timelineData) glanceView {
	p := in.Payment
	g := glanceView{Received: fmtMoney(p.AmountReceived, p.Currency), RiskText: "Not provided", RiskTone: toneNone}
	if v, _ := latestRisk(in); v != nil {
		g.RiskText, g.RiskTone = v.Score, v.Tone
	}

	var failed, paid, waiting int
	lastStatus := ""
	for _, t := range allTries(in) {
		lastStatus = t.Status
		switch t.Status {
		case string(state.AttemptFailed):
			failed++
			g.LastFailure = tryFailureReason(t)
		case string(state.AttemptCaptured):
			paid++
		default:
			waiting++
		}
	}
	g.LastFailureTone = toneNone
	if lastStatus == string(state.AttemptFailed) && p.Status != string(state.PaymentSucceeded) {
		g.LastFailureTone = toneFail
	}
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if paid > 0 {
		parts = append(parts, fmt.Sprintf("%d paid", paid))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", waiting))
	}
	g.Tries, g.TriesTone = strings.Join(parts, ", "), toneNone
	switch {
	case g.Tries == "":
		g.Tries = "None yet"
	case paid > 0:
		g.TriesTone = tonePass
	case failed > 0:
		g.TriesTone = toneFail
	}

	var failing, delivered, sending int
	for _, ev := range in.Events {
		for _, r := range in.webhookRows(ev.ID) {
			switch {
			case r.New:
			case r.Tone == tonePass:
				delivered++
			case r.Tone == toneFail && !r.history:
				failing++
			case r.Tone == toneNone:
				sending++
			}
		}
	}
	switch {
	case failing > 0:
		g.Webhooks, g.WebhooksTone = fmt.Sprintf("%d failing", failing), toneFail
	case sending > 0:
		g.Webhooks, g.WebhooksTone = "Sending", toneNone
	case delivered > 0:
		g.Webhooks, g.WebhooksTone = "All delivered", tonePass
	default:
		g.Webhooks, g.WebhooksTone = "None", toneNone
	}
	return g
}
