package einvoice

import (
	"regexp"
	"strconv"
	"strings"
)

// The portal wants an address as lines, a locality and a 6-digit PIN. Invoices
// store either those parts (newer ones) or one comma-separated string.
// The split of free text is a best effort (the last comma segment before the
// PIN, minus a trailing state name, is the locality); what cannot be split is
// reported to the operator as a problem to fix, never guessed.

type address struct {
	addr1, addr2, loc string
	pin               int
	problems          []string
}

var (
	pinRE   = regexp.MustCompile(`(^|[^0-9])([1-9][0-9]{5})([^0-9]|$)`)
	spaceRE = regexp.MustCompile(`\s+`)
)

func collapse(s string) string { return strings.TrimSpace(spaceRE.ReplaceAllString(s, " ")) }

func trimSeg(s string) string { return strings.Trim(collapse(s), " .;,") }

func splitAddress(p Party, stateName string) address {
	var a address

	// Structured parts first.
	if pc := strings.TrimSpace(p.PostalCode); pc != "" {
		if n, err := strconv.Atoi(pc); err == nil && pinRE.MatchString(pc) && len(pc) == 6 {
			a.pin = n
		} else {
			a.problems = append(a.problems, "the PIN code "+strconv.Quote(pc)+" is not 6 digits")
		}
	}
	if l1 := collapse(p.Line1); l1 != "" {
		a.addr1 = clip(l1, 100)
		a.addr2 = clip(collapse(p.Line2), 100)
	}
	if city := collapse(p.City); city != "" {
		a.loc = clip(city, 50)
	}

	if a.pin == 0 || a.addr1 == "" || a.loc == "" {
		text := collapse(p.Address)
		if text == "" {
			text = joinNonEmpty(", ", p.Line1, p.Line2, p.City, p.PostalCode)
		}
		pin := 0
		if all := pinRE.FindAllStringSubmatchIndex(text, -1); len(all) > 0 {
			last := all[len(all)-1]
			pin, _ = strconv.Atoi(text[last[4]:last[5]])
			text = text[:last[4]] + " " + text[last[5]:]
		}
		var parts []string
		for _, seg := range strings.Split(text, ",") {
			if seg = trimSeg(seg); seg != "" {
				parts = append(parts, seg)
			}
		}
		if n := len(parts); n >= 2 && stateName != "" && strings.EqualFold(parts[n-1], stateName) {
			parts = parts[:n-1]
		}
		if a.pin == 0 {
			a.pin = pin
		}
		if len(parts) > 0 {
			if a.loc == "" {
				a.loc = clip(parts[len(parts)-1], 50)
			}
			if a.addr1 == "" {
				if len(parts) > 1 {
					a.addr1 = clip(strings.Join(parts[:len(parts)-1], ", "), 100)
				} else {
					a.addr1 = clip(parts[0], 100)
				}
			}
		}
	}
	if a.pin == 0 && len(a.problems) == 0 {
		a.problems = append(a.problems, "it needs a 6-digit PIN code")
	}
	if a.addr1 == "" {
		a.problems = append(a.problems, "it has no address line")
	}
	if len([]rune(a.loc)) < 3 {
		a.problems = append(a.problems, "it has no city or locality of at least 3 characters")
	}
	return a
}
