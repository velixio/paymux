package invoice

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
)

var (
	// ErrInvalid wraps every validation failure of the admin operations.
	ErrInvalid = errors.New("invalid")
	sacRE      = regexp.MustCompile(`^[0-9]{6}$`)
	prefixRE   = regexp.MustCompile(`^[A-Z0-9]{1,5}$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// TaxEntityInput describes a legal entity to register.
type TaxEntityInput struct {
	LegalName, TradeName, GSTIN, PAN, Address string
	// StateCode defaults to the State encoded in the GSTIN and must equal it.
	StateCode string
	// LUT is optional: all three fields or none. Dates are inclusive IST days.
	LUTNumber        string
	LUTFrom, LUTTo   time.Time
	InvoicePrefix    string // default INV
	CreditNotePrefix string // default CN
	SignatoryName    string
	FooterNote       string
	SupportEmail     string
	TurnoverBand     string
	// LogoBlobID sets the logo; on update an empty value keeps the current one
	// unless RemoveLogo is set.
	LogoBlobID string
	RemoveLogo bool
}

// checkPrefix rejects a prefix that could push a document number past 16
// characters or outside [A-Z0-9/-].
func checkPrefix(kind, p string) error {
	if !prefixRE.MatchString(p) {
		return invalid("%s prefix %q must be 1-5 characters of A-Z and 0-9", kind, p)
	}
	if _, err := tax.DocumentNumber(p, "2627", 999999); err != nil {
		return invalid("%s prefix %q: %v", kind, p, err)
	}
	return nil
}

// normalize validates the input and fills its defaults (State and PAN from
// the GSTIN, default prefixes).
func (in *TaxEntityInput) normalize() error {
	in.GSTIN = strings.ToUpper(strings.TrimSpace(in.GSTIN))
	if strings.TrimSpace(in.LegalName) == "" {
		return invalid("legal name is required")
	}
	if err := tax.ValidGSTIN(in.GSTIN); err != nil {
		return invalid("GSTIN: %v", err)
	}
	fromGSTIN := tax.StateFromGSTIN(in.GSTIN)
	if in.StateCode == "" {
		in.StateCode = fromGSTIN
	}
	if in.StateCode != fromGSTIN {
		return invalid("state code %s does not match the GSTIN state %s", in.StateCode, fromGSTIN)
	}
	if in.PAN == "" {
		in.PAN = in.GSTIN[2:12]
	}
	if in.InvoicePrefix == "" {
		in.InvoicePrefix = "INV"
	}
	if in.CreditNotePrefix == "" {
		in.CreditNotePrefix = "CN"
	}
	for kind, p := range map[string]string{"invoice": in.InvoicePrefix, "credit note": in.CreditNotePrefix} {
		if err := checkPrefix(kind, p); err != nil {
			return err
		}
	}
	hasLUT := in.LUTNumber != "" || !in.LUTFrom.IsZero() || !in.LUTTo.IsZero()
	if hasLUT && (in.LUTNumber == "" || in.LUTFrom.IsZero() || in.LUTTo.IsZero() || in.LUTTo.Before(in.LUTFrom)) {
		return invalid("a LUT needs a number and a valid-from date not after its valid-to date")
	}
	return nil
}

// AddTaxEntity validates and stores a tax entity and audits it.
func (s *Service) AddTaxEntity(ctx context.Context, actor audit.Actor, in TaxEntityInput) (store.TaxEntity, error) {
	if err := in.normalize(); err != nil {
		return store.TaxEntity{}, err
	}
	now := s.now().UTC()
	e := store.TaxEntity{ID: ids.New("te"), OrgID: store.DefaultOrgID, LegalName: strings.TrimSpace(in.LegalName),
		TradeName: in.TradeName, GSTIN: in.GSTIN, PAN: in.PAN, Address: in.Address, StateCode: in.StateCode,
		LUTNumber: in.LUTNumber, LUTValidFrom: in.LUTFrom, LUTValidTo: in.LUTTo,
		InvoicePrefix: in.InvoicePrefix, CreditNotePrefix: in.CreditNotePrefix, SignatoryName: in.SignatoryName,
		FooterNote: in.FooterNote, SupportEmail: in.SupportEmail, TurnoverBand: in.TurnoverBand, LogoBlobID: in.LogoBlobID,
		CreatedAt: now, UpdatedAt: now}
	err := s.db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertTaxEntity(ctx, tx, e); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: e.OrgID, Actor: actor, Action: "tax_entity.create", Target: e.ID, After: e})
	})
	if errors.Is(err, store.ErrConflict) {
		return store.TaxEntity{}, invalid("a tax entity with GSTIN %s already exists", in.GSTIN)
	}
	return e, err
}

// UpdateTaxEntity changes a tax entity and audits the change. The GSTIN cannot
// change (documents already carry it): register a new entity instead. Prefix
// changes apply to documents issued from now on.
func (s *Service) UpdateTaxEntity(ctx context.Context, actor audit.Actor, id string, in TaxEntityInput) (store.TaxEntity, error) {
	if err := in.normalize(); err != nil {
		return store.TaxEntity{}, err
	}
	var out store.TaxEntity
	err := s.db.Write(ctx, func(tx store.Tx) error {
		cur, err := store.GetTaxEntity(ctx, tx, id)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("no tax entity %q", id)
		}
		if err != nil {
			return err
		}
		if in.GSTIN != cur.GSTIN {
			return invalid("the GSTIN of an entity cannot change; register a new tax entity")
		}
		out = cur
		out.LegalName, out.TradeName, out.PAN, out.Address = strings.TrimSpace(in.LegalName), in.TradeName, in.PAN, in.Address
		out.LUTNumber, out.LUTValidFrom, out.LUTValidTo = in.LUTNumber, in.LUTFrom, in.LUTTo
		out.InvoicePrefix, out.CreditNotePrefix = in.InvoicePrefix, in.CreditNotePrefix
		out.SignatoryName, out.FooterNote, out.SupportEmail, out.TurnoverBand = in.SignatoryName, in.FooterNote, in.SupportEmail, in.TurnoverBand
		switch {
		case in.RemoveLogo:
			out.LogoBlobID = ""
		case in.LogoBlobID != "":
			out.LogoBlobID = in.LogoBlobID
		}
		out.UpdatedAt = s.now().UTC()
		if err := store.UpdateTaxEntity(ctx, tx, out, out.UpdatedAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: cur.OrgID, Actor: actor, Action: "tax_entity.update", Target: id, Before: cur, After: out})
	})
	return out, err
}

// FindTaxEntity resolves an id (te_...) or a GSTIN.
func (s *Service) FindTaxEntity(ctx context.Context, idOrGSTIN string) (store.TaxEntity, error) {
	var e store.TaxEntity
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		if strings.HasPrefix(idOrGSTIN, "te_") {
			e, err = store.GetTaxEntity(ctx, q, idOrGSTIN)
		} else {
			e, err = store.FindTaxEntityByGSTIN(ctx, q, store.DefaultOrgID, strings.ToUpper(idOrGSTIN))
		}
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.TaxEntity{}, invalid("no tax entity %q", idOrGSTIN)
	}
	return e, err
}

// AppTaxInput is the tax profile of an app.
type AppTaxInput struct {
	AppID, TaxEntityID string
	IssueInvoices      bool
	SACCode            string // default 998315
	GSTRateBP          int    // basis points, 0..2800 (1800 = 18%)
	PricesIncludeTax   bool
	// InvoicePrefix and CreditNotePrefix optionally give the app its own series.
	InvoicePrefix, CreditNotePrefix string
}

// SetAppTaxProfile validates and stores the tax profile of an app.
func (s *Service) SetAppTaxProfile(ctx context.Context, actor audit.Actor, in AppTaxInput) (store.AppTaxProfile, error) {
	if in.SACCode == "" {
		in.SACCode = "998315"
	}
	if !sacRE.MatchString(in.SACCode) {
		return store.AppTaxProfile{}, invalid("SAC code %q must be 6 digits", in.SACCode)
	}
	if in.GSTRateBP < 0 || in.GSTRateBP > 2800 {
		return store.AppTaxProfile{}, invalid("rate %d must be 0..2800 basis points", in.GSTRateBP)
	}
	for kind, p := range map[string]string{"invoice": in.InvoicePrefix, "credit note": in.CreditNotePrefix} {
		if p != "" {
			if err := checkPrefix(kind, p); err != nil {
				return store.AppTaxProfile{}, err
			}
		}
	}
	p := store.AppTaxProfile{AppID: in.AppID, TaxEntityID: in.TaxEntityID, IssueInvoices: in.IssueInvoices, SACCode: in.SACCode,
		GSTRateBP: in.GSTRateBP, PricesIncludeTax: in.PricesIncludeTax, InvoicePrefix: in.InvoicePrefix, CreditNotePrefix: in.CreditNotePrefix}
	err := s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetApp(ctx, tx, in.AppID)
		if err != nil {
			return invalid("no app %q", in.AppID)
		}
		if _, err := store.GetTaxEntity(ctx, tx, in.TaxEntityID); err != nil {
			return invalid("no tax entity %q", in.TaxEntityID)
		}
		if err := store.UpsertAppTaxProfile(ctx, tx, p, s.now().UTC()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: actor, Action: "app_tax_profile.set", Target: app.ID, After: p})
	})
	return p, err
}
