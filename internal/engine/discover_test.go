package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

func discoveryEnv(t *testing.T, mode DiscoveryMode) *env {
	t.Helper()
	return newEnv(t, func(o *Options) {
		o.Discovery = mode
		o.DiscoveryMarker = "🔁"
	})
}

// shareDiscoveryEnv is discoveryEnv whose keys also carry the permissions sharing needs
// (teardown); tests set Options.DiscoveryShare themselves.
func shareDiscoveryEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryMarker = "🔁"
		o.DiscoveryShare[A] = []string{"perms@example.test"}
	})
}

func (e *env) discover() ([]Pair, DiscoveryReport) {
	e.t.Helper()
	ps, rep, err := e.eng.Pairs(context.Background(), nil)
	if err != nil {
		e.t.Fatalf("Pairs: %v", err)
	}
	return ps, rep
}

func albumByName(srv *fakeimmich.Server, owner, name string) []string {
	var ids []string
	for _, id := range srv.AlbumIDs() {
		if al, ok := srv.Album(id); ok && al.Name == name && al.OwnerID == owner {
			ids = append(ids, id)
		}
	}
	return ids
}

func pairNames(ps []Pair) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

func TestDiscoveryAllPairsByNameAndCreatesCounterparts(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	solo := e.fa.CreateAlbum(e.ua, "Solo")
	e.fa.EditAlbum(solo, func(al *fakeimmich.FakeAlbum) { al.Description = "only on A" })
	onlyB := e.fb.CreateAlbum(e.ub, "OnlyB")
	// Albums the user may only view, or not access at all, are not selected.
	carol, _ := e.fa.AddUser("carol")
	viewed := e.fa.CreateAlbum(carol, "Viewed")
	e.fa.Share(viewed, e.ua, "viewer")
	e.fa.CreateAlbum(carol, "Private")
	edited := e.fa.CreateAlbum(carol, "Edited")
	e.fa.Share(edited, e.ua, "editor")

	ps, rep := e.discover()
	if len(ps) != 4 || len(rep.Created) != 3 || rep.Problems() {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	byA := map[string]Pair{}
	for _, p := range ps {
		byA[p.A.ID] = p
		if p.Name != DiscoveredPairPrefix+p.A.ID {
			t.Errorf("pair name %q for A album %s", p.Name, p.A.ID)
		}
	}
	if p := byA[e.albumA]; p.B.ID != e.albumB {
		t.Errorf("Trip not paired by name: %+v", p)
	}
	soloB := albumByName(e.fb, e.ub, "Solo")
	if len(soloB) != 1 || byA[solo].B.ID != soloB[0] {
		t.Fatalf("Solo counterpart: %v pair %+v", soloB, byA[solo])
	}
	if al, _ := e.fb.Album(soloB[0]); al.Description != "only on A" {
		t.Errorf("created album description %q", al.Description)
	}
	onlyBA := albumByName(e.fa, e.ua, "OnlyB")
	if len(onlyBA) != 1 || byA[onlyBA[0]].B.ID != onlyB {
		t.Fatalf("OnlyB counterpart: %v", onlyBA)
	}
	if _, ok := byA[edited]; !ok || len(albumByName(e.fb, e.ub, "Edited")) != 1 {
		t.Errorf("editor-shared album not synced")
	}
	if len(albumByName(e.fb, e.ub, "Viewed"))+len(albumByName(e.fb, e.ub, "Private")) != 0 {
		t.Error("viewer-only or inaccessible album synced")
	}

	// Second pass: stable, nothing created.
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	ps2, rep2 := e.discover()
	if strings.Join(pairNames(ps2), ",") != strings.Join(pairNames(ps), ",") || len(rep2.Created) != 0 {
		t.Fatalf("second pass changed: %v %+v", pairNames(ps2), rep2)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
		t.Error("second pass mutated")
	}

	// A discovered pair syncs like a configured one.
	e.seedA("x") // into Trip on A
	s, err := e.eng.RunPair(context.Background(), byA[e.albumA])
	if err != nil || s.Copied.AtoB != 1 {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestDiscoveryMarker(t *testing.T) {
	e := discoveryEnv(t, DiscoveryMarker)
	// Marker on one side is enough; B's "Trip" has none.
	e.fa.EditAlbum(e.albumA, func(al *fakeimmich.FakeAlbum) { al.Description = "summer 🔁" })
	e.fa.CreateAlbum(e.ua, "Unmarked")
	marked := e.fb.CreateAlbum(e.ub, "Marked")
	e.fb.EditAlbum(marked, func(al *fakeimmich.FakeAlbum) { al.Description = "🔁" })

	ps, rep := e.discover()
	if len(ps) != 2 || len(rep.Created) != 1 || rep.Created[0].Side != "a" || rep.Problems() {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if len(albumByName(e.fb, e.ub, "Unmarked")) != 0 {
		t.Error("unmarked album synced")
	}
	created := albumByName(e.fa, e.ua, "Marked")
	if len(created) != 1 {
		t.Fatalf("Marked on A: %v", created)
	}
	if al, _ := e.fa.Album(created[0]); al.Description != "🔁" {
		t.Errorf("created description %q (marker must be copied)", al.Description)
	}

	// Sync Trip once so a pair state file exists.
	trip := Pair{Name: DiscoveredPairPrefix + e.albumA, A: AlbumRef{ID: e.albumA}, B: AlbumRef{ID: e.albumB}}
	e.seedA("x")
	if _, err := e.eng.RunPair(context.Background(), trip); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(e.dir, "pairs", trip.Name+".json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal(err)
	}

	// Album info sync copied the marker to B. Removing it on A keeps the pair until the
	// next sync propagates the removal; then syncing stops: pair state moved aside,
	// albums untouched, no tombstone.
	if al, _ := e.fb.Album(e.albumB); al.Description != "summer 🔁" {
		t.Fatalf("description not synced to B: %q", al.Description)
	}
	e.fa.EditAlbum(e.albumA, func(al *fakeimmich.FakeAlbum) { al.Description = "summer" })
	if ps, _ = e.discover(); len(ps) != 2 {
		t.Fatalf("unpaired before the removal reached B: %v", pairNames(ps))
	}
	if _, err := e.eng.RunPair(context.Background(), trip); err != nil {
		t.Fatal(err)
	}
	ps, rep = e.discover()
	if len(ps) != 1 || len(rep.Unpaired) != 1 || rep.Unpaired[0].Pair != trip.Name {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Errorf("pair state not moved aside: %v", err)
	}
	if len(e.fb.AlbumAssetIDs(e.albumB)) != 1 {
		t.Error("album contents changed by unpairing")
	}
	// Marker back: paired again (fresh state).
	e.fb.EditAlbum(e.albumB, func(al *fakeimmich.FakeAlbum) { al.Description = "🔁" })
	ps, rep = e.discover()
	if len(ps) != 2 || len(rep.Created) != 0 || rep.Problems() {
		t.Fatalf("re-pair: %v %+v", pairNames(ps), rep)
	}
}

func TestDiscoveryDeletedAlbumIsNotRecreated(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.discover()
	e.fb.DeleteAlbum(e.albumB)

	ps, rep := e.discover()
	if len(ps) != 0 || len(rep.Unpaired) != 1 || len(rep.Created) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if !strings.Contains(rep.Unpaired[0].Reason, "side b") {
		t.Errorf("reason %q", rep.Unpaired[0].Reason)
	}
	ps, rep = e.discover()
	if len(ps) != 0 || len(rep.Created) != 0 || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Reason, "deleted") {
		t.Fatalf("recreated or not reported: %v %+v", pairNames(ps), rep)
	}
	if n := len(albumByName(e.fb, e.ub, "Trip")); n != 0 {
		t.Fatalf("album recreated on B (%d)", n)
	}
	// A user re-creating the album by hand resumes syncing and clears the tombstone.
	again := e.fb.CreateAlbum(e.ub, "Trip")
	ps, rep = e.discover()
	if len(ps) != 1 || ps[0].B.ID != again || rep.Problems() {
		t.Fatalf("resume: %v %+v", pairNames(ps), rep)
	}
	d, _ := e.st.PeekDiscovery()
	if len(d.Tombstones) != 0 {
		t.Errorf("tombstones kept: %+v", d.Tombstones)
	}
}

func TestDiscoveryLostAccessIsNotRecreated(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	carol, _ := e.fb.AddUser("carol")
	shared := e.fb.CreateAlbum(carol, "Shared")
	e.fb.Share(shared, e.ub, "editor")
	ps, _ := e.discover()
	if len(ps) != 2 {
		t.Fatalf("pairs %v", pairNames(ps))
	}
	e.fb.Share(shared, e.ub, "viewer")
	ps, rep := e.discover()
	if len(ps) != 1 || len(rep.Unpaired) != 1 || len(rep.Created) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	ps, rep = e.discover()
	if len(rep.Created) != 0 || len(albumByName(e.fb, e.ub, "Shared")) != 0 {
		t.Fatalf("counterpart recreated: %+v", rep)
	}
}

func TestDiscoveryConflictsAreSkipped(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.fa.CreateAlbum(e.ua, "Dup")
	e.fa.CreateAlbum(e.ua, "Dup")
	e.fb.CreateAlbum(e.ub, "TwoOnB")
	e.fb.CreateAlbum(e.ub, "TwoOnB")
	e.fa.CreateAlbum(e.ua, "TwoOnB")
	e.fa.CreateAlbum(e.ua, "")
	ps, rep := e.discover()
	if len(ps) != 1 || len(rep.Created) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	// Dup x2 on A, TwoOnB on A and on B x2, unnamed album.
	if len(rep.Skipped) != 6 {
		t.Fatalf("skipped %+v", rep.Skipped)
	}
	if len(albumByName(e.fb, e.ub, "Dup")) != 0 {
		t.Error("ambiguous album created")
	}
}

func TestDiscoveryDryRunNeverMutates(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.fa.CreateAlbum(e.ua, "Solo")
	dry := e.newEngine(func() Options { o := e.opts; o.DryRun = true; return o }())
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	ps, rep, err := dry.Pairs(context.Background(), nil)
	if err != nil || len(ps) != 1 || len(rep.WouldCreate) != 1 || len(rep.Created) != 0 {
		t.Fatalf("%v %v %+v", err, pairNames(ps), rep)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
		t.Error("dry run mutated")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "discovery.json")); !os.IsNotExist(err) {
		t.Errorf("discovery state written: %v", err)
	}
}

func TestDiscoveryReadOnlyStoreNeverMutates(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.fa.CreateAlbum(e.ua, "Solo")
	ro, err := state.OpenReadOnly(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(e.opts, e.clA, e.clB, ro, nil)
	if err != nil {
		t.Fatal(err)
	}
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	if _, rep, err := eng.Pairs(context.Background(), nil); err != nil || len(rep.WouldCreate) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
		t.Error("read-only discovery mutated")
	}
}

// A listing that misses a pinned album must not unpair it: absence is confirmed with a GET.
func TestDiscoveryListingGlitchKeepsPair(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.discover()
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums", Status: 200, Body: "[]"})
	ps, rep := e.discover()
	if len(ps) != 1 || len(rep.Unpaired) != 0 || len(rep.Created) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
}

// An album lookup that fails keeps the pair (not synced this cycle, never unpaired).
func TestDiscoveryLookupErrorKeepsPair(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.discover()
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums", Status: 200, Body: "[]"})
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums/", Status: 503, Times: 5})
	ps, rep := e.discover()
	if len(ps) != 0 || len(rep.Unpaired) != 0 || len(rep.Errors) != 1 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	ps, rep = e.discover()
	if len(ps) != 1 || rep.Problems() {
		t.Fatalf("pair lost after transient error: %v %+v", pairNames(ps), rep)
	}
}

// POST /albums is never retried; the next pass creates (or adopts) the album.
func TestDiscoveryCreateFailureNotRetried(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.fa.CreateAlbum(e.ua, "Solo")
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/albums", Status: 502})
	ps, rep := e.discover()
	if len(ps) != 1 || len(rep.Errors) != 1 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	posts := 0
	for _, r := range e.fb.Requests() {
		if r.Method == "POST" && r.Path == "/api/albums" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("POST /albums sent %d times", posts)
	}
	ps, rep = e.discover()
	if len(ps) != 2 || len(rep.Created) != 1 {
		t.Fatalf("second pass: %v %+v", pairNames(ps), rep)
	}
}

// Pairs never carry over to another account: a discovery file of other users is discarded.
func TestDiscoveryStateBoundToUsers(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	e.discover()
	d, _ := e.st.PeekDiscovery()
	d.UserIDs[1] = "someone-else"
	if err := e.st.SaveDiscovery(d); err != nil {
		t.Fatal(err)
	}
	ps, _ := e.discover()
	if len(ps) != 1 {
		t.Fatalf("pairs %v", pairNames(ps))
	}
	m, _ := filepath.Glob(filepath.Join(e.dir, "discovery.json.fpreset-*"))
	if len(m) != 1 {
		t.Errorf("foreign discovery state not moved aside: %v", m)
	}
}

func TestDiscoveryOffReturnsStatic(t *testing.T) {
	e := newEnv(t)
	ps, rep, err := e.eng.Pairs(context.Background(), []Pair{e.pair()})
	if err != nil || len(ps) != 1 || ps[0] != e.pair() || rep.Mode != DiscoveryOff {
		t.Fatalf("%v %v %+v", err, ps, rep)
	}
}

func TestDiscoverySharesCreatedAlbums(t *testing.T) {
	e := shareDiscoveryEnv(t)
	carolB, _ := e.fb.AddUser("carol")
	daveB, _ := e.fb.AddUser("dave")
	erinA, _ := e.fa.AddUser("erin")
	solo := e.fa.CreateAlbum(e.ua, "Solo")
	onlyB := e.fb.CreateAlbum(e.ub, "OnlyB")
	o := e.opts
	o.DiscoveryShareRole = "viewer"
	// Emails match case-insensitively; the service user itself and repeats are dropped.
	o.DiscoveryShare = [2][]string{{erinA}, {"Carol@Example.TEST", daveB, "bob@example.test", carolB}}
	e.eng = e.newEngine(o)

	_, rep := e.discover()
	if len(rep.Created) != 2 || rep.Problems() {
		t.Fatalf("report %+v", rep)
	}
	soloB := albumByName(e.fb, e.ub, "Solo")
	if len(soloB) != 1 {
		t.Fatalf("Solo on B: %v", soloB)
	}
	al, _ := e.fb.Album(soloB[0])
	want := []immich.AlbumUser{{UserID: carolB, Role: "viewer"}, {UserID: daveB, Role: "viewer"}}
	if !reflect.DeepEqual(al.Users, want) {
		t.Errorf("Solo on B shared with %+v, want %+v", al.Users, want)
	}
	onlyBA := albumByName(e.fa, e.ua, "OnlyB")
	if len(onlyBA) != 1 {
		t.Fatalf("OnlyB on A: %v", onlyBA)
	}
	if al, _ := e.fa.Album(onlyBA[0]); !reflect.DeepEqual(al.Users, []immich.AlbumUser{{UserID: erinA, Role: "viewer"}}) {
		t.Errorf("OnlyB on A shared with %+v", al.Users)
	}
	// Albums paired with an existing counterpart are never touched.
	for _, id := range []string{e.albumA, solo} {
		if al, _ := e.fa.Album(id); len(al.Users) != 0 {
			t.Errorf("existing album %s shared: %+v", id, al.Users)
		}
	}
	if al, _ := e.fb.Album(onlyB); len(al.Users) != 0 {
		t.Errorf("existing album shared: %+v", al.Users)
	}
	// Only side B needed an email lookup, once.
	if n := countReqs(e.fb, "GET", "/api/users"); n != 1 {
		t.Errorf("GET /users on B: %d", n)
	}
	if n := countReqs(e.fa, "GET", "/api/users"); n != 0 {
		t.Errorf("GET /users on A: %d", n)
	}
}

func TestDiscoveryShareUnresolvableNeverCreatesUnsharedAlbum(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry=%v", dry), func(t *testing.T) {
			e := shareDiscoveryEnv(t)
			e.fa.CreateAlbum(e.ua, "Solo")
			o := e.opts
			o.DryRun = dry
			o.DiscoveryShare = [2][]string{nil, {"nobody@example.test"}}
			e.eng = e.newEngine(o)
			mb := e.fb.Mutations()

			ps, rep := e.discover()
			if len(rep.Created)+len(rep.WouldCreate) != 0 || len(rep.Errors) != 1 ||
				!strings.Contains(rep.Errors[0].Reason, "nobody@example.test") || rep.Errors[0].Side != "b" {
				t.Fatalf("report %+v", rep)
			}
			if len(ps) != 1 { // Trip pairs by name and still syncs
				t.Errorf("pairs %v", pairNames(ps))
			}
			if e.fb.Mutations() != mb || len(albumByName(e.fb, e.ub, "Solo")) != 0 {
				t.Error("album created on B")
			}
		})
	}
}

func TestDiscoveryShareRoleValidated(t *testing.T) {
	e := discoveryEnv(t, DiscoveryAll)
	o := e.opts
	o.DiscoveryShareRole = "owner"
	if _, err := New(o, e.clA, e.clB, e.st, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("err %v", err)
	}
}

func countReqs(srv *fakeimmich.Server, method, path string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}
