// Package money converts between decimal strings and int64 minor units using
// integer arithmetic only.
package money

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrUnknownCurrency = errors.New("money: unknown currency")
	ErrInvalidAmount   = errors.New("money: invalid amount")
)

// Zero-decimal and three-decimal ISO-4217 currencies; every other active
// code uses 2 decimals.
var (
	zeroDecimal  = set("BIF CLP DJF GNF ISK JPY KMF KRW PYG RWF UGX UYI VND VUV XAF XOF XPF")
	threeDecimal = set("BHD IQD JOD KWD LYD OMR TND")
	twoDecimal   = set("AED AFN ALL AMD ANG AOA ARS AUD AWG AZN BAM BBD BDT BGN BMD BND BOB BRL BSD BTN BWP BYN BZD " +
		"CAD CDF CHF CNY COP CRC CUP CVE CZK DKK DOP DZD EGP ERN ETB EUR FJD FKP GBP GEL GHS GIP GMD GTQ GYD " +
		"HKD HNL HTG HUF IDR ILS INR IRR JMD KES KGS KHR KPW KYD KZT LAK LBP LKR LRD LSL MAD MDL MGA MKD MMK " +
		"MNT MOP MRU MUR MVR MWK MXN MYR MZN NAD NGN NIO NOK NPR NZD PAB PEN PGK PHP PKR PLN QAR RON RSD RUB " +
		"SAR SBD SCR SDG SEK SGD SHP SLE SOS SRD SSP STN SVC SYP SZL THB TJS TMT TOP TRY TTD TWD TZS UAH USD " +
		"UYU UZS VES WST XCD YER ZAR ZMW ZWL")
)

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(s) {
		m[c] = true
	}
	return m
}

// allCurrencies lists every supported code, sorted.
func allCurrencies() []string {
	var out []string
	for _, m := range []map[string]bool{zeroDecimal, threeDecimal, twoDecimal} {
		for c := range m {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Exponent returns the number of minor-unit decimals for an uppercase ISO-4217 code.
func Exponent(currency string) (int, error) {
	switch {
	case zeroDecimal[currency]:
		return 0, nil
	case threeDecimal[currency]:
		return 3, nil
	case twoDecimal[currency]:
		return 2, nil
	}
	return 0, ErrUnknownCurrency
}

// ParseDecimal parses a non-negative plain decimal string ("499", "499.5",
// "499.00") into minor units. It rejects signs, spaces, exponents, grouping,
// more fractional digits than the currency allows (zeros beyond the exponent
// are tolerated) and values that overflow int64.
func ParseDecimal(s, currency string) (int64, error) {
	exp, err := Exponent(currency)
	if err != nil {
		return 0, err
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if !allDigits(intPart) || (hasDot && !allDigits(frac)) {
		return 0, ErrInvalidAmount
	}
	if len(frac) > exp {
		if strings.Trim(frac[exp:], "0") != "" {
			return 0, ErrInvalidAmount
		}
		frac = frac[:exp]
	}
	frac += strings.Repeat("0", exp-len(frac))
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil { // out of range; digits are already validated
		return 0, ErrInvalidAmount
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FormatDecimal renders non-negative minor units as a decimal string with
// exactly the currency's number of decimals.
func FormatDecimal(minor int64, currency string) (string, error) {
	exp, err := Exponent(currency)
	if err != nil {
		return "", err
	}
	if minor < 0 {
		return "", ErrInvalidAmount
	}
	d := strconv.FormatInt(minor, 10)
	if exp == 0 {
		return d, nil
	}
	if len(d) <= exp {
		d = strings.Repeat("0", exp-len(d)+1) + d
	}
	return d[:len(d)-exp] + "." + d[len(d)-exp:], nil
}
