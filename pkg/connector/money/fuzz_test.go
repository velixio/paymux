package money

import "testing"

// Run for real with: go test -run xxx -fuzz FuzzParseDecimal -fuzztime 2m ./pkg/connector/money
func FuzzParseDecimal(f *testing.F) {
	for _, c := range [][2]string{{"499.00", "INR"}, {"0.5", "INR"}, {"1", "JPY"}, {"1.234", "KWD"}, {"-1.00", "USD"}, {"", "USD"}, {"1e3", "USD"},
		{"9223372036854775807", "JPY"}, {"92233720368547758.07", "USD"}, {"1,000.00", "USD"}, {"٣.٠٠", "USD"}, {"1.", "USD"}, {".5", "USD"}, {"499.00", "ZZZ"}} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, s, currency string) {
		minor, err := ParseDecimal(s, currency)
		if err != nil {
			return
		}
		text, err := FormatDecimal(minor, currency)
		if err != nil {
			t.Fatalf("ParseDecimal(%q,%q)=%d but FormatDecimal fails: %v", s, currency, minor, err)
		}
		back, err := ParseDecimal(text, currency)
		if err != nil || back != minor {
			t.Fatalf("%q %s -> %d -> %q -> %d (%v)", s, currency, minor, text, back, err)
		}
	})
}

func FuzzFormatDecimal(f *testing.F) {
	for _, v := range []int64{0, 1, -1, 49900, 1<<63 - 1, -1 << 63} {
		f.Add(v, "USD")
		f.Add(v, "JPY")
	}
	f.Fuzz(func(t *testing.T, minor int64, currency string) {
		text, err := FormatDecimal(minor, currency)
		if err != nil {
			return
		}
		back, err := ParseDecimal(text, currency)
		if err != nil || back != minor {
			t.Fatalf("%d %s -> %q -> %d (%v)", minor, currency, text, back, err)
		}
	})
}
