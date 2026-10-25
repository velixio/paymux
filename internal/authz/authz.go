// Package authz answers "may this actor do this action on this resource".
// Every guarded operation goes through Can; the set of actions is closed.
package authz

import (
	"slices"
	"strings"
)

// Role is a dashboard user's role.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator" // refunds, disputes, replay, reissue, view
	RoleViewer   Role = "viewer"
)

// Action is a closed enum of guarded operations.
type Action string

const (
	PaymentCreate         Action = "payment.create"
	PaymentRead           Action = "payment.read"
	PaymentCancel         Action = "payment.cancel"
	RefundCreate          Action = "refund.create"
	RefundRead            Action = "refund.read"
	DisputeManage         Action = "dispute.manage"
	WebhookReplay         Action = "webhook.replay"
	InvoiceReissue        Action = "invoice.reissue"
	SettingsRead          Action = "settings.read"
	SettingsWrite         Action = "settings.write"
	AppCreate             Action = "app.create"
	APIKeyCreate          Action = "api_key.create"
	ProviderAccountManage Action = "provider_account.manage"
	UserManage            Action = "user.manage"
	AuditRead             Action = "audit.read"
	RoleManage            Action = "role.manage"
	PayoutCreate          Action = "payout.create"  // sends money out: admin only unless a role is given it
	LogsRead              Action = "logs.read"      // the Logs page: errors and failed provider calls; admin only unless a role is given it
	SettingsAdmin         Action = "settings.admin" // the sign-in policy and the e-invoice trust anchor; the built-in admin only
	SystemUpdate          Action = "system.update"  // installs a new release of the hub from the dashboard; the built-in admin only
)

// AllActions lists every action.
var AllActions = []Action{PaymentCreate, PaymentRead, PaymentCancel, RefundCreate, RefundRead, DisputeManage,
	WebhookReplay, InvoiceReissue, SettingsRead, SettingsWrite, AppCreate, APIKeyCreate, ProviderAccountManage,
	UserManage, AuditRead, RoleManage, PayoutCreate, LogsRead, SettingsAdmin, SystemUpdate}

// adminOnly actions can never be granted to a custom role: whoever holds them
// can create users and roles, so they could give themselves everything, or
// (SettingsAdmin) stretch every session or swap the e-invoice trust anchor,
// or (SystemUpdate) replace the hub's own binary.
var adminOnly = []Action{UserManage, RoleManage, SettingsAdmin, SystemUpdate}

// Grantable lists the actions a custom role may be given.
func Grantable() []Action {
	var out []Action
	for _, a := range AllActions {
		if !slices.Contains(adminOnly, a) {
			out = append(out, a)
		}
	}
	return out
}

// ValidAction reports whether a is in the closed enum.
func ValidAction(a Action) bool { return slices.Contains(AllActions, a) }

// BuiltinRoles are the roles defined in code: they cannot be changed or
// deleted, and always resolve from here, whatever the roles table says.
func BuiltinRoles() map[Role][]Action {
	return map[Role][]Action{
		RoleAdmin:    slices.Clone(AllActions),
		RoleOperator: slices.Clone(operatorActions),
		RoleViewer:   slices.Clone(viewerActions),
	}
}

// Resolver answers which actions a custom role grants. Built-in roles never
// ask it.
type Resolver interface {
	Actions(role Role) ([]Action, bool)
}

// Kind is the type of actor.
type Kind string

const (
	KindUser   Kind = "user"
	KindAPIKey Kind = "api_key"
	KindSystem Kind = "system"
)

// Actor is who is asking. Users carry a Role and optionally an OrgID.
// API keys carry the AppID and Mode they belong to.
type Actor struct {
	Kind  Kind
	ID    string
	Role  Role
	OrgID string
	AppID string
	Mode  string
}

// Resource is what the action targets. Empty fields mean "not applicable".
type Resource struct {
	Type  string
	OrgID string
	AppID string
	Mode  string
}

var (
	viewerActions   = []Action{PaymentRead, RefundRead, SettingsRead}
	operatorActions = append(slices.Clone(viewerActions), PaymentCreate, PaymentCancel, RefundCreate, DisputeManage,
		WebhookReplay, InvoiceReissue)
	apiKeyActions = []Action{PaymentCreate, PaymentRead, PaymentCancel, RefundCreate, RefundRead, PayoutCreate}
)

// Can reports whether actor may perform action on res, with the built-in
// roles only. Unknown actions, roles and actor kinds are denied.
func Can(actor Actor, action Action, res Resource) bool { return CanWith(nil, actor, action, res) }

// CanWith is Can with custom roles resolved by r (nil: built-in roles only).
func CanWith(r Resolver, actor Actor, action Action, res Resource) bool {
	if !slices.Contains(AllActions, action) {
		return false
	}
	switch actor.Kind {
	case KindSystem:
		return true
	case KindAPIKey:
		return actor.AppID != "" && res.AppID == actor.AppID &&
			(res.Mode == "" || res.Mode == actor.Mode) &&
			slices.Contains(apiKeyActions, action)
	case KindUser:
		if actor.OrgID != "" && res.OrgID != "" && actor.OrgID != res.OrgID {
			return false
		}
		switch actor.Role {
		case RoleAdmin:
			return true
		case RoleOperator:
			return slices.Contains(operatorActions, action)
		case RoleViewer:
			return slices.Contains(viewerActions, action)
		}
		if r != nil && !slices.Contains(adminOnly, action) {
			if granted, ok := r.Actions(actor.Role); ok {
				return slices.Contains(granted, action)
			}
		}
	}
	return false
}

// Scopes limit an API key. A key without scopes (nil) has all of them.
const (
	ScopePaymentsRead       = "payments:read"
	ScopePaymentsWrite      = "payments:write"
	ScopeRefundsRead        = "refunds:read"
	ScopeRefundsWrite       = "refunds:write"
	ScopeSubscriptionsRead  = "subscriptions:read"
	ScopeSubscriptionsWrite = "subscriptions:write"
	ScopeInvoicesRead       = "invoices:read"
	ScopePayoutsRead        = "payouts:read"
	ScopePayoutsWrite       = "payouts:write"
)

// AllScopes lists every scope.
var AllScopes = []string{ScopePaymentsRead, ScopePaymentsWrite, ScopeRefundsRead, ScopeRefundsWrite,
	ScopeSubscriptionsRead, ScopeSubscriptionsWrite, ScopeInvoicesRead, ScopePayoutsRead, ScopePayoutsWrite}

// explicitScopes are never implied by "no scopes": a key has them only when
// they are listed. Sending money out must be a deliberate grant, so keys made
// before payouts existed, and keys made without a scope list, cannot pay out.
var explicitScopes = []string{ScopePayoutsRead, ScopePayoutsWrite}

// ActionsForScopes lists the actions a person must hold to issue an API key
// with these scopes: a key is a delegation, so it may not carry power its issuer
// lacks. nil scopes (a full-access key) need what every non-payout scope needs.
// Read scopes need nothing beyond the right to issue keys.
func ActionsForScopes(scopes []string) []Action {
	if scopes == nil {
		return []Action{PaymentCreate, PaymentCancel, RefundCreate}
	}
	var out []Action
	add := func(a Action) {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	for _, sc := range scopes {
		switch sc {
		case ScopePaymentsWrite, ScopeSubscriptionsWrite:
			// These scopes also cancel payments and subscriptions.
			add(PaymentCreate)
			add(PaymentCancel)
		case ScopeRefundsWrite:
			add(RefundCreate)
		case ScopePayoutsRead, ScopePayoutsWrite:
			add(PayoutCreate)
		}
	}
	return out
}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool { return slices.Contains(AllScopes, s) }

// ScopeAllows reports whether the granted scopes cover need. nil granted means
// every scope except the explicit ones (payouts); "x:write" also covers
// "x:read".
func ScopeAllows(granted []string, need string) bool {
	if need == "" {
		return true
	}
	if granted == nil {
		return !slices.Contains(explicitScopes, need)
	}
	if slices.Contains(granted, need) {
		return true
	}
	if res, ok := strings.CutSuffix(need, ":read"); ok {
		return slices.Contains(granted, res+":write")
	}
	return false
}
