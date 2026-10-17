// Package audit appends security-relevant actions to the audit log.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
)

// ActorType says who performed an action.
type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorAPIKey ActorType = "api_key"
	ActorSystem ActorType = "system"
)

// Actor identifies the performer: a dashboard user, an API key, or the system.
type Actor struct {
	Type ActorType
	ID   string
}

// System is the actor for automatic actions.
var System = Actor{Type: ActorSystem}

// Entry is one audit record. Before and After are snapshots (any
// JSON-serialisable value); secrets in them are redacted before storage.
type Entry struct {
	OrgID         string
	Actor         Actor
	Action        string // e.g. "setting.update", "refund.create"
	Target        string // e.g. "app:app_x/payment_ttl" or a row id
	Before, After any
	IP            string
}

// Record appends e to the audit log inside tx, so the log entry commits or
// rolls back together with the change it describes.
func Record(ctx context.Context, tx store.Tx, e Entry) error {
	if e.OrgID == "" || e.Action == "" {
		return errors.New("audit: org and action are required")
	}
	switch e.Actor.Type {
	case ActorUser, ActorAPIKey, ActorSystem:
	default:
		return errors.New("audit: unknown actor type " + string(e.Actor.Type))
	}
	return store.InsertAudit(ctx, tx, store.AuditRow{
		ID: ids.New("aud"), OrgID: e.OrgID, ActorType: string(e.Actor.Type), ActorID: e.Actor.ID,
		Action: e.Action, Target: e.Target, Before: Redact(e.Before), After: Redact(e.After), IP: e.IP,
		At: time.Now().UTC(),
	})
}

var secretKey = regexp.MustCompile(`(?i)secret|password|passwd|token|hash|api_key|private_key|totp|dsc|authorization|config_enc`)

const redacted = "[redacted]"

// Redact renders v as JSON with the values of secret-looking keys replaced.
// It returns "" for nil.
func Redact(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return `"[unserialisable]"`
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return `"[unserialisable]"`
	}
	out, _ := json.Marshal(scrub(tree))
	return string(out)
}

func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secretKey.MatchString(k) {
				x[k] = redacted
			} else {
				x[k] = scrub(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = scrub(x[i])
		}
	}
	return v
}
