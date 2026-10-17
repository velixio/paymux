package invoice

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// orgValue resolves an org-level setting through q, or says it is unset when
// the service has no settings (the CLI's admin-only service).
func (s *Service) orgValue(ctx context.Context, q store.Queryer, orgID, key string, dst any) (bool, error) {
	if s.settings == nil {
		return false, nil
	}
	v, err := s.settings.ResolveOrgCachedWith(ctx, q, orgID, key)
	if err != nil {
		return false, fmt.Errorf("invoice: setting %s: %w", key, err)
	}
	return true, v.Decode(dst)
}

// b2clThreshold is the b2cl_threshold setting in paise.
func (s *Service) b2clThreshold(ctx context.Context, orgID string) (int64, error) {
	n := B2CLThresholdPaise
	err := s.db.Read(ctx, func(q store.Querier) error {
		_, err := s.orgValue(ctx, q, orgID, "b2cl_threshold", &n)
		return err
	})
	return n, err
}

// creditNoteDeadline is the last instant a credit note may be issued against an
// invoice issued at `issued`, by the credit_note_cutoff setting. ok is false
// when the cutoff is off.
func (s *Service) creditNoteDeadline(ctx context.Context, q store.Queryer, orgID string, issued time.Time) (deadline time.Time, ok bool, err error) {
	cutoff := "11-30"
	if _, err := s.orgValue(ctx, q, orgID, "credit_note_cutoff", &cutoff); err != nil {
		return time.Time{}, false, err
	}
	if cutoff == "off" {
		return time.Time{}, false, nil
	}
	mm, dd, found := strings.Cut(cutoff, "-")
	m, err1 := strconv.Atoi(mm)
	d, err2 := strconv.Atoi(dd)
	if !found || err1 != nil || err2 != nil {
		return tax.CreditNoteDeadline(issued), true, nil // a malformed value never reaches here (validated); fall back to the statutory day
	}
	return tax.CreditNoteDeadlineOn(issued, time.Month(m), d), true, nil
}
