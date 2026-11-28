package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/store"
)

// IP review flags. FlagVPN marks a success paid from, or started from, a VPN,
// proxy or Tor IP; FlagIPMismatch one paid from an IP that never opened the
// checkout. Both are review reasons an operator may approve.
const (
	FlagVPN        = "vpn"
	FlagIPMismatch = "ip_mismatch"
)

// ipLookupMaxAge is how long a cached lookup is believed.
const ipLookupMaxAge = 30 * 24 * time.Hour

// ipLookupTimeout bounds one lookup made while a provider snapshot waits.
const ipLookupTimeout = 4 * time.Second

// NoteCheckoutIP records that ip opened payment paymentID's checkout. The IP is
// looked up when a success arrives, which is when the review needs it. A
// failure here never stops the checkout: the caller logs it.
func (s *Service) NoteCheckoutIP(ctx context.Context, paymentID, ip string) error {
	if !iprep.Public(ip) {
		// A private address is a proxy paymux does not trust (or a test
		// browser), never the buyer: comparing it would hold every success.
		return nil
	}
	if err := s.db.Write(ctx, func(tx store.Tx) error {
		return store.NoteCheckoutIP(ctx, tx, paymentID, ip, store.CheckoutIPPayPage, s.nowUTC())
	}); err != nil {
		return err
	}
	return nil
}

// CheckoutIPs lists the IPs that opened payment paymentID's checkout, with
// what the lookup service said about each when it was asked (nil when not).
func (s *Service) CheckoutIPs(ctx context.Context, paymentID string) ([]store.CheckoutIP, map[string]iprep.Info, error) {
	var ips []store.CheckoutIP
	infos := map[string]iprep.Info{}
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if ips, err = store.CheckoutIPs(ctx, q, paymentID); err != nil {
			return err
		}
		for _, c := range ips {
			if info, ok := cachedIPInfo(ctx, q, c.IP, time.Time{}); ok {
				infos[c.IP] = info
			}
		}
		return nil
	})
	return ips, infos, err
}

// IPInfo returns the cached lookup of ip, if any. It never asks the service.
func (s *Service) IPInfo(ctx context.Context, ip string) (iprep.Info, bool) {
	var info iprep.Info
	var ok bool
	_ = s.db.Read(ctx, func(q store.Querier) error {
		info, ok = cachedIPInfo(ctx, q, ip, time.Time{})
		return nil
	})
	return info, ok
}

// cachedIPInfo reads the cached lookup of ip, ignoring one made before since.
func cachedIPInfo(ctx context.Context, q store.Queryer, ip string, since time.Time) (iprep.Info, bool) {
	raw, at, err := store.GetIPLookup(ctx, q, ip)
	if err != nil || at.Before(since) {
		return iprep.Info{}, false
	}
	var info iprep.Info
	if json.Unmarshal([]byte(raw), &info) != nil {
		return iprep.Info{}, false
	}
	return info, true
}

// ipInfo returns what is known of ip: the cache when fresh, else the lookup
// service, whose answer is cached. ok is false when neither can say.
func (s *Service) ipInfo(ctx context.Context, ip string) (iprep.Info, bool) {
	if s.ipLookup == nil || !iprep.Public(ip) {
		return iprep.Info{}, false
	}
	var info iprep.Info
	var ok bool
	_ = s.db.Read(ctx, func(q store.Querier) error {
		info, ok = cachedIPInfo(ctx, q, ip, s.nowUTC().Add(-ipLookupMaxAge))
		return nil
	})
	if ok {
		return info, true
	}
	lctx, cancel := context.WithTimeout(ctx, ipLookupTimeout)
	defer cancel()
	info, err := s.ipLookup.Lookup(lctx, ip)
	if err != nil {
		slog.Warn("core: IP lookup failed; IP review rules skipped for it", "err", err)
		return iprep.Info{}, false
	}
	raw, err := json.Marshal(info)
	if err == nil {
		err = s.db.Write(ctx, func(tx store.Tx) error { return store.PutIPLookup(ctx, tx, ip, string(raw), s.nowUTC()) })
	}
	if err != nil {
		slog.Warn("core: IP lookup could not be cached", "err", err)
	}
	return info, true
}

// ipCheck is what the IP rules know when a success arrives, read before the
// snapshot's transaction: the IPs that opened the checkout, the IP the
// provider saw pay, and lookups of them. Like the try history it is
// observational: what cannot be read is skipped, never fatal.
type ipCheck struct {
	loaded   bool
	checkout []string
	paidFrom string
	info     map[string]iprep.Info
}

func (s *Service) loadIPCheck(ctx context.Context, attemptID string, snap Snapshot) ipCheck {
	if snap.Status != state.AttemptCaptured {
		return ipCheck{} // only a success is reviewed
	}
	var c ipCheck
	err := s.db.Read(ctx, func(q store.Querier) error {
		a, err := store.GetAttempt(ctx, q, attemptID)
		if err != nil {
			return err
		}
		ips, err := store.CheckoutIPs(ctx, q, a.PaymentID)
		for _, ip := range ips {
			c.checkout = append(c.checkout, ip.IP)
		}
		return err
	})
	if err != nil {
		slog.Warn("core: checkout IPs could not be read; IP review rules skipped", "attempt", attemptID, "err", err)
		return ipCheck{}
	}
	c.loaded, c.paidFrom, c.info = true, snap.Risk.IPAddress, map[string]iprep.Info{}
	for _, ip := range append(slices.Clone(c.checkout), c.paidFrom) {
		if _, seen := c.info[ip]; ip == "" || seen {
			continue
		}
		if info, ok := s.ipInfo(ctx, ip); ok {
			c.info[ip] = info
		}
	}
	return c
}

// ipReason is the IP review reason of a success on p, or "".
func (s *Service) ipReason(ctx context.Context, tx store.Tx, p store.Payment, c ipCheck) (string, error) {
	if !c.loaded || len(c.checkout) == 0 {
		return "", nil // nothing opened the pay page we know of: no comparison
	}
	vpn, err := s.boolPolicy(ctx, tx, p.AppID, "review_vpn")
	if err != nil {
		return "", err
	}
	if vpn {
		for _, ip := range append(slices.Clone(c.checkout), c.paidFrom) {
			if c.info[ip].Masked() {
				return FlagVPN, nil
			}
		}
	}
	mismatch, err := s.boolPolicy(ctx, tx, p.AppID, "review_ip_mismatch")
	if err != nil || !mismatch {
		return "", err
	}
	if ipMismatch(c, s.ipLookup != nil) {
		return FlagIPMismatch, nil
	}
	return "", nil
}

// ipMismatch reports whether the IP that paid is not one that opened the
// checkout. With lookups, a different IP in a country one of the checkout IPs
// is in still matches (a phone moving between Wi-Fi and mobile data); one not
// looked up is not judged. Without lookups, only an IP of the same family
// (IPv4 or IPv6) counts, since a browser may reach two sites over different
// families.
func ipMismatch(c ipCheck, lookups bool) bool {
	paid, err := netip.ParseAddr(c.paidFrom)
	if err != nil || slices.Contains(c.checkout, c.paidFrom) {
		return false
	}
	paid = paid.Unmap()
	if lookups {
		at, ok := c.info[c.paidFrom]
		if !ok || at.Country == "" {
			return false
		}
		for _, ip := range c.checkout {
			info, ok := c.info[ip]
			if !ok || info.Country == "" || info.Country == at.Country {
				return false
			}
		}
		return true
	}
	for _, ip := range c.checkout {
		if a, err := netip.ParseAddr(ip); err == nil && a.Unmap().Is4() == paid.Is4() {
			return true
		}
	}
	return false
}

func (s *Service) boolPolicy(ctx context.Context, q store.Queryer, appID, key string) (bool, error) {
	v, err := s.policy(ctx, q, appID, key)
	if err != nil {
		return false, err
	}
	var b bool
	err = v.Decode(&b)
	return b, err
}

// FlagAddressMismatch marks a success whose card billing address, as the buyer
// entered it at the provider, is in another country than the billing address
// the app sent. Another postal code in the same country is not judged: buyers
// often give a nearby or older address, so it held genuine payments. A review
// reason an operator may approve.
const FlagAddressMismatch = "address_mismatch"

// addressReason is the address review reason of a success on p paid with
// snap, or "". c is p's customer (zero when it has none).
func (s *Service) addressReason(ctx context.Context, tx store.Tx, p store.Payment, c store.Customer, snap Snapshot) (string, error) {
	on, err := s.boolPolicy(ctx, tx, p.AppID, "review_address_mismatch")
	if err != nil || !on {
		return "", err
	}
	if AddressMismatch(c.Billing.Country, snap.Risk.Country) {
		return FlagAddressMismatch, nil
	}
	return "", nil
}

// AddressMismatch reports whether the order's billing country differs from
// the card's. A side that is missing is not judged.
func AddressMismatch(orderCountry, cardCountry string) bool {
	orderCountry, cardCountry = strings.ToUpper(strings.TrimSpace(orderCountry)), strings.ToUpper(strings.TrimSpace(cardCountry))
	return orderCountry != "" && cardCountry != "" && orderCountry != cardCountry
}
