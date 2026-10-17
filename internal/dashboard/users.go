package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
)

// Roles, lowest to highest.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// Roles lists the valid roles.
var Roles = []string{RoleViewer, RoleOperator, RoleAdmin}

// ErrInvalid marks a rejected user input (bad email, role, weak password,
// duplicate); its message is safe to show to the operator.
var ErrInvalid = errors.New("invalid input")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func roleRank(r string) int {
	for i, x := range Roles {
		if x == r {
			return i + 1
		}
	}
	return 0
}

// Users creates and administers dashboard users. The HTTP handlers and the
// CLI share it.
type Users struct {
	DB  *store.DB
	Now func() time.Time
}

// Create adds a user with a password. The user enrols TOTP at first login.
func (u Users) Create(ctx context.Context, email, role, password string) (store.UserRow, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return store.UserRow{}, invalid("%q is not a valid email address", email)
	}
	if roleRank(role) == 0 {
		return store.UserRow{}, invalid("role must be one of %s", strings.Join(Roles, ", "))
	}
	hash, err := auth.HashPassword(password)
	if errors.Is(err, auth.ErrWeakPassword) {
		return store.UserRow{}, invalid("%v", err)
	}
	if err != nil {
		return store.UserRow{}, err
	}
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	row := store.UserRow{ID: ids.New("usr"), Email: email, PasswordHash: hash, TOTPLastStep: -1, Role: role, CreatedAt: now()}
	if err := u.DB.InsertUser(ctx, row); errors.Is(err, store.ErrDuplicate) {
		return store.UserRow{}, invalid("a user with that email already exists")
	} else if err != nil {
		return store.UserRow{}, err
	}
	return row, nil
}

// ResetTOTP clears a user's second factor and signs them out everywhere, so
// they enrol a new authenticator at next login.
func (u Users) ResetTOTP(ctx context.Context, email string) error {
	row, err := u.DB.UserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if err := u.DB.ResetUserTOTP(ctx, row.ID); err != nil {
		return err
	}
	return u.DB.DeleteUserSessions(ctx, row.ID)
}
