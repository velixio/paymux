package dashboard

import (
	"context"
	"encoding/json"
	"time"
)

// zoneChoice is the time_zone setting and the location it stands for.
type zoneChoice struct {
	name string
	loc  *time.Location
}

// location is the time zone of the dashboard's dates and date filters: the
// time_zone setting, else the server's PAYMUX_TZ. It never reads the database,
// because fmtTime runs inside open read transactions, and one more connection
// taken there could wait forever on a full pool.
func (d *Dashboard) location() *time.Location {
	if z := d.zone.Load(); z != nil {
		return z.loc
	}
	return d.o.Location
}

// zoneRefreshWait bounds refreshZone, so a busy connection pool cannot hold up
// a request over a setting that rarely changes.
const zoneRefreshWait = time.Second

// refreshZone picks up the time_zone setting at the start of a request. The
// setting's cache makes this cheap. If it cannot be read, the last zone stays.
func (d *Dashboard) refreshZone(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, zoneRefreshWait)
	defer cancel()
	org, err := d.orgID(ctx)
	if err != nil {
		return
	}
	val, err := d.o.Settings.ResolveOrg(ctx, org, "time_zone")
	if err != nil {
		return
	}
	var name string
	if json.Unmarshal(val.Raw, &name) != nil {
		return
	}
	if z := d.zone.Load(); z != nil && z.name == name {
		return
	}
	loc := d.o.Location
	if name != "server" {
		if loc, err = time.LoadLocation(name); err != nil {
			return
		}
	}
	d.zone.Store(&zoneChoice{name: name, loc: loc})
}
