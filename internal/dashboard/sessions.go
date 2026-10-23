package dashboard

import (
	"context"
	"net/netip"
	"time"
)

// How long a sign-in lasts. The three limits are org settings:
//
//   - session_idle_hours (12): a session that saw no request for this long ends.
//   - session_max_days (7): a session ends this long after the sign-in, however active.
//   - session_remember_days (30; 0 turns "Remember me" off): with the box ticked
//     both limits are this one instead, so the user stays signed in for up to this
//     long after the last request and never longer than this after signing in.
//
// Remembering only lengthens a sign-in that has happened in full, password and
// second factor; it never replaces them.
const (
	defaultIdleHours    = 12
	defaultMaxDays      = 7
	defaultRememberDays = 30
)

type sessionLimits struct {
	idle, max time.Duration
	remember  time.Duration // 0: Remember me is off
}

func (d *Dashboard) intSetting(ctx context.Context, key string, def int) int {
	org, err := d.orgID(ctx)
	if err != nil || d.o.Settings == nil {
		return def
	}
	v, err := d.o.Settings.ResolveOrg(ctx, org, key)
	if err != nil {
		return def
	}
	var n int
	if err := v.Decode(&n); err != nil {
		return def
	}
	return n
}

func (d *Dashboard) sessionLimits(ctx context.Context) sessionLimits {
	return sessionLimits{
		idle:     time.Duration(d.intSetting(ctx, "session_idle_hours", defaultIdleHours)) * time.Hour,
		max:      time.Duration(d.intSetting(ctx, "session_max_days", defaultMaxDays)) * 24 * time.Hour,
		remember: time.Duration(d.intSetting(ctx, "session_remember_days", defaultRememberDays)) * 24 * time.Hour,
	}
}

// rememberDays is what the login page offers: 0 hides the checkbox.
func (d *Dashboard) rememberDays(ctx context.Context) int {
	return int(d.sessionLimits(ctx).remember / (24 * time.Hour))
}

// ipHint is the network of an address, not the address: a /24 for IPv4, a /64
// for IPv6. It is what the sessions list shows.
func ipHint(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	a = a.Unmap()
	bits := 64
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}
