package tax

import (
	"testing"
	"time"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzValidGSTIN -fuzztime 2m ./internal/tax

func FuzzValidGSTIN(f *testing.F) {
	for _, s := range []string{"27AAPFU0939F1ZV", "29AAGCB7383J1Z4", "27AAPFU0939F1ZX", "", "99AAPFU0939F1ZV", "27aapfu0939f1zv", "27AAPFU0939F1Z", "\x00", "２７AAPFU0939F1ZV"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, g string) {
		err := ValidGSTIN(g)
		sc := StateFromGSTIN(g)
		if err == nil {
			if len(g) != 15 {
				t.Fatalf("accepted %q (length %d)", g, len(g))
			}
			if _, ok := StateName(sc); !ok {
				t.Fatalf("accepted %q with unknown state %q", g, sc)
			}
		}
	})
}

func FuzzClassify(f *testing.F) {
	f.Add("27AAPFU0939F1ZV", "27", "29AAGCB7383J1Z4", "29", "IN", int64(1760000000), true, true)
	f.Add("27AAPFU0939F1ZV", "27", "", "", "US", int64(1760000000), true, false)
	f.Add("", "", "", "ZZ", "", int64(-1), false, true)
	f.Fuzz(func(t *testing.T, supGSTIN, supState, rcpGSTIN, rcpState, country string, unix int64, foreign, lut bool) {
		sup := Supplier{GSTIN: supGSTIN, StateCode: supState}
		at := time.Unix(unix, 0).UTC()
		if lut {
			sup.LUT = &LUT{Number: "L1", ValidFrom: at.Add(-time.Hour), ValidTo: at.Add(time.Hour)}
		}
		c, err := Classify(sup, Recipient{GSTIN: rcpGSTIN, StateCode: rcpState, Country: country}, at, foreign)
		if err != nil {
			return
		}
		if c.SupplyType == "" || c.PlaceOfSupply == "" {
			t.Fatalf("incomplete classification %+v", c)
		}
		_ = c.ZeroRated()
		_ = IsB2CL(c, 1, 0)
		_ = FiscalYear(at)
	})
}

func FuzzAmountInWordsINR(f *testing.F) {
	for _, v := range []int64{0, 1, 99, 100, 49900, 10000000, 1<<63 - 1, -1, -1 << 63} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, paise int64) {
		_ = AmountInWordsINR(paise)
	})
}
