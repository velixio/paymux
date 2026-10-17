package money

import (
	"errors"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		in, cur string
		want    int64
		err     error
	}{
		{"499.00", "INR", 49900, nil},
		{"499", "INR", 49900, nil},
		{"499.5", "INR", 49950, nil},
		{"0.01", "INR", 1, nil},
		{"500", "JPY", 500, nil},
		{"500.00", "JPY", 500, nil}, // trailing zeros past the exponent are harmless
		{"1.234", "KWD", 1234, nil},
		{"499.005", "INR", 0, ErrInvalidAmount},
		{"500.1", "JPY", 0, ErrInvalidAmount},
		{"", "INR", 0, ErrInvalidAmount},
		{" 1", "INR", 0, ErrInvalidAmount},
		{"1e3", "INR", 0, ErrInvalidAmount},
		{"-1", "INR", 0, ErrInvalidAmount},
		{"+1", "INR", 0, ErrInvalidAmount},
		{"1,000", "INR", 0, ErrInvalidAmount},
		{"NaN", "INR", 0, ErrInvalidAmount},
		{"1.", "INR", 0, ErrInvalidAmount},
		{".5", "INR", 0, ErrInvalidAmount},
		{"92233720368547758.08", "INR", 0, ErrInvalidAmount},
		{"92233720368547758.07", "INR", 9223372036854775807, nil},
		{"1", "XYZ", 0, ErrUnknownCurrency},
		{"1", "inr", 0, ErrUnknownCurrency},
		{"1", "BDT", 100, nil},
	}
	for _, tc := range tests {
		got, err := ParseDecimal(tc.in, tc.cur)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("ParseDecimal(%q,%q) = %d,%v; want %d,%v", tc.in, tc.cur, got, err, tc.want, tc.err)
		}
	}
}

func TestFormatDecimal(t *testing.T) {
	tests := []struct {
		minor int64
		cur   string
		want  string
	}{
		{49900, "INR", "499.00"},
		{5, "INR", "0.05"},
		{500, "JPY", "500"},
		{1234, "KWD", "1.234"},
		{0, "KWD", "0.000"},
	}
	for _, tc := range tests {
		got, err := FormatDecimal(tc.minor, tc.cur)
		if err != nil || got != tc.want {
			t.Errorf("FormatDecimal(%d,%q) = %q,%v; want %q", tc.minor, tc.cur, got, err, tc.want)
		}
	}
	if _, err := FormatDecimal(-1, "INR"); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("negative: %v", err)
	}
	if _, err := FormatDecimal(1, "XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("unknown: %v", err)
	}
}

func TestExponent(t *testing.T) {
	for cur, want := range map[string]int{"INR": 2, "JPY": 0, "KWD": 3, "USD": 2, "VND": 0, "TND": 3} {
		if got, err := Exponent(cur); err != nil || got != want {
			t.Errorf("Exponent(%s) = %d,%v; want %d", cur, got, err, want)
		}
	}
	if _, err := Exponent("XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("unknown: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, cur := range allCurrencies() {
		for _, minor := range []int64{0, 1, 99, 123456789} {
			s, err := FormatDecimal(minor, cur)
			if err != nil {
				t.Fatalf("%s %d: %v", cur, minor, err)
			}
			back, err := ParseDecimal(s, cur)
			if err != nil || back != minor {
				t.Errorf("%s %d -> %q -> %d,%v", cur, minor, s, back, err)
			}
		}
	}
}
