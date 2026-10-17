package store

import "context"

// SubFilter filters SearchSubscriptions.
type SubFilter struct {
	AppID, Mode, Status string
	// Search matches the subscription id or the customer's email, case-insensitively.
	Search string
}

// SearchSubscriptions returns matching subscriptions across apps, newest
// first (ids are time-ordered). afterID is an exclusive cursor.
func SearchSubscriptions(ctx context.Context, q Queryer, f SubFilter, afterID string, limit int) ([]Subscription, error) {
	where, args := "1 = 1", []any{}
	add := func(cond string, a ...any) {
		where += " AND " + cond
		args = append(args, a...)
	}
	if f.AppID != "" {
		add("app_id = ?", f.AppID)
	}
	if f.Mode != "" {
		add("mode = ?", f.Mode)
	}
	if f.Status != "" {
		add("status = ?", f.Status)
	}
	if f.Search != "" {
		like := "%" + likeEscape(f.Search) + "%"
		add(`(LOWER(id) LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM customers c WHERE c.id = subscriptions.customer_id
			AND LOWER(COALESCE(c.email,'')) LIKE ? ESCAPE '\'))`, like, like)
	}
	if afterID != "" {
		add("id < ?", afterID)
	}
	return listSubs(ctx, q, where+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
}
