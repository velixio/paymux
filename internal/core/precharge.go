package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
)

// Pre-charge block flags. A checkout refused before any card is charged costs
// no processing fee, unlike a fraud refunded later. FlagRiskBlocked marks a
// payment refused at least once, with a FlagBlocked* flag naming the first rule
// that refused it; FlagRiskAllowed is an operator's word that it may be paid.
const (
	FlagRiskBlocked       = "risk_blocked"
	FlagRiskAllowed       = "risk_allowed"
	FlagBlockedVPN        = "blocked_vpn"
	FlagBlockedCountry    = "blocked_country"
	FlagBlockedFraudLink  = "blocked_fraud_link"
	FlagBlockedIPLimit    = "blocked_ip_limit"
	FlagBlockedIPDeclines = "blocked_ip_declines"
	FlagBlockedEmailLimit = "blocked_email_limit"
)

// prechargeWindow is how far back the per-IP and per-email counts look.
const prechargeWindow = 24 * time.Hour

// prechargeBlock is why a checkout of p must be refused before any charge.
type prechargeBlock struct {
	Flag   string // one of the FlagBlocked* flags; "" when the checkout may start
	Detail string // for the operator: the alert and the audit log, never the buyer
}

// prechargeCheck applies the pre-charge block rules to p, from the IPs that
// opened its checkout (the app's Paymux-Buyer-IP and the pay page) and its
// customer. Only live payments are checked. Like the review rules it is observational: what cannot be read is
// logged and skipped, never fatal, so an outage never stops every checkout.
func (s *Service) prechargeCheck(ctx context.Context, p store.Payment) prechargeBlock {
	// Test mode charges no card, so it pays no fee and is never refused.
	if p.Mode != "live" || merchantInitiated(p) || p.HasFlag(FlagRiskAllowed) {
		return prechargeBlock{}
	}
	since := s.nowUTC().Add(-prechargeWindow)
	var block prechargeBlock
	var ips []string
	var cust store.Customer
	var vpnOn, countryOn bool
	err := s.db.Read(ctx, func(q store.Querier) error {
		checkout, err := store.CheckoutIPs(ctx, q, p.ID)
		if err != nil {
			return err
		}
		for _, c := range checkout {
			if iprep.Public(c.IP) {
				ips = append(ips, c.IP)
			}
		}
		if p.CustomerID != "" {
			if cust, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && err != store.ErrNotFound {
				return err
			}
		}
		email := strings.TrimSpace(cust.Email)
		if on, err := s.boolPolicy(ctx, q, p.AppID, "block_fraud_linked"); err != nil {
			return err
		} else if on {
			for _, ip := range ips {
				linked, err := store.FraudLinked(ctx, q, p.OrgID, ip, "")
				if err != nil {
					return err
				}
				if linked != "" {
					block = prechargeBlock{FlagBlockedFraudLink, fmt.Sprintf("its IP %s was used on payment %s, tagged fraud", ip, linked)}
					return nil
				}
			}
			if email != "" {
				linked, err := store.FraudLinked(ctx, q, p.OrgID, "", email)
				if err != nil {
					return err
				}
				if linked != "" {
					block = prechargeBlock{FlagBlockedFraudLink, fmt.Sprintf("its email address was used on payment %s, tagged fraud", linked)}
					return nil
				}
			}
		}
		if limit, err := s.intPolicy(ctx, q, p.AppID, "ip_payment_limit"); err != nil {
			return err
		} else if limit > 0 {
			for _, ip := range ips {
				n, err := store.IPCheckoutPayments(ctx, q, p.AppID, ip, p.ID, since)
				if err != nil {
					return err
				}
				if n >= limit {
					block = prechargeBlock{FlagBlockedIPLimit, fmt.Sprintf("IP %s already started a checkout for %d other payments in 24 hours (limit %d)", ip, n, limit)}
					return nil
				}
			}
		}
		if limit, err := s.intPolicy(ctx, q, p.AppID, "ip_decline_limit"); err != nil {
			return err
		} else if limit > 0 {
			for _, ip := range ips {
				n, err := store.IPDeclinedCards(ctx, q, p.AppID, ip, since)
				if err != nil {
					return err
				}
				if n >= limit {
					block = prechargeBlock{FlagBlockedIPDeclines, fmt.Sprintf("%d different cards were declined from IP %s in 24 hours (limit %d)", n, ip, limit)}
					return nil
				}
			}
		}
		if limit, err := s.intPolicy(ctx, q, p.AppID, "email_paid_limit"); err != nil {
			return err
		} else if limit > 0 && email != "" {
			n, err := store.EmailPaidPayments(ctx, q, p.AppID, email, since)
			if err != nil {
				return err
			}
			if n >= limit {
				block = prechargeBlock{FlagBlockedEmailLimit, fmt.Sprintf("its email address already paid %d payments in 24 hours (limit %d)", n, limit)}
				return nil
			}
		}
		if vpnOn, err = s.boolPolicy(ctx, q, p.AppID, "block_vpn_checkout"); err != nil {
			return err
		}
		countryOn, err = s.boolPolicy(ctx, q, p.AppID, "block_country_mismatch")
		return err
	})
	if err != nil {
		slog.Warn("core: pre-charge checks could not be read; they were skipped", "payment", p.ID, "err", err)
		return prechargeBlock{}
	}
	if block.Flag != "" || (!vpnOn && !countryOn) {
		return block
	}
	billing := strings.ToUpper(strings.TrimSpace(cust.Billing.Country))
	for _, ip := range ips {
		info, ok := s.ipInfo(ctx, ip)
		if !ok {
			continue
		}
		if vpnOn && info.Masked() {
			return prechargeBlock{FlagBlockedVPN, fmt.Sprintf("IP %s is a %s", ip, info.Kind())}
		}
		if countryOn && billing != "" && info.Country != "" && info.Country != billing {
			return prechargeBlock{FlagBlockedCountry, fmt.Sprintf("IP %s is in %s, the billing address in %s", ip, info.Country, billing)}
		}
	}
	return prechargeBlock{}
}

// recordPrechargeBlock tags payment paymentID as refused by block, audits it
// and alerts the operator, the first time only.
func (s *Service) recordPrechargeBlock(ctx context.Context, paymentID string, block prechargeBlock) {
	var alerts []notify.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		alerts = nil
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return err
		}
		if p.HasFlag(FlagRiskBlocked) {
			return nil
		}
		addFlag(&p, FlagRiskBlocked)
		addFlag(&p, block.Flag)
		if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
			return err
		}
		alerts = append(alerts, notify.Alert{Severity: notify.SeverityWarning, Kind: "payment.risk_blocked",
			Message: fmt.Sprintf("Payment %s was refused before any charge: %s. If the buyer is genuine, allow it in the dashboard.", p.ID, block.Detail),
			Refs:    map[string]string{"payment": p.ID, "app": p.AppID, "rule": block.Flag}})
		return audit.Record(ctx, tx, audit.Entry{OrgID: p.OrgID, Actor: audit.System, Action: "payment.risk_block", Target: p.ID,
			After: map[string]any{"rule": block.Flag, "detail": block.Detail}})
	})
	if err != nil {
		slog.Warn("core: a pre-charge block could not be recorded", "payment", paymentID, "err", err)
		return
	}
	s.raise(ctx, alerts)
}

// Blocked reports whether p was refused by a pre-charge rule and not allowed since.
func Blocked(p store.Payment) bool {
	return p.HasFlag(FlagRiskBlocked) && !p.HasFlag(FlagRiskAllowed)
}

// AllowCheckout lets payment paymentID be paid despite the pre-charge rules,
// on an operator's word. It reports whether anything changed.
func (s *Service) AllowCheckout(ctx context.Context, actor audit.Actor, paymentID string) (bool, error) {
	var changed bool
	err := s.db.Write(ctx, func(tx store.Tx) error {
		changed = false
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if p.HasFlag(FlagRiskAllowed) {
			return nil
		}
		before := ViewPayment(p)
		addFlag(&p, FlagRiskAllowed)
		if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
			return err
		}
		changed = true
		return audit.Record(ctx, tx, audit.Entry{OrgID: p.OrgID, Actor: actor, Action: "payment.risk_allow", Target: p.ID,
			Before: before, After: ViewPayment(p)})
	})
	return changed, err
}
