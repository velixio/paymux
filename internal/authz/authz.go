// Package authz answers "may this actor do this action on this resource".
// Every guarded operation goes through Can; the set of actions is closed.
package authz

import "slices"

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
)

// AllActions lists every action.
var AllActions = []Action{PaymentCreate, PaymentRead, PaymentCancel, RefundCreate, RefundRead, DisputeManage,
	WebhookReplay, InvoiceReissue, SettingsRead, SettingsWrite, AppCreate, APIKeyCreate, ProviderAccountManage,
	UserManage, AuditRead}

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
	apiKeyActions = []Action{PaymentCreate, PaymentRead, PaymentCancel, RefundCreate, RefundRead}
)

// Can reports whether actor may perform action on res. Unknown actions,
// roles and actor kinds are denied.
func Can(actor Actor, action Action, res Resource) bool {
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
	}
	return false
}
