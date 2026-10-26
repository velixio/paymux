package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"regexp"
	"strings"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
)

var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHost(h string) bool {
	if h == "" || len(h) > 253 || net.ParseIP(h) != nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !hostLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// DomainChallenge is the DNS TXT record that proves control of d's zone:
// name _paymux-challenge.<host>, value paymux-verify=<token>.
func DomainChallenge(d store.Domain) (name, value string) {
	return domains.ChallengeName(d.Host), domains.ChallengeValue(d.Token)
}

// AddDomain registers a custom domain for an app (status pending). The first
// domain of an app becomes its primary one. The returned Token is what the
// activation check looks for at /.well-known/paymux/<token>, and the owner
// must also publish the TXT record DomainChallenge returns.
func (s *Service) AddDomain(ctx context.Context, actor audit.Actor, sc OrgScope, appID, host string) (store.Domain, error) {
	if err := sc.validate(); err != nil {
		return store.Domain{}, err
	}
	host = domains.Normalize(strings.TrimSpace(host))
	if !validHost(host) {
		return store.Domain{}, newErr(ErrInvalid, "%q is not a valid host name", host)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return store.Domain{}, err
	}
	var d store.Domain
	err := s.db.Write(ctx, func(tx store.Tx) error {
		_, err := store.GetAppInOrg(ctx, tx, sc.OrgID, appID)
		if err != nil {
			return mapStoreErr(err, "app")
		}
		n, err := store.CountDomains(ctx, tx, appID)
		if err != nil {
			return err
		}
		d = store.Domain{ID: ids.New("dom"), OrgID: sc.OrgID, AppID: appID, Host: host, IsPrimary: n == 0,
			Status: domains.StatusPending, Token: hex.EncodeToString(raw[:]), CreatedAt: s.nowUTC()}
		if err := store.InsertDomain(ctx, tx, d); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return newErr(ErrConflict, "domain %s is already registered", host)
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "domain.add", Target: d.ID,
			After: map[string]any{"app_id": appID, "host": host, "is_primary": d.IsPrimary}})
	})
	return d, err
}

// LookupHost implements domains.Lookup.
func (s *Service) LookupHost(ctx context.Context, host string) (domains.Domain, bool, error) {
	var d store.Domain
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		d, err = store.GetDomainByHost(ctx, q, domains.Normalize(host))
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return domains.Domain{}, false, nil
	}
	if err != nil {
		return domains.Domain{}, false, err
	}
	return domains.Domain{AppID: d.AppID, Status: d.Status, TXTVerified: !d.TXTVerifiedAt.IsZero()}, true, nil
}

// VerifyDomainToken reports whether token is the activation token of host. It
// answers GET /.well-known/paymux/<token> on custom domains.
func (s *Service) VerifyDomainToken(ctx context.Context, host, token string) bool {
	if token == "" {
		return false
	}
	var d store.Domain
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		d, err = store.GetDomainByHost(ctx, q, domains.Normalize(host))
		return
	})
	return err == nil && subtle.ConstantTimeCompare([]byte(d.Token), []byte(token)) == 1
}

// RecheckDomain puts a failed domain of the scope's org back to pending so
// the activation check tries again for another grace period.
func (s *Service) RecheckDomain(ctx context.Context, actor audit.Actor, sc OrgScope, domainID string) error {
	if err := sc.validate(); err != nil {
		return err
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		d, err := store.GetDomainByID(ctx, tx, domainID)
		if errors.Is(err, store.ErrNotFound) || err == nil && d.OrgID != sc.OrgID {
			return newErr(ErrNotFound, "domain not found")
		}
		if err != nil {
			return err
		}
		if d.Status != domains.StatusFailed {
			return newErr(ErrInvalidState, "only a failed domain can be rechecked, this one is %s", d.Status)
		}
		if err := store.ResetDomain(ctx, tx, d.ID, s.nowUTC()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "domain.recheck", Target: d.ID,
			After: map[string]string{"host": d.Host}})
	})
}
