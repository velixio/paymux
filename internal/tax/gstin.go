// Package tax implements Indian GST rules used for invoicing: GSTIN
// validation, state codes, supply classification, tax amounts and credit-note
// splits. All amounts are int64 minor units (paise); there is no float math.
package tax

import (
	"errors"
	"regexp"
)

const gstinAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// GSTIN layout: 2-digit state, 10-char PAN, entity number, 'Z', checksum.
var gstinShape = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)

var states = map[string]string{
	"01": "Jammu and Kashmir", "02": "Himachal Pradesh", "03": "Punjab", "04": "Chandigarh",
	"05": "Uttarakhand", "06": "Haryana", "07": "Delhi", "08": "Rajasthan", "09": "Uttar Pradesh",
	"10": "Bihar", "11": "Sikkim", "12": "Arunachal Pradesh", "13": "Nagaland", "14": "Manipur",
	"15": "Mizoram", "16": "Tripura", "17": "Meghalaya", "18": "Assam", "19": "West Bengal",
	"20": "Jharkhand", "21": "Odisha", "22": "Chhattisgarh", "23": "Madhya Pradesh", "24": "Gujarat",
	"26": "Dadra and Nagar Haveli and Daman and Diu", "27": "Maharashtra", "29": "Karnataka",
	"30": "Goa", "31": "Lakshadweep", "32": "Kerala", "33": "Tamil Nadu", "34": "Puducherry",
	"35": "Andaman and Nicobar Islands", "36": "Telangana", "37": "Andhra Pradesh", "38": "Ladakh",
	"97": "Other Territory",
}

// StateName returns the GST state name for a 2-digit state code.
func StateName(code string) (string, bool) {
	n, ok := states[code]
	return n, ok
}

// StateCodes returns all valid state codes (for pay-page dropdowns).
func StateCodes() map[string]string {
	out := make(map[string]string, len(states))
	for k, v := range states {
		out[k] = v
	}
	return out
}

// gstinCheck computes the GSTIN check character for the first 14 characters.
func gstinCheck(first14 string) byte {
	sum := 0
	for i := 0; i < 14; i++ {
		v := indexOf(first14[i])
		f := 1
		if i%2 == 1 {
			f = 2
		}
		p := v * f
		sum += p/36 + p%36
	}
	return gstinAlphabet[(36-sum%36)%36]
}

func indexOf(c byte) int {
	for i := 0; i < len(gstinAlphabet); i++ {
		if gstinAlphabet[i] == c {
			return i
		}
	}
	return 0
}

// ErrInvalidGSTIN is returned for malformed GSTINs.
var ErrInvalidGSTIN = errors.New("tax: invalid GSTIN")

// ValidGSTIN checks shape, state code and checksum.
func ValidGSTIN(g string) error {
	if !gstinShape.MatchString(g) {
		return ErrInvalidGSTIN
	}
	if _, ok := states[g[:2]]; !ok {
		return ErrInvalidGSTIN
	}
	if gstinCheck(g[:14]) != g[14] {
		return ErrInvalidGSTIN
	}
	return nil
}

// StateFromGSTIN returns the state code embedded in a GSTIN.
func StateFromGSTIN(g string) string {
	if len(g) < 2 {
		return ""
	}
	return g[:2]
}
