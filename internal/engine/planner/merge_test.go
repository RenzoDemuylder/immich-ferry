package planner

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

func ip(i int) *int          { return &i }
func fp(f float64) *float64  { return &f }
func bp(b bool) *bool        { return &b }
func tp(s string) *time.Time { t, _ := time.Parse(time.RFC3339, s); return &t }
func meta(f func(m *state.Meta)) *state.Meta {
	m := &state.Meta{Visibility: "timeline"}
	if f != nil {
		f(m)
	}
	return m
}

var allOwned = [2]bool{true, true}

func TestOffsetSeconds(t *testing.T) {
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		tz   string
		at   time.Time
		want int
		ok   bool
	}{
		{"Europe/Brussels", winter, 3600, true},
		{"Europe/Brussels", summer, 7200, true},
		{"UTC+1", winter, 3600, true},
		{"UTC+2", summer, 7200, true},
		{"UTC-03:30", winter, -12600, true},
		{"+02:00", winter, 7200, true},
		{"-0530", winter, -19800, true},
		{"UTC", winter, 0, true},
		{"", winter, 0, false},
		{"Not/AZone", winter, 0, false},
		{"UTC+99", winter, 0, false},
	}
	for _, tc := range tests {
		got, ok := OffsetSeconds(tc.tz, tc.at)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %d,%v want %d,%v", tc.tz, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFormatDate(t *testing.T) {
	ts := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	if got := FormatDate(ts, nil); got != "2026-07-15T10:00:00Z" {
		t.Fatal(got)
	}
	if got := FormatDate(ts, ip(7200)); got != "2026-07-15T12:00:00+02:00" {
		t.Fatal(got)
	}
	if got := FormatDate(ts, ip(-12600)); got != "2026-07-15T06:30:00-03:30" {
		t.Fatal(got)
	}
}

func TestEquality(t *testing.T) {
	a := tp("2026-07-15T10:00:00Z")
	b := tp("2026-07-15T12:00:00+02:00")
	if !EqualDate(a, nil, b, ip(0)) {
		t.Error("instant + nil offset == 0")
	}
	if EqualDate(a, ip(0), b, ip(7200)) {
		t.Error("offset differs")
	}
	if EqualDate(a, nil, nil, nil) || !EqualDate(nil, nil, nil, ip(5)) {
		t.Error("nil handling")
	}
	if !EqualLocation(fp(1), fp(2), fp(1.0000001), fp(2)) || EqualLocation(fp(1), fp(2), fp(1.00001), fp(2)) {
		t.Error("loc eps")
	}
	if !EqualLocation(nil, nil, nil, fp(1)) || EqualLocation(fp(1), fp(1), nil, nil) {
		t.Error("loc nil")
	}
	if !EqualRating(nil, nil) || EqualRating(ip(1), nil) || !EqualRating(ip(3), ip(3)) {
		t.Error("rating")
	}
	if !EqualDescription("", "") || EqualDescription("a", "a ") {
		t.Error("desc")
	}
}

type fcase struct {
	name       string
	set        func(m *state.Meta, v int)
	val        [3]int // distinct variants 0..2
	checkUpd   func(t *testing.T, u *immich.AssetUpdate, v int)
	fieldName  string
	fillsEmpty bool
}

func variants() []fcase {
	return []fcase{
		{name: "description", fieldName: "description", fillsEmpty: true,
			set: func(m *state.Meta, v int) { m.Description = []string{"", "one", "two"}[v] },
			checkUpd: func(t *testing.T, u *immich.AssetUpdate, v int) {
				t.Helper()
				// description is non-nullable in UpdateAssetDto: clearing sends "".
				want := immich.Some([]string{"", "one", "two"}[v])
				if !reflect.DeepEqual(u.Description, want) {
					t.Errorf("description %+v want %+v", u.Description, want)
				}
			}},
		{name: "date", fieldName: "date",
			set: func(m *state.Meta, v int) {
				m.TakenAt = []*time.Time{tp("2026-01-01T10:00:00Z"), tp("2026-02-01T10:00:00Z"), tp("2026-03-01T10:00:00Z")}[v]
				m.OffsetSec = ip(3600 * v)
			},
			checkUpd: func(t *testing.T, u *immich.AssetUpdate, v int) {
				t.Helper()
				if !u.DateTimeOriginal.Set || u.DateTimeOriginal.Val == nil {
					t.Fatal("date not set")
				}
				want := []string{"2026-01-01T10:00:00Z", "2026-02-01T11:00:00+01:00", "2026-03-01T12:00:00+02:00"}[v]
				if *u.DateTimeOriginal.Val != want {
					t.Errorf("date %q want %q", *u.DateTimeOriginal.Val, want)
				}
			}},
		{name: "location", fieldName: "location", fillsEmpty: true,
			set: func(m *state.Meta, v int) {
				if v == 0 {
					m.Lat, m.Lon = nil, nil
					return
				}
				m.Lat, m.Lon = fp(float64(v)), fp(float64(v)*2)
			},
			checkUpd: func(t *testing.T, u *immich.AssetUpdate, v int) {
				t.Helper()
				if !u.Latitude.Set || !u.Longitude.Set || *u.Latitude.Val != float64(v) || *u.Longitude.Val != float64(v)*2 {
					t.Errorf("loc %+v %+v", u.Latitude, u.Longitude)
				}
			}},
		{name: "rating", fieldName: "rating", fillsEmpty: true,
			set: func(m *state.Meta, v int) {
				if v == 0 {
					m.Rating = nil
				} else {
					m.Rating = ip(v)
				}
			},
			checkUpd: func(t *testing.T, u *immich.AssetUpdate, v int) {
				t.Helper()
				want := immich.Some(v)
				if v == 0 {
					want = immich.Null[int]()
				}
				if !reflect.DeepEqual(u.Rating, want) {
					t.Errorf("rating %+v want %+v", u.Rating, want)
				}
			}},
		{name: "visibility", fieldName: "visibility",
			set: func(m *state.Meta, v int) { m.Visibility = []string{"timeline", "archive", "timeline"}[v] },
			checkUpd: func(t *testing.T, u *immich.AssetUpdate, v int) {
				t.Helper()
				want := []immich.Visibility{"timeline", "archive", "timeline"}[v]
				if !u.Visibility.Set || *u.Visibility.Val != want {
					t.Errorf("vis %+v", u.Visibility)
				}
			}},
	}
}

// countFields returns how many distinct fields an update touches.
func onlyField(t *testing.T, u *immich.AssetUpdate, name string) {
	t.Helper()
	got := []string{}
	if u.Description.Set {
		got = append(got, "description")
	}
	if u.DateTimeOriginal.Set {
		got = append(got, "date")
	}
	if u.Latitude.Set {
		got = append(got, "location")
	}
	if u.Rating.Set {
		got = append(got, "rating")
	}
	if u.IsFavorite.Set {
		got = append(got, "favorite")
	}
	if u.Visibility.Set {
		got = append(got, "visibility")
	}
	if len(got) != 1 || got[0] != name {
		t.Errorf("update fields %v, want only %s", got, name)
	}
}

func mk(f fcase, v int) *state.Meta {
	m := meta(nil)
	f.set(m, v)
	return m
}

func TestMergeMetaPerField(t *testing.T) {
	for _, f := range variants() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			// baseline = variant 1 on both; A changes to 2.
			for s := 0; s < 2; s++ {
				cur := [2]*state.Meta{mk(f, 1), mk(f, 1)}
				base := [2]*state.Meta{mk(f, 1), mk(f, 1)}
				cur[s] = mk(f, 2)
				upd, nb, conf := MergeMeta(cur, base, allOwned, 0, [2]bool{})
				o := 1 - s
				if upd[s] != nil || upd[o] == nil || len(conf) != 0 {
					t.Fatalf("only side %d changed: %+v %v", s, upd, conf)
				}
				onlyField(t, upd[o], f.fieldName)
				f.checkUpd(t, upd[o], 2)
				if !EqualMeta(nb[o], cur[s]) || !EqualMeta(nb[s], cur[s]) {
					t.Errorf("newBase %+v %+v", nb[0], nb[1])
				}
				// not owned target
				upd, _, conf = MergeMeta(cur, base, [2]bool{s == 0, s == 1}, 0, [2]bool{})
				if upd[0] != nil || upd[1] != nil || len(conf) != 1 || conf[0] != "not_owner:"+f.fieldName {
					t.Errorf("not owner: %+v %v", upd, conf)
				}
			}
			// both changed same
			cur := [2]*state.Meta{mk(f, 2), mk(f, 2)}
			base := [2]*state.Meta{mk(f, 1), mk(f, 1)}
			if upd, _, conf := MergeMeta(cur, base, allOwned, 0, [2]bool{}); upd[0] != nil || upd[1] != nil || len(conf) != 0 {
				t.Errorf("both same: %+v %v", upd, conf)
			}
			// both changed different: winner a / b. Use variants 2 and 0 vs base 1.
			cur = [2]*state.Meta{mk(f, 2), mk(f, 0)}
			for _, w := range []uint8{0, 1} {
				if f.name == "visibility" {
					break // two-valued: both changed always means equal
				}
				upd, _, conf := MergeMeta(cur, base, allOwned, w, [2]bool{})
				loser := 1 - int(w)
				if f.name == "location" {
					// winner b is empty: clearing unsupported when b wins
					if w == 1 {
						if upd[0] != nil || upd[1] != nil || !contains(conf, "unsupported:clear_location") {
							t.Errorf("clear loc: %+v %v", upd, conf)
						}
						continue
					}
				}
				if upd[1-loser] != nil || upd[loser] == nil {
					t.Fatalf("winner %d: %+v", w, upd)
				}
				onlyField(t, upd[loser], f.fieldName)
				f.checkUpd(t, upd[loser], []int{2, 0}[w])
				if !contains(conf, "conflict:"+f.fieldName+":winner="+sideName(int(w))) {
					t.Errorf("conf %v", conf)
				}
			}
			// no baseline
			cur = [2]*state.Meta{mk(f, 2), mk(f, 1)}
			upd, _, conf := MergeMeta(cur, [2]*state.Meta{}, allOwned, 0, [2]bool{true, true})
			if upd[0] != nil || upd[1] != nil || !contains(conf, "diverged:"+f.fieldName) {
				if f.fillsEmpty {
					if len(conf) != 0 {
						t.Errorf("fill-empty: both set must not write: %v", conf)
					}
				} else {
					t.Errorf("no baseline %+v %v", upd, conf)
				}
			}
			if f.fillsEmpty {
				cur = [2]*state.Meta{mk(f, 0), mk(f, 2)}
				upd, nb, _ := MergeMeta(cur, [2]*state.Meta{}, allOwned, 1, [2]bool{true, true})
				if upd[1] != nil || upd[0] == nil {
					t.Fatalf("fill-empty: %+v", upd)
				}
				onlyField(t, upd[0], f.fieldName)
				f.checkUpd(t, upd[0], 2)
				if !EqualMeta(nb[0], nb[1]) {
					t.Error("newBase should converge")
				}
				// not owned target -> no fill
				upd, _, conf = MergeMeta(cur, [2]*state.Meta{}, [2]bool{false, true}, 1, [2]bool{true, true})
				if upd[0] != nil || !contains(conf, "not_owner:"+f.fieldName) {
					t.Errorf("fill not-owner %v", conf)
				}
			}
			// noBaseline due to MetaID mismatch only on one side -> not both known
			cur = [2]*state.Meta{mk(f, 2), mk(f, 1)}
			base = [2]*state.Meta{mk(f, 1), mk(f, 1)}
			upd, _, conf = MergeMeta(cur, base, allOwned, 0, [2]bool{false, true})
			if f.fillsEmpty {
				if upd[0] != nil || upd[1] != nil {
					t.Errorf("one side lacking baseline, both set: %+v", upd)
				}
			} else if upd[0] != nil || upd[1] != nil || !contains(conf, "diverged:"+f.fieldName) {
				t.Errorf("mismatch %+v %v", upd, conf)
			}
		})
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestMergeMetaVisibilityOnlyWritable(t *testing.T) {
	cur := [2]*state.Meta{meta(func(m *state.Meta) { m.Visibility = "hidden" }), meta(nil)}
	base := [2]*state.Meta{meta(nil), meta(nil)}
	upd, _, _ := MergeMeta(cur, base, allOwned, 0, [2]bool{})
	if upd[1] != nil || upd[0] != nil {
		t.Fatalf("hidden must not propagate: %+v", upd)
	}
}

func TestMergeMetaClearLocationUnsupported(t *testing.T) {
	cur := [2]*state.Meta{meta(nil), meta(func(m *state.Meta) { m.Lat, m.Lon = fp(1), fp(2) })}
	base := [2]*state.Meta{meta(func(m *state.Meta) { m.Lat, m.Lon = fp(1), fp(2) }), meta(func(m *state.Meta) { m.Lat, m.Lon = fp(1), fp(2) })}
	upd, nb, conf := MergeMeta(cur, base, allOwned, 0, [2]bool{})
	if upd[0] != nil || upd[1] != nil || !contains(conf, "unsupported:clear_location") {
		t.Fatalf("%+v %v", upd, conf)
	}
	if nb[0].Lat != nil || nb[1].Lat == nil {
		t.Error("newBase must equal cur when unwritten")
	}
}

func TestMergeMetaTZEquivalence(t *testing.T) {
	// B stores zone as UTC+N, A as IANA: normalized offsets are equal -> no diff.
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	offA, _ := OffsetSeconds("Europe/Brussels", at)
	offB, _ := OffsetSeconds("UTC+1", at)
	mkm := func(off int) *state.Meta { return meta(func(m *state.Meta) { m.TakenAt = &at; m.OffsetSec = &off }) }
	cur := [2]*state.Meta{mkm(offA), mkm(offB)}
	base := [2]*state.Meta{mkm(offA), mkm(offB)}
	if upd, _, conf := MergeMeta(cur, base, allOwned, 0, [2]bool{}); upd[0] != nil || upd[1] != nil || len(conf) != 0 {
		t.Fatal(upd, conf)
	}
	// Summer: Brussels = +2, UTC+1 differs.
	sum := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	o1, _ := OffsetSeconds("Europe/Brussels", sum)
	o2, _ := OffsetSeconds("UTC+1", sum)
	if o1 != 7200 || o2 != 3600 || EqualDate(&sum, &o1, &sum, &o2) {
		t.Fatal("summer offsets should differ")
	}
}

func TestMergeMetaNilCur(t *testing.T) {
	upd, nb, conf := MergeMeta([2]*state.Meta{nil, meta(nil)}, [2]*state.Meta{}, allOwned, 0, [2]bool{true, true})
	if upd[0] != nil || upd[1] != nil || nb[0] != nil || nb[1] == nil || conf != nil {
		t.Fatal("nil cur")
	}
}

// Ping-pong property: applying the returned updates to cur then merging again
// with newBase yields no updates.
func TestMergeMetaNoPingPong(t *testing.T) {
	fs := variants()
	apply := func(m *state.Meta, u *immich.AssetUpdate, src *state.Meta) {
		if u == nil {
			return
		}
		c := cloneMeta(src)
		if u.Description.Set {
			m.Description = c.Description
		}
		if u.DateTimeOriginal.Set {
			m.TakenAt, m.OffsetSec = c.TakenAt, c.OffsetSec
		}
		if u.Latitude.Set {
			m.Lat, m.Lon = c.Lat, c.Lon
		}
		if u.Rating.Set {
			m.Rating = c.Rating
		}
		if u.Visibility.Set {
			m.Visibility = c.Visibility
		}
	}
	for _, withBase := range []bool{true, false} {
		for ca := 0; ca < 3; ca++ {
			for cb := 0; cb < 3; cb++ {
				for ba := 0; ba < 3; ba++ {
					for bb := 0; bb < 3; bb++ {
						for _, w := range []uint8{0, 1} {
							// vary all fields together with different variants by field index
							mkAll := func(v int) *state.Meta {
								m := meta(nil)
								for i, f := range fs {
									f.set(m, (v+i)%3)
								}
								return m
							}
							cur := [2]*state.Meta{mkAll(ca), mkAll(cb)}
							var base [2]*state.Meta
							noBase := [2]bool{true, true}
							if withBase {
								base = [2]*state.Meta{mkAll(ba), mkAll(bb)}
								noBase = [2]bool{}
							}
							upd, nb, _ := MergeMeta(cur, base, allOwned, w, noBase)
							next := [2]*state.Meta{cloneMeta(cur[0]), cloneMeta(cur[1])}
							apply(next[0], upd[0], cur[1])
							apply(next[1], upd[1], cur[0])
							upd2, _, _ := MergeMeta(next, nb, allOwned, w, [2]bool{})
							if upd2[0] != nil || upd2[1] != nil {
								t.Fatalf("ping-pong: cur=%v/%v base=%v/%v w=%d withBase=%v upd2=%+v", ca, cb, ba, bb, w, withBase, upd2)
							}
						}
					}
				}
			}
		}
	}
	_ = strings.ToLower
}
