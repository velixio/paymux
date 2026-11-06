package invoice_test

import (
	"testing"

	"github.com/velixio/paymux/internal/invoice"
)

func TestConvertMinor(t *testing.T) {
	for name, c := range map[string]struct {
		minor          int64
		from, fromRate string
		to, toRate     string
		want           int64
	}{
		"same currency needs no rate": {1234, "USD", "", "USD", "", 1234},
		"foreign to INR":              {10000, "USD", "80", "INR", "", 800000},
		"INR to foreign":              {800000, "INR", "", "USD", "80", 10000},
		"cross rate through INR":      {2000, "EUR", "90", "USD", "80", 2250},
		"zero-decimal source":         {1000, "JPY", "0.5", "USD", "80", 625},
		"zero-decimal target":         {100, "USD", "80", "JPY", "0.5", 160},
		"three-decimal target":        {100, "USD", "80", "KWD", "270", 296},
		"cent to paise":               {1, "USD", "50", "INR", "", 50},
		"half a cent rounds up":       {25, "INR", "", "USD", "50", 1},
		"negative rounds symmetric":   {-25, "INR", "", "USD", "50", -1},
	} {
		got, err := invoice.ConvertMinor(c.minor, c.from, c.fromRate, c.to, c.toRate)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %d want %d", name, got, c.want)
		}
	}
}

func TestConvertMinorRejectsBadInput(t *testing.T) {
	for name, c := range map[string]struct {
		from, fromRate, to, toRate string
	}{
		"missing source rate": {"USD", "", "INR", ""},
		"missing target rate": {"INR", "", "USD", ""},
		"zero rate":           {"USD", "0", "INR", ""},
		"unknown source":      {"QQQ", "80", "INR", ""},
		"unknown target":      {"INR", "", "QQQ", "80"},
	} {
		if _, err := invoice.ConvertMinor(100, c.from, c.fromRate, c.to, c.toRate); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
