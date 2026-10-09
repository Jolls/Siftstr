package runs

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
)

// commonZones is the list offered in the timezone dropdown. It is not
// exhaustive: SetTimezone accepts any IANA name, and a saved zone missing
// from this list is still shown, so nothing is lost.
var commonZones = []string{
	"UTC",
	"Africa/Cairo", "Africa/Johannesburg", "Africa/Lagos", "Africa/Nairobi", "Africa/Casablanca", "Africa/Accra",
	"America/Anchorage", "America/Argentina/Buenos_Aires", "America/Bogota", "America/Caracas", "America/Chicago",
	"America/Denver", "America/Halifax", "America/Havana", "America/Lima", "America/Los_Angeles", "America/Mexico_City",
	"America/New_York", "America/Phoenix", "America/Santiago", "America/Sao_Paulo", "America/St_Johns", "America/Toronto",
	"America/Vancouver",
	"Asia/Bangkok", "Asia/Dhaka", "Asia/Dubai", "Asia/Hong_Kong", "Asia/Jakarta", "Asia/Jerusalem", "Asia/Karachi",
	"Asia/Kathmandu", "Asia/Kolkata", "Asia/Manila", "Asia/Riyadh", "Asia/Seoul", "Asia/Shanghai", "Asia/Singapore",
	"Asia/Taipei", "Asia/Tehran", "Asia/Tokyo",
	"Atlantic/Azores", "Atlantic/Reykjavik",
	"Australia/Adelaide", "Australia/Brisbane", "Australia/Darwin", "Australia/Perth", "Australia/Sydney",
	"Europe/Amsterdam", "Europe/Athens", "Europe/Berlin", "Europe/Brussels", "Europe/Bucharest", "Europe/Dublin",
	"Europe/Helsinki", "Europe/Istanbul", "Europe/Kyiv", "Europe/Lisbon", "Europe/London", "Europe/Madrid",
	"Europe/Moscow", "Europe/Oslo", "Europe/Paris", "Europe/Prague", "Europe/Rome", "Europe/Stockholm",
	"Europe/Vienna", "Europe/Warsaw", "Europe/Zurich",
	"Pacific/Auckland", "Pacific/Fiji", "Pacific/Honolulu", "Pacific/Port_Moresby",
}

// ZoneGroup is one region of the timezone dropdown.
type ZoneGroup struct {
	Region string
	Zones  []string
}

// ZoneGroups returns the dropdown options grouped by region. current is added
// if it is a valid zone that the common list lacks, so a saved value always
// has an entry to select.
func ZoneGroups(current string) []ZoneGroup {
	zones := append([]string(nil), commonZones...)
	if loadable(current) && !slices.Contains(zones, current) {
		zones = append(zones, current)
	}
	sort.Strings(zones)
	var out []ZoneGroup
	for _, z := range zones {
		region := "Other"
		if r, _, ok := strings.Cut(z, "/"); ok {
			region = r
		}
		if n := len(out); n > 0 && out[n-1].Region == region {
			out[n-1].Zones = append(out[n-1].Zones, z)
			continue
		}
		out = append(out, ZoneGroup{Region: region, Zones: []string{z}})
	}
	return out
}

// ErrBadTimezone is returned for a name time.LoadLocation rejects.
var ErrBadTimezone = errors.New("unknown timezone; use an IANA name such as Europe/Berlin")

// Timezone returns userID's timezone name.
func (s *Service) Timezone(ctx context.Context, userID string) (string, error) {
	var tz string
	err := s.DB.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id = ?`, userID).Scan(&tz)
	return tz, err
}

// SetTimezone saves userID's timezone. Stored times stay UTC; this only
// decides which day a run belongs to and how times are shown.
func (s *Service) SetTimezone(ctx context.Context, userID, name string) error {
	name = strings.TrimSpace(name)
	if !loadable(name) {
		return ErrBadTimezone
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET timezone = ? WHERE id = ?`, name, userID)
	return err
}
