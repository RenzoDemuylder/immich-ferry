package planner

import (
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// field describes one mergeable metadata field.
type field struct {
	name string
	// exif fields are source-wins while an upload is pending.
	exif bool
	// fill: with no baseline, an empty side is filled from the other.
	fill bool
	// skip reports the field as not comparable for these current values.
	skip func(c0, c1 *state.Meta) bool
	// baseKnown reports whether a baseline has a usable value for the field.
	baseKnown func(b *state.Meta) bool
	eq        func(a, b *state.Meta) bool
	empty     func(m *state.Meta) bool
	// upd adds the write of src's value to u. ok=false means not writable (note explains).
	upd func(u *immich.AssetUpdate, src, dst *state.Meta) (ok bool, note string)
	// set copies src's value into dst's baseline copy.
	set func(dst, src *state.Meta)
}

func never(_, _ *state.Meta) bool { return false }
func always(*state.Meta) bool     { return true }

var fields = []field{
	{
		name: "description", exif: true, fill: true, skip: never, baseKnown: always,
		eq:    func(a, b *state.Meta) bool { return EqualDescription(a.Description, b.Description) },
		empty: func(m *state.Meta) bool { return m.Description == "" },
		upd: func(u *immich.AssetUpdate, src, _ *state.Meta) (bool, string) {
			// UpdateAssetDto.description is non-nullable in v3.4.1; clear with "".
			u.Description = immich.Some(src.Description)
			return true, ""
		},
		set: func(d, s *state.Meta) { d.Description = s.Description },
	},
	{
		name: "date", exif: true, skip: never, baseKnown: always,
		eq:    func(a, b *state.Meta) bool { return EqualDate(a.TakenAt, a.OffsetSec, b.TakenAt, b.OffsetSec) },
		empty: func(m *state.Meta) bool { return m.TakenAt == nil },
		upd: func(u *immich.AssetUpdate, src, _ *state.Meta) (bool, string) {
			if src.TakenAt == nil {
				return false, "unsupported:clear_date"
			}
			u.DateTimeOriginal = immich.Some(FormatDate(*src.TakenAt, src.OffsetSec))
			return true, ""
		},
		set: func(d, s *state.Meta) {
			c := cloneMeta(s)
			d.TakenAt, d.OffsetSec = c.TakenAt, c.OffsetSec
		},
	},
	{
		name: "location", exif: true, fill: true, skip: never, baseKnown: always,
		eq:    func(a, b *state.Meta) bool { return EqualLocation(a.Lat, a.Lon, b.Lat, b.Lon) },
		empty: func(m *state.Meta) bool { return m.Lat == nil || m.Lon == nil },
		upd: func(u *immich.AssetUpdate, src, _ *state.Meta) (bool, string) {
			if src.Lat == nil || src.Lon == nil {
				return false, "unsupported:clear_location"
			}
			u.Latitude = immich.Some(*src.Lat)
			u.Longitude = immich.Some(*src.Lon)
			return true, ""
		},
		set: func(d, s *state.Meta) {
			c := cloneMeta(s)
			d.Lat, d.Lon = c.Lat, c.Lon
		},
	},
	{
		name: "rating", exif: true, fill: true, skip: never, baseKnown: always,
		eq:    func(a, b *state.Meta) bool { return EqualRating(a.Rating, b.Rating) },
		empty: func(m *state.Meta) bool { return m.Rating == nil },
		upd: func(u *immich.AssetUpdate, src, _ *state.Meta) (bool, string) {
			if src.Rating == nil {
				u.Rating = immich.Null[int]()
			} else {
				u.Rating = immich.Some(*src.Rating)
			}
			return true, ""
		},
		set: func(d, s *state.Meta) { d.Rating = cloneMeta(s).Rating },
	},
	{
		name:      "visibility",
		skip:      func(c0, c1 *state.Meta) bool { return c0.Visibility == "" || c1.Visibility == "" },
		baseKnown: func(b *state.Meta) bool { return b.Visibility != "" },
		eq:        func(a, b *state.Meta) bool { return EqualVisibility(a.Visibility, b.Visibility) },
		empty:     func(m *state.Meta) bool { return m.Visibility == "" },
		upd: func(u *immich.AssetUpdate, src, dst *state.Meta) (bool, string) {
			if !writableVisibility(src.Visibility) || !writableVisibility(dst.Visibility) {
				return false, ""
			}
			u.Visibility = immich.Some(immich.Visibility(src.Visibility))
			return true, ""
		},
		set: func(d, s *state.Meta) { d.Visibility = s.Visibility },
	},
}

func writableVisibility(v string) bool {
	return v == string(immich.VisibilityTimeline) || v == string(immich.VisibilityArchive)
}

func sideName(s int) string {
	if s == 0 {
		return "a"
	}
	return "b"
}

// MergeMeta merges per-side metadata using last-observed baselines.
// It returns the updates to apply per side, the new baselines and conflict notes.
func MergeMeta(cur, base [2]*state.Meta, owned [2]bool, winner uint8, noBaseline [2]bool) (upd [2]*immich.AssetUpdate, newBase [2]*state.Meta, conflicts []string) {
	return mergeFields(cur, base, owned, winner, noBaseline, func(*field) bool { return true })
}

func mergeFields(cur, base [2]*state.Meta, owned [2]bool, winner uint8, noBaseline [2]bool, use func(*field) bool) (upd [2]*immich.AssetUpdate, newBase [2]*state.Meta, conflicts []string) {
	newBase = [2]*state.Meta{cloneMeta(cur[0]), cloneMeta(cur[1])}
	if cur[0] == nil || cur[1] == nil {
		return
	}
	for i := range fields {
		f := &fields[i]
		if !use(f) || f.skip(cur[0], cur[1]) {
			continue
		}
		var known [2]bool
		for s := 0; s < 2; s++ {
			known[s] = base[s] != nil && !noBaseline[s] && f.baseKnown(base[s])
		}
		src := -1
		if known[0] && known[1] {
			ch0 := !f.eq(cur[0], base[0])
			ch1 := !f.eq(cur[1], base[1])
			switch {
			case !ch0 && !ch1:
				continue
			case ch0 && !ch1:
				src = 0
			case ch1 && !ch0:
				src = 1
			default:
				if f.eq(cur[0], cur[1]) {
					continue
				}
				src = int(winner)
				conflicts = append(conflicts, "conflict:"+f.name+":winner="+sideName(src))
			}
		} else {
			if f.eq(cur[0], cur[1]) {
				continue
			}
			if !f.fill {
				conflicts = append(conflicts, "diverged:"+f.name)
				continue
			}
			e0, e1 := f.empty(cur[0]), f.empty(cur[1])
			switch {
			case e0 && !e1:
				src = 1
			case e1 && !e0:
				src = 0
			default:
				continue
			}
		}
		dst := 1 - src
		if !owned[dst] {
			conflicts = append(conflicts, "not_owner:"+f.name)
			continue
		}
		u := upd[dst]
		if u == nil {
			u = &immich.AssetUpdate{}
		}
		ok, note := f.upd(u, cur[src], cur[dst])
		if note != "" {
			conflicts = append(conflicts, note)
		}
		if !ok {
			continue
		}
		upd[dst] = u
		f.set(newBase[dst], cur[src])
	}
	return
}

// sourceWinsExif writes the EXIF-derived fields of cur[s] onto side t where they
// differ (pending-upload semantics). wrote reports whether any write was produced.
func sourceWinsExif(cur [2]*state.Meta, owned [2]bool, s, t int, upd *[2]*immich.AssetUpdate, newBase *[2]*state.Meta) (wrote bool, notes []string) {
	for i := range fields {
		f := &fields[i]
		if !f.exif || f.eq(cur[s], cur[t]) {
			continue
		}
		if !owned[t] {
			notes = append(notes, "not_owner:"+f.name)
			continue
		}
		u := upd[t]
		if u == nil {
			u = &immich.AssetUpdate{}
		}
		ok, note := f.upd(u, cur[s], cur[t])
		if note != "" {
			notes = append(notes, note)
		}
		if !ok {
			continue
		}
		upd[t] = u
		f.set(newBase[t], cur[s])
		wrote = true
	}
	return
}
