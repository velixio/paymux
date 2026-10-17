package tax

import (
	"testing"
	"time"
)

func ist(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, IST)
}

func TestClassify(t *testing.T) {
	lut := &LUT{Number: "AD270326000001X", ValidFrom: ist(2026, 4, 1, 0, 0), ValidTo: ist(2027, 3, 31, 0, 0)}
	sup := Supplier{GSTIN: "27AAPFU0939F1ZV", StateCode: "27", LUT: lut}
	day := ist(2026, 10, 17, 12, 0)
	cases := []struct {
		name    string
		sup     Supplier
		rcp     Recipient
		fx      bool
		when    time.Time
		want    SupplyType
		pos     string
		inter   bool
		flag    string
	}{
		{"b2b intra", sup, Recipient{GSTIN: "27AAPFU0939F1ZV", Country: "IN"}, false, day, B2B, "27", false, ""},
		{"b2c inter", sup, Recipient{StateCode: "29", Country: "IN"}, false, day, B2C, "29", true, ""},
		{"b2c unknown state defaults to supplier", sup, Recipient{Country: "IN"}, false, day, B2C, "27", false, FlagPOSDefaulted},
		{"export under lut", sup, Recipient{Country: "BD"}, true, day, ExportLUT, PlaceOutsideIndia, true, ""},
		{"export, lut expired", sup, Recipient{Country: "BD"}, true, ist(2027, 4, 2, 0, 0), ExportIGST, PlaceOutsideIndia, true, FlagExportIGST},
		{"export paid in INR", sup, Recipient{Country: "US"}, false, day, ExportIGST, PlaceOutsideIndia, true, FlagExportIGST},
		{"export, no lut", Supplier{GSTIN: sup.GSTIN, StateCode: "27"}, Recipient{Country: "AE"}, true, day, ExportIGST, PlaceOutsideIndia, true, FlagExportIGST},
		{"lut last day inclusive", sup, Recipient{Country: "BD"}, true, ist(2027, 3, 31, 23, 59), ExportLUT, PlaceOutsideIndia, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Classify(c.sup, c.rcp, c.when, c.fx)
			if err != nil {
				t.Fatal(err)
			}
			if got.SupplyType != c.want || got.PlaceOfSupply != c.pos || got.InterState != c.inter {
				t.Fatalf("got %+v", got)
			}
			if c.flag != "" && !got.Has(c.flag) || c.flag == "" && len(got.Flags) != 0 {
				t.Fatalf("flags %v, want %q", got.Flags, c.flag)
			}
			if got.ZeroRated() != (c.want == ExportLUT) {
				t.Fatalf("ZeroRated mismatch")
			}
		})
	}
	if _, err := Classify(sup, Recipient{GSTIN: "BADGSTIN", Country: "IN"}, day, false); err == nil {
		t.Fatal("invalid recipient GSTIN accepted")
	}
	if _, err := Classify(sup, Recipient{StateCode: "99", Country: "IN"}, day, false); err == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestB2CL(t *testing.T) {
	c := Classification{SupplyType: B2C, InterState: true}
	if IsB2CL(c, 10000000, 10000000) {
		t.Fatal("exactly ₹1,00,000 is not above the threshold")
	}
	if !IsB2CL(c, 10000001, 10000000) {
		t.Fatal("above threshold should be B2CL")
	}
	c.InterState = false
	if IsB2CL(c, 20000000, 10000000) {
		t.Fatal("intra-state B2C is never B2CL")
	}
}

func TestFiscalYear(t *testing.T) {
	if fy := FiscalYear(ist(2026, 4, 1, 0, 0)); fy != "2627" {
		t.Fatalf("Apr 1 00:00 IST: %s", fy)
	}
	if fy := FiscalYear(ist(2026, 3, 31, 23, 59)); fy != "2526" {
		t.Fatalf("Mar 31 23:59 IST: %s", fy)
	}
	// 2026-03-31 19:00 UTC is 2026-04-01 00:30 IST.
	if fy := FiscalYear(time.Date(2026, 3, 31, 19, 0, 0, 0, time.UTC)); fy != "2627" {
		t.Fatalf("UTC input not converted to IST: %s", fy)
	}
	if l := FiscalYearLabel("2627"); l != "2026-27" {
		t.Fatalf("label %s", l)
	}
}

func TestCreditNoteDeadline(t *testing.T) {
	got := CreditNoteDeadline(ist(2026, 10, 17, 12, 0))
	want := ist(2027, 11, 30, 23, 59).Add(59*time.Second + 999999999)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := CreditNoteDeadline(ist(2027, 3, 31, 12, 0)); got.Year() != 2027 || got.Month() != 11 {
		t.Fatalf("FY 2026-27 supply: %v", got)
	}
}

func TestDocumentNumber(t *testing.T) {
	n, err := DocumentNumber("INV", "2627", 123)
	if err != nil || n != "INV/2627/000123" {
		t.Fatalf("%q %v", n, err)
	}
	if _, err := DocumentNumber("VSPROD", "2627", 1); err == nil {
		t.Fatal("prefix over 5 chars accepted")
	}
	if _, err := DocumentNumber("in", "2627", 1); err == nil {
		t.Fatal("lowercase prefix accepted")
	}
	if n, err := DocumentNumber("CN", "2627", 1234567); err != nil || n != "CN/2627/1234567" {
		t.Fatalf("7-digit seq: %q %v", n, err)
	}
	if _, err := DocumentNumber("ABCDE", "2627", 1234567); err == nil {
		t.Fatal("17 chars accepted")
	}
	if _, err := DocumentNumber("INV", "2627", 0); err == nil {
		t.Fatal("seq 0 accepted")
	}
}

func TestAmountInWords(t *testing.T) {
	cases := map[int64]string{
		49900:      "Rupees Four Hundred Ninety-Nine Only",
		49950:      "Rupees Four Hundred Ninety-Nine and Fifty Paise Only",
		1:          "Rupees Zero and One Paise Only",
		10000000:   "Rupees One Lakh Only",
		1234567800: "Rupees One Crore Twenty-Three Lakh Forty-Five Thousand Six Hundred Seventy-Eight Only",
		0:          "Rupees Zero Only",
	}
	for v, want := range cases {
		if got := AmountInWordsINR(v); got != want {
			t.Errorf("%d: got %q want %q", v, got, want)
		}
	}
}
