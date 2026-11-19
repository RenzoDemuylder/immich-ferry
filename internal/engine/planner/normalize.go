package planner

import (
	"math"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // embed the IANA database so distroless images can resolve zones

	"immich-ferry/internal/state"
)

const locEpsilon = 1e-6

// OffsetSeconds returns the UTC offset in seconds of the time zone tz at the
// instant at. tz may be an IANA name, "UTC", "UTC+2", "UTC-03:30" or "+02:00".
// ok is false when tz cannot be interpreted.
func OffsetSeconds(tz string, at time.Time) (int, bool) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return 0, false
	}
	up := strings.ToUpper(tz)
	if up == "UTC" || up == "GMT" || up == "Z" {
		return 0, true
	}
	s := tz
	if strings.HasPrefix(up, "UTC") || strings.HasPrefix(up, "GMT") {
		s = tz[3:]
	}
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		if off, ok := parseOffset(s); ok {
			return off, true
		}
		return 0, false
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return 0, false
	}
	_, off := at.In(loc).Zone()
	return off, true
}

// parseOffset parses +H, +HH, +H:MM, +HH:MM and +HHMM.
func parseOffset(s string) (int, bool) {
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	r := s[1:]
	var hs, ms string
	if i := strings.IndexByte(r, ':'); i >= 0 {
		hs, ms = r[:i], r[i+1:]
	} else if len(r) == 4 {
		hs, ms = r[:2], r[2:]
	} else {
		hs = r
	}
	if hs == "" || len(hs) > 2 || (ms != "" && len(ms) != 2) {
		return 0, false
	}
	h, err := strconv.Atoi(hs)
	if err != nil || h < 0 || h > 14 || strings.ContainsAny(hs, "+-") {
		return 0, false
	}
	m := 0
	if ms != "" {
		m, err = strconv.Atoi(ms)
		if err != nil || m < 0 || m > 59 || strings.ContainsAny(ms, "+-") {
			return 0, false
		}
	}
	return sign * (h*3600 + m*60), true
}

// FormatDate formats t as RFC3339 with a numeric offset; a nil offset means UTC ("Z").
func FormatDate(t time.Time, offsetSec *int) string {
	if offsetSec == nil || *offsetSec == 0 {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return t.In(time.FixedZone("", *offsetSec)).Format(time.RFC3339Nano)
}

// EqualDescription compares descriptions (no trimming; the zero value is "null").
func EqualDescription(a, b string) bool { return a == b }

func offsetOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// EqualDate compares the taken-at instant and the UTC offset (nil offset == 0).
// Both nil are equal; exactly one nil is not.
func EqualDate(t1 *time.Time, o1 *int, t2 *time.Time, o2 *int) bool {
	if t1 == nil || t2 == nil {
		return t1 == nil && t2 == nil
	}
	return t1.Equal(*t2) && offsetOrZero(o1) == offsetOrZero(o2)
}

// EqualLocation compares coordinates with epsilon 1e-6; a missing pair equals a missing pair.
func EqualLocation(lat1, lon1, lat2, lon2 *float64) bool {
	p1 := lat1 != nil && lon1 != nil
	p2 := lat2 != nil && lon2 != nil
	if !p1 || !p2 {
		return !p1 && !p2
	}
	return math.Abs(*lat1-*lat2) < locEpsilon && math.Abs(*lon1-*lon2) < locEpsilon
}

// EqualRating compares ratings by value.
func EqualRating(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// EqualVisibility compares visibility strings.
func EqualVisibility(a, b string) bool { return a == b }

// EqualMeta reports whether all mergeable fields of a and b are equal after normalization.
func EqualMeta(a, b *state.Meta) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return EqualDescription(a.Description, b.Description) &&
		EqualDate(a.TakenAt, a.OffsetSec, b.TakenAt, b.OffsetSec) &&
		EqualLocation(a.Lat, a.Lon, b.Lat, b.Lon) &&
		EqualRating(a.Rating, b.Rating) &&
		EqualVisibility(a.Visibility, b.Visibility)
}

func cloneMeta(m *state.Meta) *state.Meta {
	if m == nil {
		return nil
	}
	c := *m
	if m.TakenAt != nil {
		v := *m.TakenAt
		c.TakenAt = &v
	}
	if m.OffsetSec != nil {
		v := *m.OffsetSec
		c.OffsetSec = &v
	}
	if m.Lat != nil {
		v := *m.Lat
		c.Lat = &v
	}
	if m.Lon != nil {
		v := *m.Lon
		c.Lon = &v
	}
	if m.Rating != nil {
		v := *m.Rating
		c.Rating = &v
	}
	return &c
}
