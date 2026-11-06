package outbound

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/velixio/paymux/internal/store"
)

const (
	// telegramPaymentLockFor outlasts one job's two Bot API calls, and frees the
	// message if the process dies holding the lock.
	telegramPaymentLockFor = 2 * time.Minute
	maxTelegramReasonRunes = 60
	maxTelegramReasonLines = 10
)

// telegramPaymentEvents are the events that update a payment's one message.
// Others, such as payment.needs_review, are alerts of their own: they can
// report something the message would not visibly change for.
var telegramPaymentEvents = []string{"payment.created", "payment.requires_action", "payment.processing",
	"payment.succeeded", "payment.failed", "payment.canceled", "payment.expired"}

func updatesTelegramPaymentMessage(ev store.EventRow) bool {
	return ev.ObjectType == "payment" && slices.Contains(telegramPaymentEvents, ev.Type)
}

// telegramPaymentLock is a held lock on a payment's message.
type telegramPaymentLock struct {
	key       store.TelegramPaymentMessageKey
	messageID int64
}

func (s *Service) lockTelegramPaymentMessage(ctx context.Context, ev store.EventRow, chatID string) (*telegramPaymentLock, error) {
	lock := &telegramPaymentLock{key: store.TelegramPaymentMessageKey{OrgID: ev.OrgID, Mode: ev.Mode, ChatID: chatID, PaymentID: ev.ObjectID}}
	var locked bool
	now := s.now()
	err := s.db.Write(ctx, func(tx store.Tx) (err error) {
		lock.messageID, locked, err = store.LockTelegramPaymentMessage(ctx, tx, lock.key, now, now.Add(telegramPaymentLockFor))
		return err
	})
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another job is updating this payment's Telegram message")
	}
	return lock, nil
}

func (s *Service) unlockTelegramPaymentMessage(ctx context.Context, lock *telegramPaymentLock) {
	ctx = context.WithoutCancel(ctx)
	if err := s.db.Write(ctx, func(tx store.Tx) error {
		return store.UnlockTelegramPaymentMessage(ctx, tx, lock.key, lock.messageID)
	}); err != nil {
		s.log.Warn("telegram payment message could not be unlocked", "payment_id", lock.key.PaymentID, "err", err)
	}
}

// telegramPaymentText renders the payment's current state. ok is false when the
// event's payment cannot be shown.
func (s *Service) telegramPaymentText(ctx context.Context, ev store.EventRow) (text string, ok bool, err error) {
	payment, customer, ok, err := s.notificationPayment(ctx, ev)
	if err != nil || !ok {
		return "", false, err
	}
	var attempts []store.Attempt
	var tries []store.ProviderPaymentAttempt
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		if attempts, err = store.ListAttempts(ctx, q, payment.ID); err != nil {
			return err
		}
		tries, err = store.ListProviderPaymentAttempts(ctx, q, payment.ID)
		return err
	}); err != nil {
		return "", false, err
	}
	return renderTelegramPayment(ev, payment, customer, attempts, tries), true, nil
}

func renderTelegramPayment(ev store.EventRow, p store.Payment, c store.Customer, attempts []store.Attempt, tries []store.ProviderPaymentAttempt) string {
	failures := paymentFailures(attempts, tries)
	icon, status := "⏳", ""
	switch p.Status {
	case "succeeded":
		icon, status = "✅", "Paid "+paidAt(ev, attempts).UTC().Format("15:04")+" UTC"
		if len(failures) > 0 {
			status += " on attempt " + strconv.Itoa(len(failures)+1)
		}
	case "canceled":
		icon, status = "🚫", "Canceled"
	case "expired":
		icon, status = "⌛", "Expired"
	case "needs_review":
		icon, status = "⚠️", "Needs review"
	case "failed":
		icon = "❌"
		if len(failures) == 0 {
			status = "Failed"
		}
	}
	if status == "" && len(failures) > 1 {
		status = strconv.Itoa(len(failures)) + " failed attempts"
	}

	lines := []string{icon + " " + notificationAmount(p) + " · " + strings.ToUpper(ev.Mode) + " · " + p.ID}
	var who []string
	if code := notificationCountry(p, c); code != "" {
		who = append(who, countryFlag(code)+" "+code)
	}
	if c.Email != "" {
		who = append(who, c.Email)
	}
	if len(who) > 0 {
		lines = append(lines, strings.Join(who, " · "))
	}
	if status != "" {
		lines = append(lines, status)
	}
	lines = append(lines, failureLines(failures)...)
	return strings.Join(lines, "\n")
}

type paymentFailure struct {
	reason string
	at     time.Time
}

// paymentFailures lists every failed try. An attempt's provider tries replace
// the attempt itself: it is the summary of them, or of its latest one.
func paymentFailures(attempts []store.Attempt, tries []store.ProviderPaymentAttempt) []paymentFailure {
	triesOf := map[string][]store.ProviderPaymentAttempt{}
	for _, t := range tries {
		triesOf[t.AttemptID] = append(triesOf[t.AttemptID], t)
	}
	var out []paymentFailure
	for _, a := range attempts {
		own := 0
		for _, t := range triesOf[a.ID] {
			if t.Status != "failed" {
				continue
			}
			own++
			// A try's updated_at moves whenever history is refetched, and its
			// created_at can be the checkout's, so the attempt's own failure
			// time is better for the try it settled on.
			when := t.CreatedAt
			if a.Status == "failed" && a.ProviderPaymentID == t.ProviderPaymentID {
				when = a.UpdatedAt
			}
			out = append(out, paymentFailure{failureReason(t.FailureMessage, t.FailureCode), when})
		}
		if own == 0 && a.Status == "failed" {
			out = append(out, paymentFailure{failureReason(a.FailureMessage, a.FailureCode), a.UpdatedAt})
		}
	}
	return out
}

func failureReason(message, code string) string {
	reason := strings.Join(strings.Fields(message), " ")
	if reason == "" {
		reason = strings.Join(strings.Fields(code), " ")
	}
	if reason == "" {
		return "No provider reason"
	}
	if utf8.RuneCountInString(reason) > maxTelegramReasonRunes {
		reason = string([]rune(reason)[:maxTelegramReasonRunes-1]) + "…"
	}
	return reason
}

// failureLines groups failures by reason, the most recently seen reason first.
func failureLines(failures []paymentFailure) []string {
	type group struct {
		reason string
		count  int
		last   time.Time
	}
	var groups []*group
	byReason := map[string]*group{}
	for _, f := range failures {
		g := byReason[f.reason]
		if g == nil {
			g = &group{reason: f.reason}
			byReason[f.reason] = g
			groups = append(groups, g)
		}
		g.count++
		if f.at.After(g.last) {
			g.last = f.at
		}
	}
	slices.SortFunc(groups, func(a, b *group) int {
		if c := b.last.Compare(a.last); c != 0 {
			return c
		}
		return strings.Compare(a.reason, b.reason)
	})
	var lines []string
	for i, g := range groups {
		if i == maxTelegramReasonLines {
			lines = append(lines, fmt.Sprintf("… %d more reasons", len(groups)-i))
			break
		}
		when := g.last.UTC().Format("15:04")
		if g.count > 1 {
			when = "last " + when
		}
		lines = append(lines, fmt.Sprintf("%d× %s · %s", g.count, g.reason, when))
	}
	return lines
}

func paidAt(ev store.EventRow, attempts []store.Attempt) time.Time {
	var at time.Time
	for _, a := range attempts {
		if a.Status == "captured" && !a.Duplicate && a.UpdatedAt.After(at) {
			at = a.UpdatedAt
		}
	}
	if at.IsZero() {
		return ev.CreatedAt
	}
	return at
}
