package tax

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// IST is India Standard Time (UTC+05:30, no DST).
var IST = time.FixedZone("IST", 5*3600+1800)

// SupplyType classifies a document for GST.
type SupplyType string

const (
	B2B        SupplyType = "b2b"
	B2C        SupplyType = "b2c"
	ExportLUT  SupplyType = "export_lut"
	ExportIGST SupplyType = "export_igst"
)

// PlaceOutsideIndia is the place of supply for exports.
const PlaceOutsideIndia = "OUTSIDE_INDIA"

// Flags raised during classification (reported as exceptions).
const (
	FlagPOSDefaulted = "pos_defaulted"
	FlagExportIGST   = "export_igst"
)

// LUT is a Letter of Undertaking for zero-rated exports; dates are inclusive days.
type LUT struct {
	Number             string
	ValidFrom, ValidTo time.Time
}

// Supplier is the issuing tax entity.
type Supplier struct {
	GSTIN, StateCode string
	LUT              *LUT
}

// Recipient is the buyer. Country is ISO-3166 alpha-2 ("IN" for India).
type Recipient struct {
	GSTIN, StateCode, Country string
}

// Classification is the GST treatment of one document.
type Classification struct {
	SupplyType    SupplyType
	PlaceOfSupply string // 2-digit state code or PlaceOutsideIndia
	InterState    bool
	Flags         []string
}

// Has reports whether flag f is set.
func (c Classification) Has(f string) bool {
	for _, x := range c.Flags {
		if x == f {
			return true
		}
	}
	return false
}

// ZeroRated reports whether no tax is charged (export under LUT).
func (c Classification) ZeroRated() bool { return c.SupplyType == ExportLUT }

func sameDayOrAfter(t, day time.Time) bool {
	ty, tm, td := t.In(IST).Date()
	dy, dm, dd := day.In(IST).Date()
	return time.Date(ty, tm, td, 0, 0, 0, 0, IST).Compare(time.Date(dy, dm, dd, 0, 0, 0, 0, IST)) >= 0
}

func (l *LUT) validOn(t time.Time) bool {
	return l != nil && l.Number != "" && sameDayOrAfter(t, l.ValidFrom) && sameDayOrAfter(l.ValidTo, t)
}

// Classify determines supply type, place of supply (IGST Act s.12(2)) and
// whether IGST applies. paidInForeignCurrency is the s.2(6) payment condition.
func Classify(sup Supplier, rcp Recipient, invoiceDate time.Time, paidInForeignCurrency bool) (Classification, error) {
	country := strings.ToUpper(rcp.Country)
	if country == "" {
		country = "IN"
	}
	if country != "IN" {
		c := Classification{PlaceOfSupply: PlaceOutsideIndia, InterState: true}
		if paidInForeignCurrency && sup.LUT.validOn(invoiceDate) {
			c.SupplyType = ExportLUT
		} else {
			c.SupplyType = ExportIGST
			c.Flags = []string{FlagExportIGST}
		}
		return c, nil
	}
	// A domestic supply is taxed by comparing states, so the supplier's must be
	// real (found by fuzzing: an empty one gave a place of supply of "").
	if _, ok := states[sup.StateCode]; !ok {
		return Classification{}, fmt.Errorf("tax: unknown supplier state code %q", sup.StateCode)
	}
	if rcp.GSTIN != "" {
		if err := ValidGSTIN(rcp.GSTIN); err != nil {
			return Classification{}, fmt.Errorf("recipient GSTIN %q: %w", rcp.GSTIN, err)
		}
		pos := StateFromGSTIN(rcp.GSTIN)
		return Classification{SupplyType: B2B, PlaceOfSupply: pos, InterState: pos != sup.StateCode}, nil
	}
	if rcp.StateCode == "" {
		return Classification{SupplyType: B2C, PlaceOfSupply: sup.StateCode, Flags: []string{FlagPOSDefaulted}}, nil
	}
	if _, ok := states[rcp.StateCode]; !ok {
		return Classification{}, fmt.Errorf("tax: unknown state code %q", rcp.StateCode)
	}
	return Classification{SupplyType: B2C, PlaceOfSupply: rcp.StateCode, InterState: rcp.StateCode != sup.StateCode}, nil
}

// IsB2CL reports whether an invoice goes in GSTR-1 B2CL: inter-state B2C with
// total strictly above threshold (default ₹1,00,000 = 10000000 paise).
func IsB2CL(c Classification, total, threshold int64) bool {
	return c.SupplyType == B2C && c.InterState && total > threshold
}

// FiscalYear returns the Indian financial year (Apr–Mar, IST) as "2627".
func FiscalYear(t time.Time) string {
	t = t.In(IST)
	y := t.Year()
	if t.Month() < time.April {
		y--
	}
	return fmt.Sprintf("%02d%02d", y%100, (y+1)%100)
}

// FiscalYearLabel turns "2627" into "2026-27".
func FiscalYearLabel(fy string) string {
	if len(fy) != 4 {
		return fy
	}
	return "20" + fy[:2] + "-" + fy[2:]
}

// CreditNoteDeadline is the end of 30 November after the end of the financial
// year of the original supply (CGST Act s.34(2)), in IST.
func CreditNoteDeadline(supply time.Time) time.Time {
	t := supply.In(IST)
	endYear := t.Year() + 1
	if t.Month() < time.April {
		endYear = t.Year()
	}
	return time.Date(endYear, time.November, 30, 23, 59, 59, 999999999, IST)
}

// CreditNoteDeadlineOn is CreditNoteDeadline with the day configurable: the
// end of the given month and day (IST) after the end of the financial year of
// the supply. CreditNoteDeadline is month 11, day 30.
func CreditNoteDeadlineOn(supply time.Time, month time.Month, day int) time.Time {
	t := supply.In(IST)
	endYear := t.Year() + 1
	if t.Month() < time.April {
		endYear = t.Year()
	}
	return time.Date(endYear, month, day, 23, 59, 59, 999999999, IST)
}

var prefixRE = regexp.MustCompile(`^[A-Z0-9]{1,5}$`)

// ErrDocumentNumber is returned for invalid numbering inputs.
var ErrDocumentNumber = errors.New("tax: invalid document number")

// DocumentNumber formats "{PREFIX}/{FY}/{seq:06}" and enforces Rule 46(b):
// at most 16 characters from [A-Z0-9/-].
func DocumentNumber(prefix, fy string, seq int64) (string, error) {
	if !prefixRE.MatchString(prefix) || len(fy) != 4 || seq < 1 {
		return "", ErrDocumentNumber
	}
	n := fmt.Sprintf("%s/%s/%06d", prefix, fy, seq)
	if len(n) > 16 {
		return "", fmt.Errorf("%w: %q exceeds 16 characters", ErrDocumentNumber, n)
	}
	return n, nil
}
