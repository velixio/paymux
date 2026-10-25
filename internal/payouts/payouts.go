// Package payouts sends money out to a beneficiary through a connector that
// implements connector.Payouter. A payout is recorded first, then submitted by
// a job (retried with the same idempotency token, so a timeout never pays
// twice), then followed until the provider says it arrived or failed.
//
// Payouts are deliberately hard to reach by accident: the API needs the
// payout.create action and the explicit payouts:write key scope, an account
// must opt in with the "payout" purpose, and the beneficiary is stored sealed.
//
// A connector that can tell Paymux of payout changes sends them as payout
// notifications (connector.NotifyPayout); like every webhook they are only a
// hint: the payout is fetched, checked against ours, and that is applied. A
// payout is also followed by polling, which is the only source for a provider
// that sends no webhook.
package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logctx"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Job kinds.
const (
	JobSubmit = "payout.submit"
	JobPoll   = "payout.poll"
)

// Statuses (the connector's, plus needs_review: our own).
const (
	StatusPending    = string(connector.PayoutPending)
	StatusProcessing = string(connector.PayoutProcessing)
	StatusSucceeded  = string(connector.PayoutSucceeded)
	StatusFailed     = string(connector.PayoutFailed)
	StatusReversed   = string(connector.PayoutReversed)
	// StatusNeedsReview is a payout whose submission ended in an unknown
	// outcome: the provider may or may not have it. It is polled by its
	// reference until the provider answers, and an operator is alerted.
	StatusNeedsReview = "needs_review"
)

// rawSynthesised is the raw status of a failure we concluded ourselves (the
// provider's answer to the submit was an error we read as a refusal). The
// provider has not confirmed it, so it is not final: see apply.
const rawSynthesised = "paymux_rejected"

// submitMaxAttempts is how many ambiguous submit attempts a payout gets before
// it goes to needs_review.
const submitMaxAttempts = 10

// Event types.
const (
	EvCreated    = "payout.created"
	EvProcessing = "payout.processing"
	EvSucceeded  = "payout.succeeded"
	EvFailed     = "payout.failed"
	EvReversed   = "payout.reversed"
	// EvNeedsReview is sent when a payout's outcome is unknown after the
	// submit attempts ran out.
	EvNeedsReview = "payout.needs_review"
)

// Error kinds. Errors returned by Service wrap exactly one of these.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrNoRoute  = errors.New("no route")
	// ErrNeedsReview refuses a payout to a beneficiary and amount of which
	// one is in needs_review: it may have been paid already.
	ErrNeedsReview = errors.New("a payout awaits review")
)

// Error is a typed domain error.
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func newErr(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

// Connectors gives access to the connector of a provider account.
type Connectors interface {
	Get(ctx context.Context, providerAccountID string) (connector.Connector, error)
}

// ConnectorsFunc adapts a function to Connectors.
type ConnectorsFunc func(ctx context.Context, providerAccountID string) (connector.Connector, error)

// Get calls f.
func (f ConnectorsFunc) Get(ctx context.Context, id string) (connector.Connector, error) {
	return f(ctx, id)
}

// Options configures the Service.
type Options struct {
	DB         *store.DB
	Box        *secrets.Box
	Connectors Connectors
	Jobs       *jobs.Queue
	Emitter    events.Emitter
	Logger     *slog.Logger
	Now        func() time.Time
	// Alerts receives ops alerts (a payout in needs_review); optional.
	Alerts Alerter
}

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// Service is the payouts service.
type Service struct {
	o Options
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Emitter == nil {
		o.Emitter = events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })
	}
	return &Service{o: o}
}

// RegisterJobs installs the job handlers.
func (s *Service) RegisterJobs(q *jobs.Queue) {
	q.Register(JobSubmit, s.handleSubmit)
	q.Register(JobPoll, s.handlePoll)
}

func (s *Service) now() time.Time { return s.o.Now().UTC() }

// CreateInput is a payout request.
type CreateInput struct {
	Scope       core.Scope
	Amount      int64
	Currency    string
	Beneficiary connector.Beneficiary
	Narration   string
	Metadata    map[string]string
	// ProviderAccountID picks the account; empty means the first of the app's
	// accounts that accepts payouts in the currency.
	ProviderAccountID string
	// Force sends the payout although one to the same beneficiary and amount
	// is in needs_review (it may have been paid). It is audited and needs an
	// Actor.
	Force bool
	Actor audit.Actor
}

const maxAmount = int64(1) << 50

var (
	ifscRE    = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`)
	accountRE = regexp.MustCompile(`^[0-9]{6,20}$`)
	upiRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}@[A-Za-z][A-Za-z0-9]{1,31}$`)
	currencyR = regexp.MustCompile(`^[A-Z]{3}$`)
)

func validate(in *CreateInput) error {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Amount <= 0 || in.Amount > maxAmount {
		return newErr(ErrInvalid, "amount must be a positive integer in minor units")
	}
	if !currencyR.MatchString(in.Currency) {
		return newErr(ErrInvalid, "currency must be a 3-letter ISO 4217 code")
	}
	if _, err := money.Exponent(in.Currency); err != nil {
		return newErr(ErrInvalid, "unsupported currency %s", in.Currency)
	}
	b := &in.Beneficiary
	b.Name, b.UPI, b.IFSC = strings.TrimSpace(b.Name), strings.TrimSpace(b.UPI), strings.ToUpper(strings.TrimSpace(b.IFSC))
	b.AccountNumber = strings.TrimSpace(b.AccountNumber)
	if b.Name == "" || len(b.Name) > 100 {
		return newErr(ErrInvalid, "beneficiary.name is required (at most 100 characters)")
	}
	bank := b.AccountNumber != "" || b.IFSC != ""
	switch {
	case bank && b.UPI != "":
		return newErr(ErrInvalid, "give either a bank account (account_number and ifsc) or a upi id, not both")
	case bank:
		if !accountRE.MatchString(b.AccountNumber) || !ifscRE.MatchString(b.IFSC) {
			return newErr(ErrInvalid, "beneficiary needs a numeric account_number (6-20 digits) and a valid ifsc")
		}
	case b.UPI != "":
		if !upiRE.MatchString(b.UPI) {
			return newErr(ErrInvalid, "beneficiary.upi is not a valid UPI id")
		}
	default:
		return newErr(ErrInvalid, "beneficiary needs a bank account (account_number and ifsc) or a upi id")
	}
	if len(in.Narration) > 100 {
		return newErr(ErrInvalid, "narration is at most 100 characters")
	}
	if len(in.Metadata) > 20 {
		return newErr(ErrInvalid, "metadata has at most 20 keys")
	}
	for k, v := range in.Metadata {
		if k == "" || len(k) > 40 || len(v) > 500 {
			return newErr(ErrInvalid, "metadata keys are 1-40 characters and values at most 500")
		}
	}
	return nil
}

// Label is the masked beneficiary shown in the API, the dashboard and logs.
func Label(b connector.Beneficiary) string {
	switch {
	case b.UPI != "":
		user, handle, _ := strings.Cut(b.UPI, "@")
		return user[:min(2, len(user))] + "•••@" + handle
	case len(b.AccountNumber) > 4:
		return "••••" + b.AccountNumber[len(b.AccountNumber)-4:] + " " + b.IFSC[:min(4, len(b.IFSC))]
	}
	return "••••"
}

func aad(payoutID string) []byte { return secrets.AADBeneficiary(payoutID) }

// pickAccount chooses the provider account for a payout.
func (s *Service) pickAccount(ctx context.Context, in CreateInput) (string, error) {
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		accounts, err = store.ListAppProviderAccounts(ctx, q, in.Scope.AppID, in.Scope.Mode)
		return
	}); err != nil {
		return "", err
	}
	for _, pa := range accounts {
		if pa.Status != "active" || !slices.Contains(pa.EnabledPurposes, "payout") {
			continue
		}
		if in.ProviderAccountID != "" && pa.ID != in.ProviderAccountID {
			continue
		}
		if len(pa.AcceptedCurrencies) > 0 && !slices.Contains(pa.AcceptedCurrencies, in.Currency) {
			continue
		}
		conn, err := s.o.Connectors.Get(ctx, pa.ID)
		if err != nil {
			s.o.Logger.Warn("payouts: connector unavailable", "provider_account", pa.ID, "err", err)
			continue
		}
		if _, ok := conn.(connector.Payouter); ok {
			return pa.ID, nil
		}
	}
	return "", newErr(ErrNoRoute, "no provider account of this app accepts payouts in %s", in.Currency)
}

// Create records a payout and queues its submission.
func (s *Service) Create(ctx context.Context, in CreateInput) (store.Payout, error) {
	if err := validate(&in); err != nil {
		return store.Payout{}, err
	}
	accountID, err := s.pickAccount(ctx, in)
	if err != nil {
		return store.Payout{}, err
	}
	var app store.App
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { app, err = store.GetApp(ctx, q, in.Scope.AppID); return }); err != nil {
		return store.Payout{}, newErr(ErrNotFound, "app not found")
	}
	reviewing, err := s.inReview(ctx, in, app.ID)
	if err != nil {
		return store.Payout{}, err
	}
	if reviewing != "" && !in.Force {
		return store.Payout{}, newErr(ErrNeedsReview, "payout %s to the same beneficiary and amount is awaiting review and may have been paid; resolve it, or send with force", reviewing)
	}
	if reviewing != "" && in.Actor.Type == "" {
		return store.Payout{}, newErr(ErrInvalid, "force needs an actor")
	}
	id := ids.New("po")
	raw, _ := json.Marshal(in.Beneficiary)
	sealed, err := s.o.Box.Seal(raw, aad(id))
	if err != nil {
		return store.Payout{}, err
	}
	now := s.now()
	p := store.Payout{ID: id, OrgID: app.OrgID, Mode: in.Scope.Mode, AppID: app.ID, ProviderAccountID: accountID,
		ProviderRef: ids.ProviderRef(), Amount: in.Amount, Currency: in.Currency, Status: StatusPending, BeneficiaryEnc: sealed,
		BeneficiaryLabel: Label(in.Beneficiary), Narration: strings.TrimSpace(in.Narration), Metadata: in.Metadata, Version: 1,
		CreatedAt: now, UpdatedAt: now}
	err = s.o.DB.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertPayout(ctx, tx, p); err != nil {
			return err
		}
		if err := s.o.Jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobSubmit, Payload: payload{PayoutID: p.ID}, DedupeKey: "payout:" + p.ID + ":submit",
			MaxAttempts: submitMaxAttempts + 1}); err != nil { // the +1 leaves the last attempt to the needs_review move
			return err
		}
		if reviewing != "" {
			if err := audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: in.Actor, Action: "payout.force_create", Target: p.ID,
				After: map[string]any{"amount": p.Amount, "currency": p.Currency, "beneficiary": p.BeneficiaryLabel, "while_in_review": reviewing}}); err != nil {
				return err
			}
		}
		return s.emit(ctx, tx, EvCreated, p)
	})
	return p, err
}

// inReview returns the id of a needs_review payout of the app with the same
// destination, amount and currency as in, or "".
func (s *Service) inReview(ctx context.Context, in CreateInput, appID string) (string, error) {
	var rows []store.Payout
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListPayouts(ctx, q, appID, in.Scope.Mode, StatusNeedsReview, "", 200)
		return
	})
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Amount != in.Amount || r.Currency != in.Currency {
			continue
		}
		plain, err := s.o.Box.Open(r.BeneficiaryEnc, aad(r.ID))
		if err != nil {
			return "", err
		}
		var b connector.Beneficiary
		if err := json.Unmarshal(plain, &b); err != nil {
			return "", err
		}
		if b.AccountNumber == in.Beneficiary.AccountNumber && b.IFSC == in.Beneficiary.IFSC && b.UPI == in.Beneficiary.UPI {
			return r.ID, nil
		}
	}
	return "", nil
}

// Get returns a payout of the scope's app and mode.
func (s *Service) Get(ctx context.Context, sc core.Scope, id string) (store.Payout, error) {
	var p store.Payout
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayout(ctx, q, id); return })
	if err != nil || p.AppID != sc.AppID || p.Mode != sc.Mode {
		return store.Payout{}, newErr(ErrNotFound, "payout not found")
	}
	return p, nil
}

// List returns the scope's payouts, newest first.
func (s *Service) List(ctx context.Context, sc core.Scope, status, after string, limit int) ([]store.Payout, error) {
	var out []store.Payout
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.ListPayouts(ctx, q, sc.AppID, sc.Mode, status, after, limit)
		return
	})
	return out, err
}

// ---- views and events ----

// View is the public JSON shape of a payout.
type View struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Livemode      bool              `json:"livemode"` // false for a test-mode object
	Status        string            `json:"status"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Beneficiary   BeneficiaryView   `json:"beneficiary"`
	Narration     string            `json:"narration,omitempty"`
	Metadata      map[string]string `json:"metadata"`
	FailureCode   string            `json:"failure_code,omitempty"`
	FailureReason string            `json:"failure_message,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	Version       int64             `json:"version"`
}

// BeneficiaryView is the beneficiary as the API shows it: only a masked label.
type BeneficiaryView struct {
	Label string `json:"label"`
}

// ViewPayout renders p. The beneficiary's name is inside the sealed blob, so
// the view carries the masked label only.
func ViewPayout(p store.Payout) View {
	meta := p.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	v := View{ID: p.ID, Object: "payout", Livemode: p.Mode == "live", Status: p.Status, Amount: p.Amount, Currency: p.Currency,
		Beneficiary: BeneficiaryView{Label: p.BeneficiaryLabel}, Narration: p.Narration, Metadata: meta, FailureCode: p.FailureCode,
		FailureReason: p.FailureMessage, CreatedAt: p.CreatedAt, Version: p.Version}
	if !p.CompletedAt.IsZero() {
		t := p.CompletedAt
		v.CompletedAt = &t
	}
	return v
}

func (s *Service) emit(ctx context.Context, tx store.Tx, typ string, p store.Payout) error {
	return s.o.Emitter.Emit(ctx, tx, events.Event{OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode, Type: typ, ObjectType: "payout",
		ObjectID: p.ID, ObjectVersion: p.Version, Data: ViewPayout(p)})
}

// ---- jobs ----

type payload struct {
	PayoutID string `json:"payout_id"`
	N        int    `json:"n,omitempty"`
}

func (s *Service) payouter(ctx context.Context, accountID string) (connector.Payouter, error) {
	conn, err := s.o.Connectors.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	po, ok := conn.(connector.Payouter)
	if !ok {
		return nil, errors.New("the provider account's connector cannot pay out")
	}
	return po, nil
}

func (s *Service) handleSubmit(ctx context.Context, raw json.RawMessage) error {
	var pl payload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var p store.Payout
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayout(ctx, q, pl.PayoutID); return }); err != nil {
		return jobs.Permanent(err)
	}
	if p.Status != StatusPending {
		return nil // submitted already
	}
	logctx.Note(ctx, "payout_id", p.ID, "provider_account_id", p.ProviderAccountID, "app_id", p.AppID, "org_id", p.OrgID)
	var ben connector.Beneficiary
	plain, err := s.o.Box.Open(p.BeneficiaryEnc, aad(p.ID))
	if err != nil {
		return jobs.Permanent(fmt.Errorf("opening the beneficiary of %s: %w", p.ID, err))
	}
	if err := json.Unmarshal(plain, &ben); err != nil {
		return jobs.Permanent(err)
	}
	po, err := s.payouter(ctx, p.ProviderAccountID)
	if err != nil {
		return err // retried: the account may be fixed meanwhile
	}
	// The same ProviderRef every time: the provider treats a repeat as the same payout.
	snap, err := po.CreatePayout(ctx, connector.PayoutRequest{ProviderRef: p.ProviderRef, Amount: p.Amount, Currency: p.Currency,
		Beneficiary: ben, Narration: p.Narration})
	switch {
	case err == nil:
		return s.apply(ctx, p.ID, snap)
	case errors.Is(err, connector.ErrTemporary) || isRateLimited(err) || isAmbiguous(err):
		if n, cerr := s.countAttempt(ctx, p.ID); cerr == nil && n >= submitMaxAttempts {
			// The provider may have the payout and we cannot tell: stop sending,
			// ask for it by its reference, and tell an operator.
			return s.needsReview(ctx, p.ID, err)
		}
		return err // retried by the queue with the same reference
	default:
		// The provider refused it (bad beneficiary, over a limit, bad credentials):
		// nothing was sent, and repeating the request will not change that.
		return s.apply(ctx, p.ID, connector.PayoutSnapshot{ProviderRef: p.ProviderRef, Status: connector.PayoutFailed,
			RawStatus: rawSynthesised, ErrorCode: "rejected", ErrorMessage: truncate(err.Error(), 300)})
	}
}

// isAmbiguous says whether err is a transport failure that tells nothing of
// what the provider did: the request may have been received and acted on.
// Only an answer from the provider can mark a payout refused.
func isAmbiguous(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &ne)
}

func isRateLimited(err error) bool {
	var rl connector.ErrRateLimited
	return errors.As(err, &rl)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// countAttempt records one more submit attempt and returns the count.
func (s *Service) countAttempt(ctx context.Context, id string) (n int, err error) {
	err = s.o.DB.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPayoutForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		p.SubmitAttempts++
		n = p.SubmitAttempts
		return store.UpdatePayout(ctx, tx, &p, s.now())
	})
	return n, err
}

// needsReview moves a pending payout whose submit attempts ran out on
// ambiguous errors to needs_review, starts polling it by its reference and
// raises an ops alert. cause is the last error.
func (s *Service) needsReview(ctx context.Context, id string, cause error) error {
	var moved store.Payout
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPayoutForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Status != StatusPending {
			return nil
		}
		p.Status, p.RawStatus = StatusNeedsReview, StatusNeedsReview
		p.FailureMessage = truncate("outcome unknown after "+fmt.Sprint(p.SubmitAttempts)+" attempts: "+cause.Error(), 300)
		if err := store.UpdatePayout(ctx, tx, &p, s.now()); err != nil {
			return err
		}
		moved = p
		if err := s.emit(ctx, tx, EvNeedsReview, p); err != nil {
			return err
		}
		return s.enqueuePoll(ctx, tx, p.ID, 0)
	})
	if err != nil || moved.ID == "" {
		return err
	}
	if s.o.Alerts != nil {
		_ = s.o.Alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityCritical, Kind: "payout.needs_review",
			Message: fmt.Sprintf("Payout %s (%d %s to %s) could not be confirmed with the provider after %d attempts; it may or may not have been paid. It is polled by its reference; check the provider dashboard before sending it again.",
				moved.ID, moved.Amount, moved.Currency, moved.BeneficiaryLabel, moved.SubmitAttempts),
			Refs: map[string]string{"payout": moved.ID, "provider_account": moved.ProviderAccountID, "app": moved.AppID}})
	}
	return nil
}

// pollLadder is the wait before each status check: 1m, 2m, 5m, 15m, 1h, 6h,
// then daily.
var pollLadder = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

const pollGiveUp = 14 * 24 * time.Hour

func (s *Service) handlePoll(ctx context.Context, raw json.RawMessage) error {
	var pl payload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var p store.Payout
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayout(ctx, q, pl.PayoutID); return }); err != nil {
		return jobs.Permanent(err)
	}
	// A succeeded payout is checked once more for a reversal; one in
	// needs_review, or failed by our own reading, until the provider answers.
	if p.Status != StatusProcessing && p.Status != StatusSucceeded && p.Status != StatusNeedsReview && !synthesisedFailure(p) {
		return nil
	}
	logctx.Note(ctx, "payout_id", p.ID, "provider_account_id", p.ProviderAccountID, "app_id", p.AppID, "org_id", p.OrgID)
	po, err := s.payouter(ctx, p.ProviderAccountID)
	if err != nil {
		return err
	}
	snap, err := po.FetchPayout(ctx, connector.PayoutRef{ProviderRef: p.ProviderRef, ProviderPayoutID: p.ProviderPayoutID})
	if err != nil {
		// A payout in needs_review is polled for as long as it stays there.
		if errors.Is(err, connector.ErrNotFound) && (p.Status == StatusNeedsReview || s.now().Sub(p.CreatedAt) < pollGiveUp) {
			return s.nextPoll(ctx, p.ID, pl.N)
		}
		if errors.Is(err, connector.ErrNotFound) && synthesisedFailure(p) {
			return nil // the provider never had it: our reading stands
		}
		return err
	}
	if err := payoutIsOurs(p, snap); err != nil {
		return jobs.Permanent(err) // not ours: polling again cannot fix that; the payout stays as it is
	}
	return s.apply(ctx, p.ID, snap)
}

func (s *Service) nextPoll(ctx context.Context, id string, n int) error {
	return s.o.DB.Write(ctx, func(tx store.Tx) error { return s.enqueuePoll(ctx, tx, id, n+1) })
}

func (s *Service) enqueuePoll(ctx context.Context, tx store.Tx, id string, n int) error {
	d := pollLadder[min(n, len(pollLadder)-1)]
	return s.o.Jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobPoll, RunAt: s.now().Add(d), Payload: payload{PayoutID: id, N: n},
		DedupeKey: fmt.Sprintf("payout:%s:poll:%d", id, n)})
}

// synthesisedFailure says whether p failed by our own reading of an error, not
// by the provider's word.
func synthesisedFailure(p store.Payout) bool {
	return p.Status == StatusFailed && p.RawStatus == rawSynthesised
}

func terminal(status string) bool {
	return status == StatusFailed || status == StatusReversed
}

// apply books the provider's view of a payout under the row lock: it moves the
// payout forward, emits the matching event once, and schedules the next poll
// while it is still on its way. A failed or reversed payout never moves again;
// a succeeded one can still be reversed. The exception is a failure we
// concluded ourselves (rawSynthesised): it is polled, and the provider's
// answer replaces it.
func (s *Service) apply(ctx context.Context, id string, snap connector.PayoutSnapshot) error {
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPayoutForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		next := string(snap.Status)
		switch snap.Status {
		case connector.PayoutPending, connector.PayoutProcessing, connector.PayoutSucceeded, connector.PayoutFailed, connector.PayoutReversed:
		default:
			next = StatusProcessing // an unknown status is never a failure
		}
		if next == StatusPending {
			next = StatusProcessing // the provider has it: from here on we follow it
		}
		synth := synthesisedFailure(p)
		if (terminal(p.Status) && !synth) || (p.Status == next && !(synth && snap.RawStatus != rawSynthesised)) ||
			(p.Status == StatusSucceeded && next != StatusReversed) {
			return nil
		}
		if p.Status == StatusProcessing && next == StatusPending {
			return nil // never goes backwards
		}
		now := s.now()
		p.Status, p.RawStatus = next, snap.RawStatus
		if snap.ProviderPayoutID != "" {
			p.ProviderPayoutID = snap.ProviderPayoutID
		}
		if next == StatusFailed {
			p.FailureCode, p.FailureMessage = snap.ErrorCode, snap.ErrorMessage
		} else {
			p.FailureCode, p.FailureMessage = "", "" // a needs_review note or a failure the provider overruled
		}
		p.CompletedAt = time.Time{}
		if next == StatusSucceeded || next == StatusFailed || next == StatusReversed {
			p.CompletedAt = now
		}
		if err := store.UpdatePayout(ctx, tx, &p, now); err != nil {
			return err
		}
		ev := map[string]string{StatusProcessing: EvProcessing, StatusPending: "", StatusSucceeded: EvSucceeded, StatusFailed: EvFailed, StatusReversed: EvReversed}[next]
		if ev != "" {
			if err := s.emit(ctx, tx, ev, p); err != nil {
				return err
			}
		}
		if next == StatusFailed && snap.RawStatus == rawSynthesised {
			return s.enqueuePoll(ctx, tx, p.ID, 0) // ask the provider before this is final
		}
		if next == StatusProcessing || next == StatusPending {
			return s.enqueuePoll(ctx, tx, p.ID, 0)
		}
		if next == StatusSucceeded {
			// A succeeded payout can still come back; look once more later.
			return s.enqueuePoll(ctx, tx, p.ID, len(pollLadder)-1)
		}
		return nil
	})
}
