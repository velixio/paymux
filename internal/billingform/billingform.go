// Package billingform is the billing-details form of the hosted pages: what the
// buyer is asked (State, country, address, an optional GSTIN), how the answer is
// read and checked, and how the page is filled. The pay page (one-time
// payments) and the hosted subscription page use it, so the same buyer is asked
// the same things with the same rules.
//
// What the answers decide: the buyer's country and State fix the place of supply
// and so the tax on every invoice, the first and each renewal. A buyer whose
// country is unknown is invoiced as a domestic sale in the supplier's State, with
// the pos_defaulted flag. That default cannot be avoided for a currency that
// the buyer may pay from anywhere, so a country is asked whenever it is not
// known and the currency is not rupees. Rupees are taken to be an Indian buyer's.
package billingform

import (
	"context"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/internal/web"
)

// AddressThreshold is Rs 50,000 in paise: from this amount an Indian buyer's
// full address is required for the invoice.
const AddressThreshold = 5_000_000

// Need says what the form must ask.
type Need struct {
	// Show is true when something is missing and the buyer has to be shown the form.
	Show                                            bool
	StateRequired, AddressRequired, CountryRequired bool
}

// Settings reads the two settings that decide: billing_details and issue_invoices.
func Settings(ctx context.Context, st *settings.Service, appID string) (mode string, issue bool, err error) {
	v, err := st.Resolve(ctx, appID, "billing_details")
	if err != nil {
		return "", false, err
	}
	if err = v.Decode(&mode); err != nil {
		return "", false, err
	}
	if v, err = st.Resolve(ctx, appID, "issue_invoices"); err != nil {
		return "", false, err
	}
	err = v.Decode(&issue)
	return mode, issue, err
}

// NeedFor decides what to ask. mode is the billing_details setting (off,
// state_required, optional, required), issue whether the app issues invoices,
// hasCustomer whether the payment or subscription already has a customer.
func NeedFor(mode string, issue bool, currency string, amount int64, c store.Customer, hasCustomer bool) Need {
	// state_required only applies while the app issues invoices.
	if mode == "state_required" && !issue {
		mode = "off"
	}
	country := strings.ToUpper(c.Billing.Country)
	// The buyer is Indian when their country says so, or when it is unknown and
	// they pay in rupees. A buyer abroad needs no State or PIN: the sale is an export.
	india := country == "IN" || country == "" && currency == "INR"
	n := Need{
		StateRequired:   india && (mode == "state_required" || mode == "required"),
		AddressRequired: india && (mode == "required" || issue && mode != "off" && amount >= AddressThreshold),
		// Dollars (or any currency but rupees) from an unknown country: ask.
		CountryRequired: currency != "INR" && country == "" && issue && mode != "off",
	}
	b := c.Billing
	n.Show = n.StateRequired && b.StateCode == "" ||
		n.AddressRequired && (b.Line1 == "" || b.PostalCode == "") ||
		n.CountryRequired ||
		mode == "required" && c.Name == "" ||
		mode == "optional" && !hasCustomer
	return n
}

var (
	pinRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)
	// countryRE is an ISO 3166 alpha-2 code, upper case.
	countryRE = regexp.MustCompile(`^[A-Z]{2}$`)
)

// Parse reads and checks a submitted form. On a problem it returns the
// translation key of the message (billing.err.*) with what was typed, so the
// page can show it again.
func Parse(f url.Values, currency string, need Need) (core.BillingInput, string) {
	get := func(k string) string { return strings.TrimSpace(f.Get(k)) }
	in := core.BillingInput{Name: get("name"), Email: get("email")}
	b := &in.Billing
	b.StateCode, b.Line1, b.PostalCode = get("state_code"), get("line1"), get("postal_code")
	b.GSTIN, b.LegalName = strings.ToUpper(get("gstin")), get("legal_name")
	if len(in.Name) > 200 || len(b.Line1) > 200 || len(b.LegalName) > 200 {
		return in, "billing.err.too_long"
	}
	if in.Name == "" {
		return in, "billing.err.name"
	}
	if in.Email != "" {
		if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
			return in, "billing.err.email"
		}
	}
	b.Country = strings.ToUpper(get("country"))
	if b.Country == "" && currency == "INR" {
		b.Country = "IN"
	}
	if b.Country != "" && !countryRE.MatchString(b.Country) {
		return in, "billing.err.country"
	}
	if b.Country == "" && need.CountryRequired {
		return in, "billing.err.country_required"
	}
	if b.Country != "" && b.Country != "IN" {
		// A buyer abroad: an export. No Indian State, address check or GSTIN applies.
		b.StateCode, b.GSTIN, b.LegalName = "", "", ""
		return in, ""
	}
	if b.StateCode != "" {
		if _, ok := tax.StateName(b.StateCode); !ok {
			return in, "billing.err.state_invalid"
		}
	}
	// A buyer who says they are in India, when the country was unknown, owes a State too.
	stateRequired := need.StateRequired || need.CountryRequired
	if stateRequired && b.StateCode == "" && b.GSTIN == "" {
		return in, "billing.err.state_required"
	}
	if b.GSTIN != "" {
		if err := tax.ValidGSTIN(b.GSTIN); err != nil {
			return in, "billing.err.gstin"
		}
		if b.LegalName == "" {
			return in, "billing.err.legal_name"
		}
		gst := tax.StateFromGSTIN(b.GSTIN)
		switch {
		case b.StateCode == "":
			b.StateCode = gst
		case b.StateCode != gst:
			return in, "billing.err.gst_state"
		}
	}
	if stateRequired && b.StateCode == "" {
		return in, "billing.err.state_required"
	}
	if need.AddressRequired {
		if b.Line1 == "" {
			return in, "billing.err.address"
		}
		if !pinRE.MatchString(b.PostalCode) {
			return in, "billing.err.pin"
		}
	}
	return in, ""
}

// Fill puts the customer's details and the form's options into the page data.
func Fill(data *web.PayPage, c store.Customer, currency string, n Need) {
	data.StateRequired, data.AddressRequired, data.CountryRequired = n.StateRequired, n.AddressRequired, n.CountryRequired
	data.Billing = web.Billing{Name: c.Name, Email: c.Email, StateCode: c.Billing.StateCode, Line1: c.Billing.Line1,
		PostalCode: c.Billing.PostalCode, GSTIN: c.Billing.GSTIN, LegalName: c.Billing.LegalName, Country: c.Billing.Country}
	if data.Billing.Country == "" && currency == "INR" {
		data.Billing.Country = "IN"
	}
	// The State list is for buyers who may be in India: rupees, or a known Indian buyer.
	if currency == "INR" || strings.EqualFold(c.Billing.Country, "IN") {
		codes := tax.StateCodes()
		keys := make([]string, 0, len(codes))
		for code := range codes {
			keys = append(keys, code)
		}
		sort.Strings(keys)
		for _, code := range keys {
			data.IndiaStates = append(data.IndiaStates, web.StateOption{Code: code, Name: codes[code]})
		}
	}
}
