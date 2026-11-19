package planner

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func mem(id string, f func(m *Member)) Member {
	m := Member{Ref: state.AssetRef{ID: id, Checksum: "c-" + id}, Owned: true, Meta: *meta(nil), Extracted: true, Visibility: immich.VisibilityTimeline}
	if f != nil {
		f(&m)
	}
	return m
}

func snap(id string, keys ...string) Snapshot {
	s := Snapshot{AlbumID: id, Consistent: true, Groups: map[Key][]Member{}}
	for _, k := range keys {
		s.Groups[Key(k)] = []Member{mem(id+"-"+k, nil)}
	}
	return s
}

func opts() Options {
	return Options{PropagateRemovals: true, MaxRemovalRatio: 0.2, MaxRemovalsMin: 10, SyncMetadata: true, SyncAlbumInfo: true,
		BlockedRecheck: 24 * time.Hour, PendingMetaTimeout: time.Hour}
}

func stateWith(inS ...string) *state.PairState {
	st := state.NewPairState("p")
	for _, k := range inS {
		st.Item(k).InBoth = true
	}
	return st
}

func keysOf(as []Action) []string {
	var out []string
	for _, a := range as {
		out = append(out, string(a.Key))
	}
	return out
}

func TestTruthTable(t *testing.T) {
	tests := []struct {
		name                    string
		inA, inB, inS, propag   bool
		copies, removals, drops int
		paired                  bool
		copyFrom                uint8
		probe, unlink           uint8
	}{
		{name: "1,1,0", inA: true, inB: true, propag: true, paired: true},
		{name: "1,1,1", inA: true, inB: true, inS: true, propag: true, paired: true},
		{name: "1,0,0", inA: true, propag: true, copies: 1, copyFrom: 0},
		{name: "0,1,0", inB: true, propag: true, copies: 1, copyFrom: 1},
		{name: "1,0,1", inA: true, inS: true, propag: true, removals: 1, probe: 1, unlink: 0},
		{name: "0,1,1", inB: true, inS: true, propag: true, removals: 1, probe: 0, unlink: 1},
		{name: "0,0,1", inS: true, propag: true, drops: 1},
		{name: "0,0,0", propag: true},
		{name: "1,0,1 no propagate", inA: true, inS: true},
		{name: "0,1,1 no propagate", inB: true, inS: true},
		{name: "0,0,1 no propagate still drops", inS: true, drops: 1},
		{name: "1,0,0 no propagate still copies", inA: true, copies: 1, copyFrom: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := snap("A"), snap("B")
			if tc.inA {
				a = snap("A", "k")
			}
			if tc.inB {
				b = snap("B", "k")
			}
			st := state.NewPairState("p")
			if tc.inS {
				st.Item("k").InBoth = true
			}
			o := opts()
			o.PropagateRemovals = tc.propag
			p := Build(a, b, st, o, t0)
			if len(p.Copies) != tc.copies || len(p.Removals) != tc.removals || len(p.Drops) != tc.drops || (len(p.Paired) == 1) != tc.paired {
				t.Fatalf("plan %+v", p)
			}
			if tc.copies == 1 {
				c := p.Copies[0]
				src := []string{"A", "B"}[tc.copyFrom]
				if c.Kind != ActionCopy || c.From != tc.copyFrom || c.To != 1-tc.copyFrom || c.TargetID != "" ||
					len(c.Refs) != 1 || c.Refs[0].ID != src+"-k" {
					t.Fatalf("copy %+v", c)
				}
			}
			if tc.removals == 1 {
				r := p.Removals[0]
				srcSide := []string{"A", "B"}[tc.unlink]
				if r.Kind != ActionProbeUnlink || r.From != tc.probe || r.To != tc.unlink || r.Refs[0].ID != srcSide+"-k" {
					t.Fatalf("removal %+v", r)
				}
				if p.RemovalCandidates[tc.unlink] != 1 {
					t.Fatalf("candidates %v", p.RemovalCandidates)
				}
			}
		})
	}
}

func TestEmptySNeverRemoves(t *testing.T) {
	p := Build(snap("A", "x", "y"), snap("B"), state.NewPairState("p"), opts(), t0)
	if len(p.Removals) != 0 || len(p.Copies) != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestGuardBoundary(t *testing.T) {
	// |S| = 100, ratio 0.2 → threshold max(10, 20) = 20.
	build := func(nRemove int, allow bool) Plan {
		var inS []string
		var aKeys []string
		for i := 0; i < 100; i++ {
			k := fmt.Sprintf("k%03d", i)
			inS = append(inS, k)
			if i >= nRemove { // removed keys vanish from B but stay in A → unlink from A
				aKeys = append(aKeys, k)
			}
		}
		var allA []string
		allA = append(allA, inS...)
		a := snap("A", allA...)
		b := snap("B", aKeys...)
		o := opts()
		o.AllowMassRemoval = allow
		return Build(a, b, stateWith(inS...), o, t0)
	}
	if p := build(20, false); p.GuardTripped[0] || len(p.Removals) != 20 {
		t.Fatalf("at threshold: %+v", p.GuardTripped)
	}
	p := build(21, false)
	if !p.GuardTripped[0] || p.GuardTripped[1] || len(p.Removals) != 0 || p.Skipped["guard"] != 21 || len(p.Warnings) == 0 || p.RemovalCandidates[0] != 21 {
		t.Fatalf("over threshold: %+v", p)
	}
	if p := build(21, true); p.GuardTripped[0] || len(p.Removals) != 21 {
		t.Fatalf("allow: %+v", p)
	}
	// min floor: |S|=5 all removed → 5 <= 10
	a := snap("A", "a", "b", "c", "d", "e")
	if p := Build(a, snap("B"), stateWith("a", "b", "c", "d", "e"), opts(), t0); p.GuardTripped[0] || len(p.Removals) != 5 {
		t.Fatalf("floor %+v", p)
	}
}

func TestGuardDropsRemovalsBothSides(t *testing.T) {
	var inS, aOnly, bOnly []string
	for i := 0; i < 12; i++ {
		k := fmt.Sprintf("a%02d", i)
		inS = append(inS, k)
		aOnly = append(aOnly, k) // missing in B → unlink from A: 12 > 10
	}
	inS = append(inS, "bx")
	bOnly = append(bOnly, "bx") // one on the other side
	p := Build(snap("A", aOnly...), snap("B", bOnly...), stateWith(inS...), opts(), t0)
	if !p.GuardTripped[0] || p.GuardTripped[1] || len(p.Removals) != 0 {
		t.Fatalf("%+v", p)
	}
	if p.RemovalCandidates[1] != 1 {
		t.Fatalf("%v", p.RemovalCandidates)
	}
}

func TestBlocked(t *testing.T) {
	blocked := func(since time.Time) *state.PairState {
		st := stateWith()
		st.Item("k").Blocked = &state.Blocked{Op: "copy", Side: 1, Reason: "target_hidden", Since: since}
		return st
	}
	// active block: copy skipped
	p := Build(snap("A", "k"), snap("B"), blocked(t0.Add(-time.Hour)), opts(), t0)
	if len(p.Copies) != 0 || p.Skipped["blocked"] != 1 {
		t.Fatalf("%+v", p)
	}
	// expired: treated normally
	p = Build(snap("A", "k"), snap("B"), blocked(t0.Add(-25*time.Hour)), opts(), t0)
	if len(p.Copies) != 1 || p.Skipped["blocked"] != 0 {
		t.Fatalf("expired %+v", p)
	}
	// exactly at recheck is not yet expired
	p = Build(snap("A", "k"), snap("B"), blocked(t0.Add(-24*time.Hour)), opts(), t0)
	if len(p.Copies) != 0 {
		t.Fatal("boundary")
	}
	// row change to (1,1): paired normally
	p = Build(snap("A", "k"), snap("B", "k"), blocked(t0), opts(), t0)
	if len(p.Paired) != 1 || p.Skipped["blocked"] != 0 {
		t.Fatalf("paired %+v", p)
	}
	// row change to (0,0) with S: drop
	st := stateWith("k")
	st.Item("k").Blocked = &state.Blocked{Op: "unlink", Since: t0}
	p = Build(snap("A"), snap("B"), st, opts(), t0)
	if len(p.Drops) != 1 {
		t.Fatalf("drop %+v", p)
	}
	// blocked removal candidate stays in S, not counted, no action
	st = stateWith("k")
	st.Item("k").Blocked = &state.Blocked{Op: "unlink", Since: t0}
	p = Build(snap("A", "k"), snap("B"), st, opts(), t0)
	if len(p.Removals) != 0 || p.Skipped["blocked"] != 1 || p.RemovalCandidates[0] != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestBackoff(t *testing.T) {
	st := stateWith()
	st.Item("k").Failure = &state.Failure{Op: "copy", Count: 2, Next: t0.Add(time.Minute)}
	p := Build(snap("A", "k"), snap("B"), st, opts(), t0)
	if len(p.Copies) != 0 || p.Skipped["backoff"] != 1 {
		t.Fatalf("%+v", p)
	}
	p = Build(snap("A", "k"), snap("B"), st, opts(), t0.Add(2*time.Minute))
	if len(p.Copies) != 1 {
		t.Fatal("backoff should expire")
	}
	st = stateWith("k")
	st.Item("k").Failure = &state.Failure{Op: "unlink", Count: 1, Next: t0.Add(time.Minute)}
	p = Build(snap("A", "k"), snap("B"), st, opts(), t0)
	if len(p.Removals) != 0 || p.Skipped["backoff"] != 1 {
		t.Fatalf("removal backoff %+v", p)
	}
}

func pendingCase(extracted bool, since time.Time) (Plan, *state.PairState) {
	a := snap("A", "k")
	b := snap("B", "k")
	a.Groups["k"][0].Meta.Description = "src desc"
	a.Groups["k"][0].Meta.Rating = ip(4)
	a.Groups["k"][0].Meta.Visibility = "archive"
	b.Groups["k"][0].Extracted = extracted
	st := stateWith("k")
	it := st.Item("k")
	it.Pending = &state.Pending{To: 1, TargetID: "B-k", Since: since}
	it.Meta = [2]*state.Meta{meta(nil), meta(nil)}
	it.MetaID = [2]string{"A-k", "B-k"}
	return Build(a, b, st, opts(), t0), st
}

func TestPending(t *testing.T) {
	// waiting
	p, _ := pendingCase(false, t0.Add(-time.Minute))
	if p.Pending != 1 || len(p.Meta) != 0 || len(p.Paired) != 1 {
		t.Fatalf("wait %+v", p)
	}
	// timeout: source wins on EXIF, visibility normal merge (A changed it → B)
	p, _ = pendingCase(false, t0.Add(-2*time.Hour))
	if p.Pending != 0 || len(p.Meta) != 1 {
		t.Fatalf("timeout %+v", p)
	}
	m := p.Meta[0]
	u := m.Updates[1]
	if u == nil || m.Updates[0] != nil || !u.Description.Set || *u.Description.Val != "src desc" || !u.Rating.Set || !u.Visibility.Set || u.IsFavorite.Set || m.ClearPending {
		t.Fatalf("%+v", m)
	}
	// extracted: same
	p, _ = pendingCase(true, t0.Add(-time.Minute))
	if p.Pending != 0 || len(p.Meta) != 1 || p.Meta[0].Updates[1] == nil {
		t.Fatalf("extracted %+v", p)
	}
	// clear: no EXIF diff remains
	a, b := snap("A", "k"), snap("B", "k")
	st := stateWith("k")
	st.Item("k").Pending = &state.Pending{To: 1, Since: t0.Add(-2 * time.Hour)}
	st.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
	st.Item("k").MetaID = [2]string{"A-k", "B-k"}
	p = Build(a, b, st, opts(), t0)
	if len(p.Meta) != 1 || !p.Meta[0].ClearPending || p.Meta[0].Updates[0] != nil || p.Meta[0].Updates[1] != nil {
		t.Fatalf("clear %+v", p.Meta)
	}
	// EXIF source-wins ignores baselines of target: target has own description → overwritten
	a, b = snap("A", "k"), snap("B", "k")
	a.Groups["k"][0].Meta.Description = "mine"
	b.Groups["k"][0].Meta.Description = "extracted"
	st = stateWith("k")
	st.Item("k").Pending = &state.Pending{To: 1, Since: t0.Add(-2 * time.Hour)}
	p = Build(a, b, st, opts(), t0)
	if len(p.Meta) != 1 || p.Meta[0].Updates[1] == nil || *p.Meta[0].Updates[1].Description.Val != "mine" {
		t.Fatalf("source wins %+v", p.Meta)
	}
}

func TestPlannerDoesNotMutateState(t *testing.T) {
	a, b := snap("A", "k", "x", "y"), snap("B", "k", "z")
	a.Groups["k"][0].Meta.Description = "d"
	st := stateWith("k", "z", "gone")
	st.Item("k").Pending = &state.Pending{To: 1, Since: t0.Add(-2 * time.Hour)}
	st.Item("x").Failure = &state.Failure{Op: "copy", Next: t0.Add(time.Hour)}
	st.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
	st.AlbumInfo[0] = &state.AlbumInfo{Name: "a"}
	a.AlbumName, b.AlbumName = "a2", "a"
	want := deepCopy(st)
	_ = Build(a, b, st, opts(), t0)
	if !reflect.DeepEqual(want, st) {
		t.Fatal("state mutated")
	}
}

func deepCopy(st *state.PairState) *state.PairState {
	c := *st
	c.Items = map[string]*state.Item{}
	for k, it := range st.Items {
		i := *it
		for s := 0; s < 2; s++ {
			i.Meta[s] = cloneMeta(it.Meta[s])
			i.Refs[s] = append([]state.AssetRef(nil), it.Refs[s]...)
		}
		if it.Pending != nil {
			p := *it.Pending
			i.Pending = &p
		}
		if it.Failure != nil {
			f := *it.Failure
			i.Failure = &f
		}
		c.Items[k] = &i
	}
	for s := 0; s < 2; s++ {
		if st.AlbumInfo[s] != nil {
			x := *st.AlbumInfo[s]
			c.AlbumInfo[s] = &x
		}
	}
	return &c
}

func TestLinkRepair(t *testing.T) {
	mk := func(srcLive, tgtLive string, tgtOwned bool) Plan {
		a, b := snap("A", "k"), snap("B", "k")
		a.Groups["k"][0].LivePhotoVideoID = srcLive
		b.Groups["k"][0].LivePhotoVideoID = tgtLive
		b.Groups["k"][0].Owned = tgtOwned
		st := stateWith("k")
		st.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
		st.Item("k").MetaID = [2]string{"A-k", "B-k"}
		return Build(a, b, st, opts(), t0)
	}
	if p := mk("v1", "", true); len(p.Meta) != 1 || p.Meta[0].Kind != ActionLinkRepair || p.Meta[0].From != 0 || p.Meta[0].To != 1 || p.Meta[0].TargetID != "B-k" {
		t.Fatalf("%+v", p.Meta)
	}
	if p := mk("v1", "", false); len(p.Meta) != 0 {
		t.Fatal("target not owned")
	}
	if p := mk("v1", "v2", true); len(p.Meta) != 0 {
		t.Fatal("already linked")
	}
	if p := mk("", "", true); len(p.Meta) != 0 {
		t.Fatal("no motion")
	}
	// reverse direction
	a, b := snap("A", "k"), snap("B", "k")
	b.Groups["k"][0].LivePhotoVideoID = "v"
	st := stateWith("k")
	st.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
	st.Item("k").MetaID = [2]string{"A-k", "B-k"}
	if p := Build(a, b, st, opts(), t0); len(p.Meta) != 1 || p.Meta[0].From != 1 || p.Meta[0].TargetID != "A-k" {
		t.Fatalf("%+v", p.Meta)
	}
}

func TestPairedMeta(t *testing.T) {
	a, b := snap("A", "k", "n"), snap("B", "k", "n")
	a.Groups["k"][0].Meta.Description = "edited"
	st := stateWith("k", "n")
	for _, k := range []string{"k", "n"} {
		st.Item(k).Meta = [2]*state.Meta{meta(nil), meta(nil)}
		st.Item(k).MetaID = [2]string{"A-" + k, "B-" + k}
	}
	// "n": unchanged everywhere → no action
	p := Build(a, b, st, opts(), t0)
	if got := keysOf(p.Meta); !reflect.DeepEqual(got, []string{"k"}) {
		t.Fatalf("%v", got)
	}
	m := p.Meta[0]
	if m.Updates[1] == nil || *m.Updates[1].Description.Val != "edited" || m.TargetIDs[1] != "B-k" || m.NewBase[1].Description != "edited" || m.NewBaseIDs != [2]string{"A-k", "B-k"} {
		t.Fatalf("%+v", m)
	}
	// SyncMetadata false → no meta actions, still paired
	o := opts()
	o.SyncMetadata = false
	if p := Build(a, b, st, o, t0); len(p.Meta) != 0 || len(p.Paired) != 2 {
		t.Fatal("syncmetadata off")
	}
	// No baseline (new pairing): action recording baselines even with no writes
	st2 := stateWith()
	p = Build(snap("A", "k"), snap("B", "k"), st2, opts(), t0)
	if len(p.Meta) != 1 || p.Meta[0].Updates[0] != nil || p.Meta[0].NewBase[0] == nil {
		t.Fatalf("baseline record %+v", p.Meta)
	}
	// MetaID mismatch → noBaseline: date differs → diverged, no write, baselines rebound
	a, b = snap("A", "k"), snap("B", "k")
	a.Groups["k"][0].Meta.Visibility = "archive"
	st3 := stateWith("k")
	st3.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
	st3.Item("k").MetaID = [2]string{"A-old", "B-k"}
	p = Build(a, b, st3, opts(), t0)
	if len(p.Meta) != 1 || p.Meta[0].Updates[0] != nil || p.Meta[0].Updates[1] != nil || p.Meta[0].NewBaseIDs[0] != "A-k" || len(p.Warnings) == 0 {
		t.Fatalf("%+v %v", p.Meta, p.Warnings)
	}
	// not_owner skip count
	a, b = snap("A", "k"), snap("B", "k")
	a.Groups["k"][0].Meta.Description = "x"
	b.Groups["k"][0].Owned = false
	st4 := stateWith("k")
	st4.Item("k").Meta = [2]*state.Meta{meta(nil), meta(nil)}
	st4.Item("k").MetaID = [2]string{"A-k", "B-k"}
	p = Build(a, b, st4, opts(), t0)
	if p.Skipped["not_owner"] != 1 {
		t.Fatalf("%v", p.Skipped)
	}
}

func TestAlbumInfo(t *testing.T) {
	sn := func(name, desc string) Snapshot {
		s := snap("X")
		s.AlbumName, s.AlbumDescription = name, desc
		return s
	}
	tests := []struct {
		name       string
		a, b       [2]string
		base       [2]*state.AlbumInfo
		wantUpd    [2]*immich.AlbumInfoUpdate
		wantAction bool
	}{
		{name: "no baseline never renames, records baseline", a: [2]string{"A", ""}, b: [2]string{"B", ""}, wantAction: true},
		{name: "no baseline fills empty description", a: [2]string{"N", "desc"}, b: [2]string{"N", ""}, wantAction: true,
			wantUpd: [2]*immich.AlbumInfoUpdate{nil, {Description: immich.Some("desc")}}},
		{name: "rename on B propagates to A", a: [2]string{"N", ""}, b: [2]string{"New", ""},
			base: [2]*state.AlbumInfo{{Name: "N"}, {Name: "N"}}, wantAction: true,
			wantUpd: [2]*immich.AlbumInfoUpdate{{Name: immich.Some("New")}, nil}},
		{name: "both renamed → winner a", a: [2]string{"X1", ""}, b: [2]string{"X2", ""},
			base: [2]*state.AlbumInfo{{Name: "N"}, {Name: "N"}}, wantAction: true,
			wantUpd: [2]*immich.AlbumInfoUpdate{nil, {Name: immich.Some("X1")}}},
		{name: "nothing changed", a: [2]string{"N", "d"}, b: [2]string{"N", "d"},
			base: [2]*state.AlbumInfo{{Name: "N", Description: "d"}, {Name: "N", Description: "d"}}},
		{name: "description cleared on A propagates", a: [2]string{"N", ""}, b: [2]string{"N", "d"},
			base: [2]*state.AlbumInfo{{Name: "N", Description: "d"}, {Name: "N", Description: "d"}}, wantAction: true,
			wantUpd: [2]*immich.AlbumInfoUpdate{nil, {Description: immich.Some("")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := stateWith()
			st.AlbumInfo = tc.base
			p := Build(sn(tc.a[0], tc.a[1]), sn(tc.b[0], tc.b[1]), st, opts(), t0)
			if (len(p.AlbumInfo) == 1) != tc.wantAction {
				t.Fatalf("%+v", p.AlbumInfo)
			}
			if !tc.wantAction {
				return
			}
			if !reflect.DeepEqual(p.AlbumInfo[0].AlbumUpdates, tc.wantUpd) {
				t.Fatalf("updates %+v want %+v", p.AlbumInfo[0].AlbumUpdates, tc.wantUpd)
			}
			nb := p.AlbumInfo[0].NewAlbumBase
			if nb[0] == nil || nb[1] == nil {
				t.Fatal("new base")
			}
			if tc.name == "rename on B propagates to A" && (nb[0].Name != "New" || nb[1].Name != "New") {
				t.Fatalf("%+v %+v", nb[0], nb[1])
			}
		})
	}
	o := opts()
	o.SyncAlbumInfo = false
	if p := Build(sn("a", ""), sn("b", ""), stateWith(), o, t0); len(p.AlbumInfo) != 0 {
		t.Fatal("disabled")
	}
}

func TestDeterministicOrder(t *testing.T) {
	a := snap("A", "z", "b", "m", "a")
	b := snap("B", "q", "c")
	p1 := Build(a, b, stateWith(), opts(), t0)
	p2 := Build(a, b, stateWith(), opts(), t0)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("non-deterministic")
	}
	if got := keysOf(p1.Copies); !reflect.DeepEqual(got, []string{"a", "b", "c", "m", "q", "z"}) {
		t.Fatalf("%v", got)
	}
}

func TestPrimaryAndRefs(t *testing.T) {
	a := snap("A")
	a.Groups["k"] = []Member{mem("A1", nil), mem("A2", nil)}
	p := Build(a, snap("B"), stateWith(), opts(), t0)
	if len(p.Copies) != 1 || len(p.Copies[0].Refs) != 2 || p.Copies[0].Refs[0].ID != "A1" {
		t.Fatalf("%+v", p.Copies)
	}
}
