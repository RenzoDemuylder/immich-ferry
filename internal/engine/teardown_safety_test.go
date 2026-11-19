package engine

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

// Safety branches of the discovery teardown (teardown.go): trashOurs only ever trashes
// provable own uploads and fails safe; every persisted step resumes after an error.

const tsNotFoundBody = `{"message":"Not found or no album.read access","statusCode":400}`

// tsPass runs one discovery pass and returns its error instead of failing the test.
func (e *tdEnv) tsPass() ([]Pair, DiscoveryReport, error) {
	return e.eng.Pairs(context.Background(), nil)
}

// tsProgress returns the persisted teardown progress of the Holiday pair (nil if the pair
// is not in the discovery state or not in teardown).
func (e *tdEnv) tsProgress() (*state.TeardownProgress, bool) {
	e.t.Helper()
	for _, p := range e.loadDiscovery().Pairs {
		if p.Name == e.pair.Name {
			return p.Teardown, true
		}
	}
	return nil, false
}

func (e *tdEnv) tsPairFile() string { return filepath.Join(e.dir, "pairs", e.pair.Name+".json") }

// tsEditPairState rewrites the pair state of the Holiday pair.
func (e *tdEnv) tsEditPairState(f func(ps *state.PairState)) {
	e.t.Helper()
	ps, _, err := e.st.Peek(e.pair.Name)
	if err != nil {
		e.t.Fatal(err)
	}
	f(ps)
	if err := e.st.Save(e.pair.Name, ps); err != nil {
		e.t.Fatal(err)
	}
}

// tsTrashedB returns which of ids are trashed on B.
func (e *tdEnv) tsTrashedB(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if a, ok := e.fb.Asset(id); ok && a.IsTrashed {
			out[id] = true
		}
	}
	return out
}

// tsReadOnlyDir makes the state directory unwritable (discovery.json saves fail) until
// the returned restore is called (also on cleanup).
func (e *tdEnv) tsReadOnlyDir() (restore func()) {
	e.t.Helper()
	if os.Geteuid() == 0 {
		e.t.Skip("root ignores directory permissions")
	}
	var once sync.Once
	restore = func() { once.Do(func() { _ = os.Chmod(e.dir, 0o755) }) }
	e.t.Cleanup(restore)
	if err := os.Chmod(e.dir, 0o555); err != nil {
		e.t.Fatal(err)
	}
	return restore
}

func tsHasError(rep DiscoveryReport, sub string) bool {
	for _, n := range rep.Errors {
		if strings.Contains(n.Reason, sub) {
			return true
		}
	}
	return false
}

// tsTrashRequests counts bulk trash requests on srv.
func tsTrashRequests(srv *fakeimmich.Server) int { return countReqs(srv, "DELETE", "/api/assets") }

// trashOurs: a pair state bound to other albums proves nothing (nothing trashed); a
// state without album ids (older state) is still trusted.
func TestTeardownSafetyPairStateAlbumIDs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ids       func(e *tdEnv) [2]string
		wantTrash bool
	}{
		{"other albums", func(e *tdEnv) [2]string { return [2]string{e.holidayA, "00000000-0000-4000-8000-000000000999"} }, false},
		{"other album on A", func(e *tdEnv) [2]string { return [2]string{e.albumA, e.holidayB} }, false},
		{"unset", func(e *tdEnv) [2]string { return [2]string{} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTeardownEnv(t)
			ours := e.syncedB()
			ids := tc.ids(e)
			e.tsEditPairState(func(ps *state.PairState) { ps.AlbumIDs = ids })
			e.fb.LeaveAlbum(e.holidayB, e.dave)
			ps, rep := e.discover()
			if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
				t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
			}
			got := e.tsTrashedB(ours)
			if tc.wantTrash {
				if len(got) != 3 || len(rep.Trashed) != 1 || rep.Trashed[0].Assets != 3 {
					t.Errorf("trashed %v report %+v", got, rep.Trashed)
				}
				return
			}
			if len(got) != 0 || len(rep.Trashed) != 0 || tsTrashRequests(e.fb) != 0 {
				t.Errorf("trashed with a pair state of other albums: %v report %+v", got, rep.Trashed)
			}
		})
	}
}

// trashOurs: an unreadable pair state fails the step (retried); nothing is trashed and
// the created album is kept until it succeeds.
func TestTeardownSafetyPairStateUnreadable(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	orig, err := os.ReadFile(e.tsPairFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.tsPairFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.tsPairFile(), 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	for i := 0; i < 2; i++ {
		ps, rep := e.discover()
		if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "read pair state") {
			t.Fatalf("pass %d: pairs %v report %+v", i, pairNames(ps), rep)
		}
		tp, _ := e.tsProgress()
		if tp == nil || !tp.Left || tp.AssetsDone || tp.AlbumDeleted {
			t.Fatalf("pass %d: progress %+v", i, tp)
		}
		if _, ok := e.fb.Album(e.holidayB); !ok || len(e.tsTrashedB(ours)) != 0 || tsTrashRequests(e.fb) != 0 {
			t.Fatalf("pass %d: album deleted or assets trashed with an unreadable pair state", i)
		}
	}
	if err := os.Remove(e.tsPairFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.tsPairFile(), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 3 {
		t.Errorf("trashed on resume %v", got)
	}
}

// trashOurs: a failing album listing fails the step; nothing trashed, retried.
func TestTeardownSafetyListAlbumAssetsFails(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/search/metadata", Status: 503, Times: 2}) // MaxRetries 1
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "list album assets") {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if tp, _ := e.tsProgress(); tp == nil || !tp.Left || tp.AssetsDone {
		t.Fatalf("progress %+v", tp)
	}
	if _, ok := e.fb.Album(e.holidayB); !ok || len(e.tsTrashedB(ours)) != 0 || tsTrashRequests(e.fb) != 0 {
		t.Fatal("album deleted or assets trashed without a listing")
	}
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 3 {
		t.Errorf("trashed on resume %v", got)
	}
}

// trashOurs: when the albums of one asset cannot be listed, that asset is kept and the
// others are still trashed (the step succeeds).
func TestTeardownSafetyAlbumsWithAssetErrorKeepsAsset(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	var once sync.Once
	// The hook runs right before the album listing of trashOurs: the next GET /albums is
	// the first ?assetId lookup.
	e.fb.OnSearchPage(func(int) {
		once.Do(func() {
			e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums", Status: 503, Times: 2}) // MaxRetries 1
		})
	})
	ps, rep := e.discover()
	e.fb.OnSearchPage(nil)
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	failed := ""
	for _, r := range e.fb.Requests() {
		if r.Method == "GET" && r.Path == "/api/albums" {
			if q, _ := url.ParseQuery(r.Query); q.Get("assetId") != "" {
				failed = q.Get("assetId")
				break
			}
		}
	}
	if !contains(ours, failed) {
		t.Fatalf("first asset lookup %q not one of ours %v", failed, ours)
	}
	got := e.tsTrashedB(ours)
	if got[failed] {
		t.Errorf("asset %s trashed although its albums could not be listed", failed)
	}
	if len(got) != 2 || len(rep.Trashed) != 1 || rep.Trashed[0].Assets != 2 {
		t.Errorf("trashed %v report %+v", got, rep.Trashed)
	}
}

// trashOurs: only assets that are recorded as ours AND owned by the sync user AND in no
// other visible album are trashed.
func TestTeardownSafetyOnlyProvablyOurs(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	// bob's own photo added to the created album directly (not uploaded by the tool).
	bobs := e.seed(e.fb, e.ub, e.holidayB, "bob1")
	// dave's photo in the album, falsely recorded as ours: not owned by the sync user.
	daves := e.seed(e.fb, e.dave, e.holidayB, "dave1")
	// One of ours is also in an album of dave's shared with bob.
	davesAlbum := e.fb.CreateAlbum(e.dave, "Dave's")
	e.fb.Share(davesAlbum, e.ub, "viewer")
	e.fb.AddToAlbum(davesAlbum, ours[0])
	e.tsEditPairState(func(ps *state.PairState) {
		it := ps.Item("forged-key")
		it.Ours[B] = daves
	})
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	want := map[string]bool{ours[1]: true, ours[2]: true}
	for _, id := range append(append([]string{}, ours...), bobs, daves) {
		a, ok := e.fb.Asset(id)
		if !ok || a.IsTrashed != want[id] {
			t.Errorf("asset %s: exists %v trashed %v, want trashed %v", id, ok, a.IsTrashed, want[id])
		}
	}
	if len(rep.Trashed) != 1 || rep.Trashed[0].Assets != 2 {
		t.Errorf("trashed report %+v", rep.Trashed)
	}
}

// trashOurs: a pair state that records no own uploads trashes nothing and never even
// lists the album.
func TestTeardownSafetyEmptyOurs(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.tsEditPairState(func(ps *state.PairState) {
		if len(ps.Items) == 0 {
			t.Fatal("no items in pair state")
		}
		for _, it := range ps.Items {
			if it != nil {
				it.Ours = [2]string{}
			}
		}
	})
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	searches := countReqs(e.fb, "POST", "/api/search/metadata")
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Trashed) != 0 || len(rep.Errors) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 0 || tsTrashRequests(e.fb) != 0 {
		t.Errorf("trashed without own uploads: %v", got)
	}
	if n := countReqs(e.fb, "POST", "/api/search/metadata"); n != searches {
		t.Errorf("album listed %d times without own uploads", n-searches)
	}
}

// A failing trash call fails the step: AssetsDone stays unset, the album is kept, and
// the step is retried next cycle.
func TestTeardownSafetyTrashFailsIsRetried(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/assets", Status: 503, Times: 2}) // MaxRetries 1
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || len(rep.Trashed) != 0 || !tsHasError(rep, "trash assets") {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if tp, _ := e.tsProgress(); tp == nil || !tp.Left || tp.AssetsDone || tp.AlbumDeleted {
		t.Fatalf("progress %+v", tp)
	}
	if _, ok := e.fb.Album(e.holidayB); !ok || len(e.tsTrashedB(ours)) != 0 {
		t.Fatal("album deleted or assets trashed after a failed trash call")
	}
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 || len(rep.Trashed) != 1 {
		t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 3 {
		t.Errorf("trashed on resume %v", got)
	}
	if _, ok := e.fb.Album(e.holidayB); ok {
		t.Error("album not deleted on resume")
	}
}

// Leaving the original fails and the album is still there: nothing else happens, the
// step is retried. A failed leave whose album is confirmed gone counts as done.
func TestTeardownSafetyLeave(t *testing.T) {
	t.Run("fails, still a member", func(t *testing.T) {
		e := newTeardownEnv(t)
		ours := e.syncedB()
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		e.fa.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayA + "/user/me", Status: 503, Times: 2})
		ps, rep := e.discover()
		if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "leave album") {
			t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
		}
		tp, _ := e.tsProgress()
		if tp == nil || tp.Left || tp.AssetsDone || tp.KeptAlbum {
			t.Fatalf("progress %+v", tp)
		}
		if _, ok := e.fb.Album(e.holidayB); !ok || !e.memberOfA() || len(e.tsTrashedB(ours)) != 0 {
			t.Fatal("teardown went past a failed leave")
		}
		ps, rep = e.discover()
		if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
			t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
		}
		if e.memberOfA() {
			t.Error("not left on resume")
		}
	})
	t.Run("fails, confirmed gone", func(t *testing.T) {
		e := newTeardownEnv(t)
		e.fb.LeaveAlbum(e.holidayB, e.dave)
		// The leave reports an error but the album is not accessible anymore (e.g. a retried
		// leave whose first attempt succeeded).
		e.fa.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayA + "/user/me", Status: 503, Times: 2})
		e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums/" + e.holidayA, Status: 400, Body: tsNotFoundBody})
		ps, rep := e.discover()
		if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
			t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
		}
		if _, ok := e.fb.Album(e.holidayB); ok {
			t.Error("created album not deleted")
		}
	})
}

// Saving the progress fails before the first step: nothing is done; the teardown runs
// next cycle.
func TestTeardownSafetySaveFailsBeforeFirstStep(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	restore := e.tsReadOnlyDir()
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	ps, rep, err := e.tsPass()
	if err == nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "save discovery state") {
		t.Fatalf("err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb || !e.memberOfA() {
		t.Fatal("teardown step ran although its start could not be persisted")
	}
	restore()
	if tp, _ := e.tsProgress(); tp != nil {
		t.Fatalf("progress persisted: %+v", tp)
	}
	ps, rep, err = e.tsPass()
	if err != nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if e.memberOfA() || len(e.tsTrashedB(ours)) != 3 {
		t.Error("teardown incomplete on resume")
	}
}

// Saving after the leave fails: the run stops (no trash, no delete); next cycle the
// original album is gone for the sync user, which counts as left, and the teardown ends.
func TestTeardownSafetySaveFailsAfterLeave(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	// Persist a teardown that has not left yet (failed leave).
	e.fa.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayA + "/user/me", Status: 503, Times: 2})
	if _, rep := e.discover(); !tsHasError(rep, "leave album") {
		t.Fatalf("report %+v", rep)
	}
	restore := e.tsReadOnlyDir()
	ps, rep, err := e.tsPass()
	if err == nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "save discovery state") {
		t.Fatalf("err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if e.memberOfA() {
		t.Fatal("leave not executed")
	}
	if _, ok := e.fb.Album(e.holidayB); !ok || len(e.tsTrashedB(ours)) != 0 || tsTrashRequests(e.fb) != 0 {
		t.Fatal("teardown went past an unpersisted step")
	}
	restore()
	if tp, _ := e.tsProgress(); tp == nil || tp.Left {
		t.Fatalf("progress %+v", tp)
	}
	ps, rep, err = e.tsPass()
	if err != nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); ok || len(e.tsTrashedB(ours)) != 3 {
		t.Error("teardown incomplete on resume")
	}
}

// Saving after the trash step fails: the album is not deleted in that run; next cycle the
// step is redone (nothing left to trash) and the album deleted.
func TestTeardownSafetySaveFailsAfterTrash(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Cleanup(func() { _ = os.Chmod(e.dir, 0o755) })
	var once sync.Once
	// The album listing of the trash step is the only search of a discovery pass.
	e.fb.OnSearchPage(func(int) { once.Do(func() { _ = os.Chmod(e.dir, 0o555) }) })
	ps, rep, err := e.tsPass()
	e.fb.OnSearchPage(nil)
	if err == nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "save discovery state") {
		t.Fatalf("err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); !ok {
		t.Fatal("album deleted although the trash step was not persisted")
	}
	if got := e.tsTrashedB(ours); len(got) != 3 {
		t.Fatalf("trashed %v", got)
	}
	if err := os.Chmod(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if tp, _ := e.tsProgress(); tp == nil || !tp.Left || tp.AssetsDone {
		t.Fatalf("progress %+v", tp)
	}
	ps, rep, err = e.tsPass()
	if err != nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); ok {
		t.Error("album not deleted on resume")
	}
	for _, id := range ours {
		if _, ok := e.fb.Asset(id); !ok {
			t.Errorf("asset %s gone (must only be trashed)", id)
		}
	}
}

// The delete fails, then the save after the delete fails: next cycle the delete is
// retried, the album is confirmed gone and the teardown ends.
func TestTeardownSafetySaveFailsAfterDelete(t *testing.T) {
	e := newTeardownEnv(t)
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fb.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayB, Status: 503, Times: 2})
	if _, rep := e.discover(); !tsHasError(rep, "delete album") {
		t.Fatalf("report %+v", rep)
	}
	restore := e.tsReadOnlyDir()
	ps, rep, err := e.tsPass()
	if err == nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "save discovery state") {
		t.Fatalf("err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if _, ok := e.fb.Album(e.holidayB); ok {
		t.Fatal("album not deleted")
	}
	restore()
	if tp, _ := e.tsProgress(); tp == nil || !tp.AssetsDone || tp.AlbumDeleted {
		t.Fatalf("progress %+v", tp)
	}
	ps, rep, err = e.tsPass()
	if err != nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if _, ok := e.tsProgress(); ok {
		t.Error("torn-down pair still in discovery state")
	}
}

// KeptAlbum: the sync user owns the original; both albums are tombstoned with the
// teardown reason, the created album is never deleted and nothing is re-created.
func TestTeardownSafetyKeptAlbumTombstonesBoth(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Discovery = DiscoveryAll
		o.DiscoveryShare[B] = []string{"dave@example.test"}
	})
	dave, _ := e.fb.AddUser("dave")
	hol := e.fa.CreateAlbum(e.ua, "Holiday")
	e.discover()
	holB := albumByName(e.fb, e.ub, "Holiday")[0]
	e.fb.LeaveAlbum(holB, dave)
	_, rep := e.discover()
	if len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("report %+v", rep)
	}
	d, err := e.st.PeekDiscovery()
	if err != nil {
		t.Fatal(err)
	}
	tomb := map[[2]string]bool{}
	for _, tb := range d.Tombstones {
		if tb.Reason == teardownReason {
			tomb[[2]string{Side(tb.Side).String(), tb.AlbumID}] = true
		}
	}
	if !tomb[[2]string{A.String(), hol}] || !tomb[[2]string{B.String(), holB}] || len(tomb) != 2 {
		t.Errorf("tombstones %+v", d.Tombstones)
	}
	if n := countReqs(e.fb, "DELETE", "/api/albums/"+holB); n != 0 {
		t.Errorf("created album delete requested %d times", n)
	}
	if n := countReqs(e.fa, "DELETE", "/api/albums/"+hol+"/user/me"); n != 0 {
		t.Errorf("leave of an owned album requested %d times", n)
	}
	// Both albums are skipped as torn down from now on.
	ps, rep := e.discover()
	if hasPair(ps, DiscoveredPairPrefix+hol) || len(rep.Skipped) != 2 || len(rep.Created) != 0 {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
}

// Resuming the leave step: the original album cannot be read (not listed, GET fails) →
// the step fails without guessing; retried next cycle.
func TestTeardownSafetyLeaveLookupFails(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	e.fa.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.holidayA + "/user/me", Status: 503, Times: 2})
	if _, rep := e.discover(); !tsHasError(rep, "leave album") {
		t.Fatalf("report %+v", rep)
	}
	// Listing glitch on A (the album is missing) and the confirming GET fails.
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums", Status: 200, Body: "[]"})
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/albums/" + e.holidayA, Status: 503, Times: 2})
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	ps, rep := e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || !tsHasError(rep, "read album on side a") {
		t.Fatalf("pairs %v report %+v", pairNames(ps), rep)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb || !e.memberOfA() {
		t.Fatal("teardown went past an unreadable original album")
	}
	if tp, _ := e.tsProgress(); tp == nil || tp.Left {
		t.Fatalf("progress %+v", tp)
	}
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
	}
	if e.memberOfA() || len(e.tsTrashedB(ours)) != 3 {
		t.Error("teardown incomplete on resume")
	}
}

// Cancellation during the trash step trashes nothing, is not reported as an error, and
// keeps the progress for the next cycle.
func TestTeardownSafetyCancelDuringTrashStep(t *testing.T) {
	e := newTeardownEnv(t)
	ours := e.syncedB()
	e.fb.LeaveAlbum(e.holidayB, e.dave)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fb.OnSearchPage(func(int) { cancel() })
	ps, rep, err := e.eng.Pairs(ctx, nil)
	e.fb.OnSearchPage(nil)
	if err == nil || hasPair(ps, e.pair.Name) || len(rep.TornDown) != 0 || len(rep.Errors) != 0 {
		t.Fatalf("err %v pairs %v report %+v", err, pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 0 || tsTrashRequests(e.fb) != 0 {
		t.Fatalf("trashed after cancellation: %v", got)
	}
	if tp, _ := e.tsProgress(); tp == nil || !tp.Left || tp.AssetsDone {
		t.Fatalf("progress %+v", tp)
	}
	ps, rep = e.discover()
	if hasPair(ps, e.pair.Name) || len(rep.TornDown) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("resume: pairs %v report %+v", pairNames(ps), rep)
	}
	if got := e.tsTrashedB(ours); len(got) != 3 {
		t.Errorf("trashed on resume %v", got)
	}
}
