package dashboard

import (
	"context"
	"slices"
	"strings"

	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/store"
)

// networkView is the Checkout IPs panel: the buyer's IPs at checkout (the
// pay page saw them, or the app sent them), and how the IP the provider saw
// pay compares with them.
type networkView struct {
	Rows []networkRow
	Paid *networkRow
}

type networkRow struct {
	IP, Flag, Place, Org, Seen string
	// IPTone colours the IP: "bad" when the lookup says it is masked,
	// "ok" when it says it is not, "" when it was not looked up.
	IPTone string
	Tags   []networkTag
}

// networkTag is a tag of the panel; Warn draws it as a warning.
type networkTag struct {
	Label, Title string
	Warn         bool
}

func ipRow(ip string, info iprep.Info, known bool) networkRow {
	row := networkRow{IP: ip}
	if !known {
		return row
	}
	var place []string
	for _, part := range []string{info.City, info.Region} {
		if part != "" {
			place = append(place, part)
		}
	}
	flag, name := countryFlag(info.Country)
	if name != "" {
		place = append(place, name)
	}
	row.Flag, row.Place, row.Org = flag, strings.Join(place, ", "), info.Org
	row.IPTone = "ok"
	if kind := info.Kind(); kind != "" {
		row.IPTone = "bad"
		row.Tags = append(row.Tags, networkTag{Label: kind, Title: "The IP lookup says this is a " + kind + " IP: the buyer's real location is hidden", Warn: true})
	}
	return row
}

// buildNetwork returns the panel for payment paymentID, or nil when no IP
// opened its checkout. paidFrom is the IP the provider saw pay, "" if none.
func (d *Dashboard) buildNetwork(ctx context.Context, paymentID, paidFrom string) (*networkView, error) {
	ips, infos, err := d.o.Core.CheckoutIPs(ctx, paymentID)
	if err != nil || len(ips) == 0 {
		return nil, err
	}
	v := &networkView{}
	seen := make([]string, 0, len(ips))
	for _, c := range ips {
		info, known := infos[c.IP]
		row := ipRow(c.IP, info, known)
		row.Seen = d.fmtTimeShort(c.FirstSeen)
		if c.Source == store.CheckoutIPApp {
			row.Seen += ", sent by your app"
		} else {
			row.Seen += ", opened the pay page"
		}
		v.Rows = append(v.Rows, row)
		seen = append(seen, c.IP)
	}
	if paidFrom != "" {
		info, known := d.o.Core.IPInfo(ctx, paidFrom)
		row := ipRow(paidFrom, info, known)
		if slices.Contains(seen, paidFrom) {
			row.Tags = append(row.Tags, networkTag{Label: "Matches checkout", Title: "The card was paid from one of the buyer's checkout IPs"})
		} else {
			row.Tags = append(row.Tags, networkTag{Label: "Not a checkout IP", Title: "The card was paid from an IP the buyer was never seen on", Warn: true})
		}
		v.Paid = &row
	}
	return v, nil
}
