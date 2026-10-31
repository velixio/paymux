// Package outbound persists domain events and delivers them to app webhook
// endpoints as Standard Webhooks, with retries, an SSRF guard and replay.
package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/outbound/stdwebhooks"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

// JobDeliver is the job kind that performs one delivery attempt.
const JobDeliver = "webhook.deliver"

// JobNotify sends configured operator email and Telegram notifications.
const JobNotify = "notification.deliver"

// DefaultRetrySchedule is the spec §6.5 / §8a webhook_retry_schedule: the
// delay before attempt 1, 2, ... (about 3 days in total).
var DefaultRetrySchedule = []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute,
	2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour}

// Options configures a Service.
type Options struct {
	DB                 *store.DB
	Queue              *jobs.Queue
	Box                *secrets.Box
	AllowPrivate       bool         // PAYMUX_ALLOW_PRIVATE_WEBHOOKS
	Client             *http.Client // default: SSRF-guarded client
	Logger             *slog.Logger
	Now                func() time.Time
	Mailer             notify.Mailer
	NotificationClient *http.Client
}

// Service is the outbound webhook emitter and deliverer. It implements
// events.Emitter.
type Service struct {
	db           *store.DB
	q            *jobs.Queue
	box          *secrets.Box
	client       *http.Client
	log          *slog.Logger
	now          func() time.Time
	mailer       notify.Mailer
	notifyClient *http.Client

	// RetrySchedule returns the delay before each attempt; the settings lane
	// can point it at the webhook_retry_schedule setting. Nil or empty means
	// DefaultRetrySchedule.
	RetrySchedule func() []time.Duration
}

var _ events.Emitter = (*Service)(nil)

// New builds a Service.
func New(o Options) *Service {
	s := &Service{db: o.DB, q: o.Queue, box: o.Box, client: o.Client, log: o.Logger, now: o.Now,
		mailer: o.Mailer, notifyClient: o.NotificationClient}
	if s.client == nil {
		s.client = newHTTPClient(o.AllowPrivate, nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.mailer == nil {
		s.mailer = notify.NoopMailer{}
	}
	if s.notifyClient == nil {
		s.notifyClient = &http.Client{Timeout: 10 * time.Second}
	}
	return s
}

// Register installs the delivery job handler on the queue.
func (s *Service) Register() {
	s.q.Register(JobDeliver, s.handleDeliver)
	s.q.Register(JobNotify, s.handleNotification)
}

func (s *Service) schedule() []time.Duration {
	if s.RetrySchedule != nil {
		if sc := s.RetrySchedule(); len(sc) > 0 {
			return sc
		}
	}
	return DefaultRetrySchedule
}

type deliverPayload struct {
	DeliveryID string `json:"delivery_id"`
}

func (s *Service) enqueue(ctx context.Context, tx store.Tx, deliveryID string, attempt int, runAt time.Time) error {
	return s.q.Enqueue(ctx, tx, jobs.Job{
		Kind: JobDeliver, Payload: deliverPayload{DeliveryID: deliveryID}, RunAt: runAt,
		// One key per attempt: the running job's own key must not swallow
		// the follow-up enqueued from inside its handler.
		DedupeKey: fmt.Sprintf("%s:%s:%d", JobDeliver, deliveryID, attempt),
	})
}

func matches(types []string, typ string) bool { return len(types) == 0 || slices.Contains(types, typ) }

// Emit records e and schedules a delivery to every enabled endpoint of
// e.AppID whose event types match, all inside the caller's transaction.
func (s *Service) Emit(ctx context.Context, tx store.Tx, e events.Event) error {
	data, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("marshal event data: %w", err)
	}
	now := s.now()
	row := store.EventRow{ID: ids.New("evt"), OrgID: e.OrgID, AppID: e.AppID, Mode: e.Mode, Type: e.Type,
		ObjectType: e.ObjectType, ObjectID: e.ObjectID, ObjectVersion: e.ObjectVersion, Data: data, CreatedAt: now}
	if err := s.db.InsertEvent(ctx, tx, row); err != nil {
		return err
	}
	eps, err := s.db.ListEnabledEndpoints(ctx, tx, e.AppID)
	if err != nil {
		return err
	}
	first := now.Add(s.schedule()[0])
	for _, ep := range eps {
		if !matches(ep.EventTypes, e.Type) || ep.Mode != e.Mode {
			continue // a test event never reaches a live endpoint, nor the reverse
		}
		d := store.DeliveryRow{ID: ids.New("whd"), EventID: row.ID, EndpointID: ep.ID, Status: "pending",
			NextAttemptAt: first, CreatedAt: now, UpdatedAt: now}
		if err := s.db.InsertDelivery(ctx, tx, d); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, d.ID, 0, first); err != nil {
			return err
		}
	}
	config, err := store.GetNotificationConfig(ctx, tx, e.OrgID, e.Mode)
	if err != nil {
		return err
	}
	if (config.EmailEnabled || config.TelegramEnabled) && matches(config.EventTypes, e.Type) {
		if err := s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobNotify, Payload: notificationPayload{EventID: row.ID},
			RunAt: now, DedupeKey: JobNotify + ":" + row.ID}); err != nil {
			return err
		}
	}
	return nil
}

type body struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	// Livemode is false for events of test payments, true for live ones.
	Livemode bool     `json:"livemode"`
	Data     bodyData `json:"data"`
}

type bodyData struct {
	Object json.RawMessage `json:"object"`
}

func buildBody(ev store.EventRow) ([]byte, error) {
	obj := ev.Data
	if len(obj) == 0 {
		obj = json.RawMessage("null")
	}
	return json.Marshal(body{Type: ev.Type, Timestamp: ev.CreatedAt.UTC().Format(time.RFC3339), Livemode: ev.Mode == "live", Data: bodyData{Object: obj}})
}

func (s *Service) handleDeliver(ctx context.Context, payload json.RawMessage) error {
	var p deliverPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.DeliveryID == "" {
		return jobs.Permanent(fmt.Errorf("bad %s payload", JobDeliver))
	}
	d, err := s.db.GetDelivery(ctx, p.DeliveryID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	if d.Delivery.Status != "pending" {
		return nil // already delivered, failed, or superseded by a replay
	}
	attempt := d.Delivery.AttemptCount + 1

	result := s.attempt(ctx, d)
	status, derr := result.status, result.err
	now := s.now()
	record := func(tx store.Tx) error {
		msg := ""
		if result.err != nil {
			msg = result.err.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
		}
		return store.InsertDeliveryAttempt(ctx, tx, store.DeliveryAttemptRow{ID: ids.New("wha"), DeliveryID: d.Delivery.ID,
			AttemptNo: attempt, RequestBody: result.request, ResponseStatus: result.status, ResponseBody: result.response, Error: msg, CreatedAt: now})
	}
	if derr == nil {
		return s.db.Write(ctx, func(tx store.Tx) error {
			if err := record(tx); err != nil {
				return err
			}
			return s.db.UpdateDelivery(ctx, tx, d.Delivery.ID, store.DeliveryUpdate{
				Status: "delivered", AttemptCount: attempt, LastStatus: status, DeliveredAt: now}, now)
		})
	}

	msg := derr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	u := store.DeliveryUpdate{Status: "pending", AttemptCount: attempt, LastStatus: status, LastError: msg}
	sched := s.schedule()
	var terminal bool
	var pe permanent
	if errors.As(derr, &pe) || attempt >= len(sched) {
		terminal = true
	}
	if terminal {
		u.Status = "failed"
		s.log.Warn("webhook delivery failed", "delivery_id", d.Delivery.ID, "endpoint_id", d.Endpoint.ID, "attempts", attempt, "err", msg)
		return s.db.Write(ctx, func(tx store.Tx) error {
			if err := record(tx); err != nil {
				return err
			}
			return s.db.UpdateDelivery(ctx, tx, d.Delivery.ID, u, now)
		})
	}
	next := now.Add(sched[attempt])
	u.NextAttemptAt = next
	return s.db.Write(ctx, func(tx store.Tx) error {
		if err := record(tx); err != nil {
			return err
		}
		if err := s.db.UpdateDelivery(ctx, tx, d.Delivery.ID, u, now); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, d.Delivery.ID, attempt, next)
	})
}

// permanent marks a failure that retrying cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

type attemptResult struct {
	status            int
	request, response string
	err               error
}

// attempt performs one POST and retains the bounded request/response bodies
// needed to diagnose every delivery from the dashboard.
func (s *Service) attempt(ctx context.Context, d store.DeliveryDetail) (out attemptResult) {
	b, err := buildBody(d.Event)
	if err != nil {
		out.err = permanent{err}
		return
	}
	out.request = string(b)
	if !d.Endpoint.Enabled {
		out.err = permanent{errors.New("endpoint disabled")}
		return
	}
	secret, err := s.box.Open(d.Endpoint.SigningSecretEnc, secretAAD(d.Endpoint.ID))
	if err != nil {
		out.err = permanent{fmt.Errorf("signing secret: %w", err)}
		return
	}
	signer, err := stdwebhooks.NewSigner(string(secret))
	if err != nil {
		out.err = permanent{err}
		return
	}
	// webhook-id is the event id, so retries and replays reuse it and
	// receivers can deduplicate.
	hdr := signer.Headers(d.Event.ID, s.now(), b)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint.URL, bytes.NewReader(b))
	if err != nil {
		out.err = permanent{err}
		return
	}
	req.Header = hdr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Paymux-Webhooks/1")
	resp, err := s.client.Do(req)
	if err != nil {
		out.err = err
		return
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	out.status, out.response = resp.StatusCode, string(raw)
	if readErr != nil {
		out.err = readErr
		return
	}
	if resp.StatusCode/100 != 2 {
		out.err = fmt.Errorf("http %d", resp.StatusCode)
		return
	}
	return
}

// ReplayDelivery resends a delivered or failed delivery with the same
// webhook-id, restarting its attempt count.
func (s *Service) ReplayDelivery(ctx context.Context, id string) error {
	return s.db.Write(ctx, func(tx store.Tx) error { return s.replayTx(ctx, tx, id) })
}

func (s *Service) replayTx(ctx context.Context, tx store.Tx, id string) error {
	d, err := s.db.GetDeliveryTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if d.Status == "pending" {
		return fmt.Errorf("delivery %s is still pending", id)
	}
	now := s.now()
	if err := s.db.UpdateDelivery(ctx, tx, id, store.DeliveryUpdate{Status: "pending", NextAttemptAt: now}, now); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, id, 0, now)
}

// ReplayFailedSince replays every failed delivery of the endpoint last
// updated at or after t and returns how many were queued.
func (s *Service) ReplayFailedSince(ctx context.Context, endpointID string, t time.Time) (int, error) {
	idsList, err := s.db.ListFailedDeliveryIDs(ctx, endpointID, t)
	if err != nil {
		return 0, err
	}
	err = s.db.Write(ctx, func(tx store.Tx) error {
		for _, id := range idsList {
			if err := s.replayTx(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(idsList), nil
}
