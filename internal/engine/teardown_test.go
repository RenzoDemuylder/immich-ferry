package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

// tdEnv: carol (side A) shares her album "Holiday" with the sync user alice as editor.
// Discovery creates "Holiday" on B, owned by the sync user bob and shared with dave.
type tdEnv struct {
	*env
	carol, dave string
	holidayA    string
	holidayB    string
	pair        Pair
	photos      []string // carol's assets on A
}

func newTeardownEnv(t *testing.T, mods ...func(*Options)) *tdEnv {
	t.Helper()
	e := &tdEnv{}
	e.env = newEnv(t, append([]func(*Options){func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[B] = []string{"dave@example.test"}
		o.DiscoveryTeardownTrash = true
	}}, mods...)...)
	if e.opts.DiscoveryDedicatedUser {
		// A dedicated sync user owns no albums of its own (the harness's unshared Trip).
		e.fa.DeleteAlbum(e.albumA)
		e.fb.DeleteAlbum(e.albumB)
	}
	e.carol, _ = e.fa.AddUser("carol")
	e.dave, _ = e.fb.AddUser("dave")
	e.holidayA = e.fa.CreateAlbum(e.carol, "Holiday")
	e.fa.Share(e.holidayA, e.ua, "editor")
	for _, n := range []string{"h1", "h2", "h3"} {
		e.photos = append(e.photos, e.seed(e.fa, e.carol, e.holidayA, n))
	}
	ps, rep := e.discover()
	if len(rep.Created) != 1 || rep.Problems() {
		t.Fatalf("setup discovery: %+v", rep)
	}
	ids := albumByName(e.fb, e.ub, "Holiday")
	if len(ids) != 1 {
		t.Fatalf("Holiday on B: %v", ids)
	}
	e.holidayB = ids[0]
	for _, p := range ps {
		if p.A.ID == e.holidayA {
			e.pair = p
		}
	}
	if e.pair.B.ID != e.holidayB {
		t.Fatalf("Holiday pair: %+v", ps)
	}
	if s, err := e.eng.RunPair(context.Background(), e.pair); err != nil || s.Copied.AtoB != 3 {
		t.Fatalf("initial sync: %v %+v", err, s)
	}
	return e
}

func (e *tdEnv) syncedB() []string { return e.fb.AlbumAssetIDs(e.holidayB) }

func hasPair(ps []Pair, name string) bool {
	return slices.ContainsFunc(ps, func(p Pair) bool { return p.Name == name })
}

func (e *tdEnv) memberOfA() bool {
	al, ok := e.fa.Album(e.holidayA)
	return ok && slices.ContainsFunc(al.Users, func(u immich.AlbumUser) bool { return u.UserID == e.ua })
}

func (e *tdEnv) loadDiscovery() *state.Discovery {
	e.t.Helper()
	d, err := e.st.PeekDiscovery()
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestTeardownWhenEveryoneLeft(t *testing.T) {
	for _, trash := range []bool{true, false} {
		t.Run(map[bool]string{true: "trash", false: "keep-assets"}[trash], func(t *testing.T) {
			e := newTeardownEnv(t, func(o *Options) { o.DiscoveryTeardownTrash = trash })
			ours := e.syncedB()
			if len(ours) != 3 {
				t.Fatalf("synced to B: %v", ours)
			}
			// dave adds a photo of his own; one of ours is also in another album of bob.
			daves := e.seed(e.fb, e.dave, e.holidayB, "dave1")
			other := e.fb.CreateAlbum(e.ub, "Other")
			e.fb.AddToAlbum(other, ours[0])

			e.fb.LeaveAlbum(e.holidayB, e.dave)
			ps, rep := e.discover()
			if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || rep.TornDown[0].Pair != e.pair.Name || len(rep.Errors) != 0 {
				t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
			}
			if _, ok := e.fb.Album(e.holidayB); ok {
				t.Error("created album not deleted")
			}
			if e.memberOfA() {
				t.Error("sync user still a member of the original album")
			}
			// The original album and carol's photos are untouched.
			if got := e.fa.AlbumAssetIDs(e.holidayA); len(got) != 3 {
				t.Errorf("original album assets %v", got)
			}
			for _, id := range e.photos {
				if a, _ := e.fa.Asset(id); a.IsTrashed {
					t.Errorf("A asset %s trashed", id)
				}
			}
			wantTrashed := map[string]bool{}
			if trash {
				wantTrashed[ours[1]], wantTrashed[ours[2]] = true, true
				if len(rep.Trashed) != 1 || rep.Trashed[0].Assets != 2 {
					t.Errorf("trashed report %+v", rep.Trashed)
				}
			}
			for _, id := range append(ours, daves) {
				a, ok := e.fb.Asset(id)
				if !ok || a.IsTrashed != wantTrashed[id] {
					t.Errorf("B asset %s: exists %v trashed %v, want trashed %v", id, ok, a.IsTrashed, wantTrashed[id])
				}
			}
			// Teardown is final: nothing re-created, re-paired or re-shared.
			ma, mb := e.fa.Mutations(), e.fb.Mutations()
			ps, rep = e.discover()
			if hasPair(ps, e.pair.Name) || len(rep.Created)+len(rep.TornDown) != 0 || len(albumByName(e.fb, e.ub, "Holiday")) != 0 {
				t.Fatalf("after teardown: pairs %v report %+v", pairNames(ps), rep)
			}
			if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
				t.Error("pass after teardown mutated")
			}
			for _, p := range e.loadDiscovery().Pairs {
				if p.Name == e.pair.Name {
					t.Errorf("torn-down pair still in discovery state: %+v", p)
				}
			}
		})
	}
}

func TestTeardownPartialLeaveKeepsSyncing(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[B] = []string{"dave@example.test", "erin@example.test"}
	})
	carol, _ := e.fa.AddUser("carol")
	dave, _ := e.fb.AddUser("dave")
	e.fb.AddUser("erin")
	hol := e.fa.CreateAlbum(carol, "Holiday")
	e.fa.Share(hol, e.ua, "editor")
	e.discover()
	holB := albumByName(e.fb, e.ub, "Holiday")[0]
	e.fb.LeaveAlbum(holB, dave)
	ps, rep := e.discover()
	if !hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.TornDown)+len(rep.WouldTeardown) != 0 || rep.Problems() {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
}

func TestTeardownDryRunNeverMutates(t *testing.T) {
	e := newTeardownEnv(t)
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	o := e.opts
	o.DryRun = true
	e.eng = e.newEngine(o)
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	before, _ := os.ReadFile(filepath.Join(e.dir, "discovery.json"))
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.WouldTeardown) != 1 || len(rep.TornDown) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	after, _ := os.ReadFile(filepath.Join(e.dir, "discovery.json"))
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb || string(before) != string(after) {
		t.Error("dry run mutated")
	}
}

// A listing that shows nobody but the sync user is confirmed with a GET first.
func TestTeardownListingGlitchIsConfirmed(t *testing.T) {
	e := newTeardownEnv(t)
	body, _ := json.Marshal([]any{map[string]any{"id": e.holidayB, "albumName": "Holiday", "description": "",
		"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": e.ub}}}}})
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums", Status: 200, Body: string(body)})
	ps, rep := e.discover()
	if !hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || e.loadDiscovery().Pairs[0].Teardown != nil {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	// A failing confirmation keeps the pair without syncing it.
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums/" + e.holidayB, Status: 503, Times: 2}) // MaxRetries 1
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || len(rep.Errors) != 1 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); !ok || !e.memberOfA() {
		t.Fatal("teardown ran on an unconfirmed check")
	}
	if _, rep = e.discover(); len(rep.TornDown) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

// A failed step keeps its progress; the pair is never synced again and the teardown
// resumes next cycle.
func TestTeardownResumesAfterFailure(t *testing.T) {
	e := newTeardownEnv(t)
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayB, Status: 503, Times: 2}) // MaxRetries 1
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || len(rep.Errors) != 1 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	var tp *state.TeardownProgress
	for _, p := range e.loadDiscovery().Pairs {
		if p.Name == e.pair.Name {
			tp = p.Teardown
		}
	}
	if tp == nil || !tp.Left || !tp.AssetsDone || tp.AlbumDeleted {
		t.Fatalf("progress %+v", tp)
	}
	if e.memberOfA() {
		t.Error("not left")
	}
	// Someone re-joins meanwhile: the teardown still finishes (never syncs again).
	e.fb.Share(e.holidayB, e.dave, "editor")
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); ok {
		t.Error("album not deleted on resume")
	}
}

// The sync user owns the original album: it cannot leave, so the emptied counterpart is
// kept as a blocker and nothing is ever re-created.
func TestTeardownSyncOwnsOriginal(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[B] = []string{"dave@example.test"}
	})
	dave, _ := e.fb.AddUser("dave")
	hol := e.fa.CreateAlbum(e.ua, "Holiday")
	e.discover()
	holB := albumByName(e.fb, e.ub, "Holiday")[0]
	e.fb.LeaveAlbum(holB, dave)
	ps, rep := e.discover()
	if hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(holB); !ok {
		t.Fatal("counterpart deleted although the original could not be left")
	}
	for i := 0; i < 2; i++ {
		mb := e.fb.Mutations()
		ps, rep = e.discover()
		if hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.Created) != 0 || e.fb.Mutations() != mb {
			t.Fatalf("pass %d: pairs %v report %+v", i, pairNames(ps), rep)
		}
	}
}

// Losing state never deletes or trashes anything that is not provably ours.
func TestTeardownStateLoss(t *testing.T) {
	t.Run("discovery state lost after everyone left", func(t *testing.T) {
		e := newTeardownEnv(t)
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		if err := os.Remove(filepath.Join(e.dir, "discovery.json")); err != nil {
			t.Fatal(err)
		}
		// Re-paired by name; no record that the album was shared → never torn down.
		for i := 0; i < 2; i++ {
			ps, rep := e.discover()
			if !hasPair(ps, e.pair.Name) || len(rep.Created)+len(rep.TornDown)+len(rep.WouldTeardown) != 0 {
				t.Fatalf("pass %d: pairs %v report %+v", i, pairNames(ps), rep)
			}
		}
		if _, ok := e.fb.Album(e.holidayB); !ok || !e.memberOfA() || len(e.syncedB()) != 3 {
			t.Error("albums or assets touched without state")
		}
	})
	t.Run("pair state lost: nothing provably ours", func(t *testing.T) {
		e := newTeardownEnv(t)
		if err := e.st.Reset(e.pair.Name); err != nil {
			t.Fatal(err)
		}
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		_, rep := e.discover()
		if len(rep.TornDown) != 1 || len(rep.Trashed) != 0 {
			t.Fatalf("report %+v", rep)
		}
		for _, a := range e.fb.AssetsOf(e.ub) {
			if a.IsTrashed {
				t.Errorf("asset %s trashed without pair state", a.ID)
			}
		}
	})
}

// With a dedicated sync user, losing discovery.json never re-pairs, re-syncs or
// re-creates an album everyone left, at any point of the teardown.
func TestTeardownStateLossDedicatedUser(t *testing.T) {
	dedicated := func(o *Options) { o.DiscoveryDedicatedUser = true }
	lose := func(e *tdEnv) {
		e.t.Helper()
		if err := os.Remove(filepath.Join(e.dir, "discovery.json")); err != nil {
			e.t.Fatal(err)
		}
	}
	t.Run("everyone left, no teardown yet", func(t *testing.T) {
		e := newTeardownEnv(t, dedicated)
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		lose(e)
		for i := 0; i < 2; i++ {
			ma, mb := e.fa.Mutations(), e.fb.Mutations()
			ps, rep := e.discover()
			if hasPair(ps, e.pair.Name) || len(rep.Created)+len(rep.TornDown) != 0 || e.fa.Mutations() != ma || e.fb.Mutations() != mb {
				t.Fatalf("pass %d: pairs %v report %+v", i, pairNames(ps), rep)
			}
			// The leftover on B and Holiday on A (blocked by it) are reported.
			if len(rep.Skipped) != 2 {
				t.Errorf("skipped %+v", rep.Skipped)
			}
		}
		if len(e.syncedB()) != 3 || !e.memberOfA() {
			t.Error("albums or assets touched without state")
		}
	})
	t.Run("after leaving the original", func(t *testing.T) {
		e := newTeardownEnv(t, dedicated)
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		e.fb.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/assets", Status: 503, Times: 2}) // MaxRetries 1
		if _, rep := e.discover(); len(rep.Errors) != 1 || e.memberOfA() {
			t.Fatalf("report %+v", rep)
		}
		lose(e)
		ma, mb := e.fa.Mutations(), e.fb.Mutations()
		ps, rep := e.discover()
		if len(ps) != 0 || len(rep.Created) != 0 || e.fa.Mutations() != ma || e.fb.Mutations() != mb {
			t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
		}
	})
	t.Run("sync user owns the original", func(t *testing.T) {
		e := newEnv(t, func(o *Options) {
			o.Discovery = DiscoveryAll
			o.DiscoveryShare[B] = []string{"dave@example.test"}
			o.DiscoveryDedicatedUser = true
		})
		dave, _ := e.fb.AddUser("dave")
		hol := e.fa.CreateAlbum(e.ua, "Holiday")
		e.discover()
		e.fb.LeaveAlbum(albumByName(e.fb, e.ub, "Holiday")[0], dave)
		if _, rep := e.discover(); len(rep.TornDown) != 1 {
			t.Fatalf("report %+v", rep)
		}
		if err := os.Remove(filepath.Join(e.dir, "discovery.json")); err != nil {
			t.Fatal(err)
		}
		mb := e.fb.Mutations()
		ps, rep := e.discover()
		if hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.Created) != 0 || e.fb.Mutations() != mb {
			t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
		}
	})
}

// Dedicated-user mode only affects albums that are not paired yet: a pinned pair whose
// album never had members keeps syncing, and without the option an unshared album the
// sync user owns is synced as usual.
func TestDedicatedUserScope(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		e := newEnv(t, func(o *Options) {
			o.Discovery = DiscoveryAll
			o.DiscoveryShare[B] = []string{"dave@example.test"}
			o.DiscoveryDedicatedUser = dedicated
		})
		e.fb.AddUser("dave")
		ps, rep := e.discover() // Trip (owned by bob on B, no members) pairs with Trip on A
		if dedicated {
			if len(ps) != 0 || len(rep.Skipped) != 2 {
				t.Fatalf("dedicated: pairs %v report %+v", pairNames(ps), rep)
			}
			continue
		}
		if len(ps) != 1 || rep.Problems() {
			t.Fatalf("not dedicated: pairs %v report %+v", pairNames(ps), rep)
		}
	}
	// Pinned before the option was turned on: keeps syncing.
	e := newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[B] = []string{"dave@example.test"}
	})
	e.fb.AddUser("dave")
	e.discover()
	o := e.opts
	o.DiscoveryDedicatedUser = true
	e.eng = e.newEngine(o)
	if ps, rep := e.discover(); len(ps) != 1 || rep.Problems() {
		t.Fatalf("pinned pair: pairs %v report %+v", pairNames(ps), rep)
	}
}

// Pairs from before the Created record: recorded from the live album when it has members.
func TestTeardownBackfill(t *testing.T) {
	e := newTeardownEnv(t)
	d := e.loadDiscovery()
	for i := range d.Pairs {
		d.Pairs[i].Created = nil
	}
	if err := e.st.SaveDiscovery(d); err != nil {
		t.Fatal(err)
	}
	if ps, _ := e.discover(); !hasPair(ps, e.pair.Name) {
		t.Fatal("pair lost")
	}
	var created *state.CreatedAlbum
	for _, p := range e.loadDiscovery().Pairs {
		if p.Name == e.pair.Name {
			created = p.Created
		} else if p.Created != nil {
			t.Errorf("Trip (both albums owned by the sync users) recorded as created: %+v", p.Created)
		}
	}
	if created == nil || created.Side != uint8(B) || !slices.Equal(created.SharedWith, []string{e.dave}) {
		t.Fatalf("backfilled %+v", created)
	}
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	if _, rep := e.discover(); len(rep.TornDown) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

// A pinned pair whose album never had members (sharing configured later) keeps syncing.
func TestTeardownLegacyUnsharedPairKeepsSyncing(t *testing.T) {
	// Share configured on A from the start only so the key has the teardown permissions.
	e := newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[A] = []string{"carol@example.test"}
	})
	carol, _ := e.fa.AddUser("carol")
	hol := e.fa.CreateAlbum(carol, "Holiday")
	e.fa.Share(hol, e.ua, "editor")
	e.discover() // creates Holiday on B, unshared
	o := e.opts
	e.fb.AddUser("dave")
	o.DiscoveryShare[B] = []string{"dave@example.test"}
	e.eng = e.newEngine(o)
	ps, rep := e.discover()
	if !hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.TornDown)+len(rep.WouldTeardown) != 0 || rep.Problems() {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
}

func TestRequiredPermissionsTeardown(t *testing.T) {
	o := baseOptions()
	o.Discovery = DiscoveryAll
	o.RestoreTrashed = false
	has := func(o Options, p string) bool { return slices.Contains(RequiredPermissions(o), p) }
	if has(o, "album.delete") || has(o, "albumUser.delete") || has(o, "asset.delete") {
		t.Errorf("without share: %v", RequiredPermissions(o))
	}
	o.DiscoveryShare[A] = []string{"x@example.test"}
	if !has(o, "album.delete") || !has(o, "albumUser.delete") || has(o, "asset.delete") {
		t.Errorf("with share: %v", RequiredPermissions(o))
	}
	o.DiscoveryTeardownTrash = true
	if !has(o, "asset.delete") {
		t.Errorf("with trash: %v", RequiredPermissions(o))
	}
}

// With the teardown disabled, a pair whose created album everyone left keeps syncing and
// nothing is left, trashed or deleted (the key lacks album.delete/albumUser.delete).
func TestTeardownDisabledKeepsSyncing(t *testing.T) {
	e := newTeardownEnv(t, func(o *Options) {
		o.DiscoveryTeardownOff = true
		o.DiscoveryTeardownTrash = false
	})
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	for _, dry := range []bool{true, false} {
		e.eng.o.DryRun = dry
		ps, rep := e.discover()
		if !hasPair(ps, e.pair.Name) || len(rep.TornDown)+len(rep.WouldTeardown)+len(rep.Skipped) != 0 || rep.Problems() {
			t.Fatalf("dry=%v pairs %v report %+v", dry, pairNames(ps), rep)
		}
	}
	if _, ok := e.fb.Album(e.holidayB); !ok {
		t.Error("created album deleted")
	}
	if !e.memberOfA() {
		t.Error("sync user left the original album")
	}
	e.seed(e.fa, e.carol, e.holidayA, "h4")
	if s, err := e.eng.RunPair(context.Background(), e.pair); err != nil || s.Copied.AtoB != 1 {
		t.Fatalf("sync after everyone left: %v %+v", err, s)
	}
	for _, p := range e.loadDiscovery().Pairs {
		if p.Teardown != nil {
			t.Fatalf("teardown recorded: %+v", p)
		}
	}
}

// Disabling the teardown while one is in progress pauses it: the pair is kept but never
// synced; enabling it again finishes the teardown.
func TestTeardownDisabledPausesInProgress(t *testing.T) {
	e := newTeardownEnv(t)
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayB, Status: 503, Times: 2}) // MaxRetries 1
	if ps, rep := e.discover(); hasPair(ps, e.pair.Name) || len(rep.Errors) != 1 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}

	off := e.opts
	off.DiscoveryTeardownOff, off.DiscoveryTeardownTrash = true, false
	e.eng = e.newEngine(off)
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || len(rep.Skipped) != 1 || rep.Skipped[0].Pair != e.pair.Name || len(rep.Errors) != 0 {
		t.Fatalf("paused: pairs %v report %+v", pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); !ok {
		t.Error("album deleted while the teardown is disabled")
	}
	if d := e.loadDiscovery(); len(d.Pairs) == 0 || d.Pairs[0].Teardown == nil {
		t.Fatalf("progress lost: %+v", d.Pairs)
	}

	e.eng = e.newEngine(e.opts)
	if ps, rep := e.discover(); hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resumed: pairs %v report %+v", pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); ok {
		t.Error("album not deleted on resume")
	}
}

func TestRequiredPermissionsTeardownOff(t *testing.T) {
	o := baseOptions()
	o.Discovery = DiscoveryAll
	o.RestoreTrashed = false
	o.DiscoveryShare[A] = []string{"x@example.test"}
	o.DiscoveryTeardownOff = true
	for _, p := range RequiredPermissions(o) {
		if p == "album.delete" || p == "albumUser.delete" || p == "asset.delete" {
			t.Errorf("teardown off needs %s: %v", p, RequiredPermissions(o))
		}
	}
}
