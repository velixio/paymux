package tax

import "strings"

var ones = []string{"Zero", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten",
	"Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen"}
var tens = []string{"", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"}

func below100(n int64) string {
	if n < 20 {
		return ones[n]
	}
	if n%10 == 0 {
		return tens[n/10]
	}
	return tens[n/10] + "-" + ones[n%10]
}

func below1000(n int64) string {
	var parts []string
	if n >= 100 {
		parts = append(parts, ones[n/100], "Hundred")
		n %= 100
	}
	if n > 0 {
		parts = append(parts, below100(n))
	}
	return strings.Join(parts, " ")
}

// indianWords spells n using the Indian system (crore, lakh, thousand).
func indianWords(n int64) string {
	if n == 0 {
		return "Zero"
	}
	var parts []string
	if c := n / 10000000; c > 0 {
		parts = append(parts, indianWords(c), "Crore")
		n %= 10000000
	}
	if l := n / 100000; l > 0 {
		parts = append(parts, below100(l), "Lakh")
		n %= 100000
	}
	if th := n / 1000; th > 0 {
		parts = append(parts, below100(th), "Thousand")
		n %= 1000
	}
	if n > 0 {
		parts = append(parts, below1000(n))
	}
	return strings.Join(parts, " ")
}

// AmountInWordsINR spells a paise amount for the invoice "total in words".
func AmountInWordsINR(paise int64) string {
	if paise < 0 {
		paise = -paise
	}
	s := "Rupees " + indianWords(paise/100)
	if p := paise % 100; p > 0 {
		s += " and " + below100(p) + " Paise"
	}
	return s + " Only"
}
