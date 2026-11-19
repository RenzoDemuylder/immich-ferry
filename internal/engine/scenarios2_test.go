package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

func setDesc(srv *fakeimmich.Server, id, d string) {
	srv.Edit(id, func(a *fakeimmich.FakeAsset) { a.Exif.Description = &d })
}

func pairedWithExif(t *testing.T, mods ...func(*Options)) (*env, string, string) {
	e := newEnv(t, mods...)
	a := e.seedA("x", withExif("base"))
	b := e.seedB("x", withExif("base"))
	e.run() // records baselines
	return e, a, b
}

func TestMetadataEditsBothDirections(t *testing.T) {
	e, a, b := pairedWithExif(t)
	setDesc(e.fa, a, "from-a")
	s := e.run()
	if s.MetaUpdates.AtoB != 1 || derefS(e.onB("x").Exif.Description) != "from-a" {
		t.Fatalf("%+v", s)
	}
	e.fb.Edit(b, func(x *fakeimmich.FakeAsset) { r := 4; x.Exif.Rating = &r })
	s = e.run()
	if s.MetaUpdates.BtoA != 1 || e.onA("x").Exif.Rating == nil || *e.onA("x").Exif.Rating != 4 {
		t.Fatalf("%+v", s)
	}
	if s = e.run(); s.Mutations != 0 {
		t.Fatalf("not converged %+v", s)
	}
}

func TestMetadataConflictWinner(t *testing.T) {
	for _, w := range []Side{A, B} {
		e, a, b := pairedWithExif(t, func(o *Options) { o.ConflictWinner = w })
		setDesc(e.fa, a, "A-new")
		setDesc(e.fb, b, "B-new")
		e.run()
		want := map[Side]string{A: "A-new", B: "B-new"}[w]
		if derefS(e.onA("x").Exif.Description) != want || derefS(e.onB("x").Exif.Description) != want {
			t.Errorf("winner %v: A=%q B=%q", w, derefS(e.onA("x").Exif.Description), derefS(e.onB("x").Exif.Description))
		}
	}
}

func TestMetadataNotOwnerSkipped(t *testing.T) {
	e := newEnv(t)
	other, _ := e.fa.AddUser("carol")
	e.fa.Share(e.albumA, other, "editor")
	oid := e.seed(e.fa, other, e.albumA, "theirs", withExif("base"))
	e.run()
	e.settle()
	b := e.onB("theirs")
	setDesc(e.fb, b.ID, "edited-on-b")
	s := e.run()
	if s.Skipped[SkipNotOwner] == 0 || s.MetaUpdates.BtoA != 0 {
		t.Fatalf("%+v", s)
	}
	if a, _ := e.fa.Asset(oid); derefS(a.Exif.Description) == "edited-on-b" {
		t.Error("wrote to non-owned asset")
	}
}

func TestDateEditWithTimezoneConverges(t *testing.T) {
	e, a, _ := pairedWithExif(t)
	e.fa.Edit(a, func(x *fakeimmich.FakeAsset) {
		d := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
		tz := "Europe/Berlin"
		x.Exif.DateTimeOriginal, x.Exif.TimeZone = &d, &tz
	})
	s := e.run()
	if s.MetaUpdates.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	bd := e.onB("x").Exif
	if !bd.DateTimeOriginal.Equal(time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)) || *bd.TimeZone != "UTC+2" {
		t.Fatalf("%v %v", bd.DateTimeOriginal, *bd.TimeZone)
	}
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	for i := 0; i < 2; i++ {
		if s = e.run(); s.Mutations != 0 || s.MetaUpdates.AtoB+s.MetaUpdates.BtoA != 0 {
			t.Fatalf("ping-pong %+v", s)
		}
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
		t.Error("extra mutations")
	}
}

func TestAlbumInfoBothDirections(t *testing.T) {
	e := newEnv(t)
	e.seedA("x")
	e.run()
	ctx := context.Background()
	if _, err := e.clA.UpdateAlbumInfo(ctx, e.albumA, immich.AlbumInfoUpdate{Name: immich.Some("Renamed A"), Description: immich.Some("desc A")}); err != nil {
		t.Fatal(err)
	}
	s := e.run()
	if s.AlbumInfoUpdated.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	al, _ := e.fb.Album(e.albumB)
	if al.Name != "Renamed A" || al.Description != "desc A" {
		t.Fatalf("%+v", al)
	}
	if _, err := e.clB.UpdateAlbumInfo(ctx, e.albumB, immich.AlbumInfoUpdate{Name: immich.Some("Renamed B")}); err != nil {
		t.Fatal(err)
	}
	s = e.run()
	if s.AlbumInfoUpdated.BtoA != 1 {
		t.Fatalf("%+v", s)
	}
	if al, _ := e.fa.Album(e.albumA); al.Name != "Renamed B" {
		t.Fatalf("%+v", al)
	}
	if s = e.run(); s.Mutations != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestPendingMetaEnforcedAfterExtract(t *testing.T) {
	e := newEnv(t)
	a := e.seedA("x", withExif("orig"))
	s := e.run()
	if s.Pending != 1 {
		t.Fatalf("%+v", s)
	}
	setDesc(e.fa, a, "edited")
	if s = e.run(); s.Pending != 1 || s.Mutations != 0 {
		t.Fatalf("%+v", s)
	}
	e.fb.Extract(e.onB("x").ID)
	e.run()
	if d := derefS(e.onB("x").Exif.Description); d != "edited" {
		t.Fatalf("B description %q", d)
	}
	e.run()
	if s = e.run(); s.Pending != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestMassRemovalGuardAndApprove(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := 0; i < 12; i++ {
		ids = append(ids, e.seedA(fmt.Sprintf("m%02d", i)))
	}
	e.run()
	e.fa.RemoveFromAlbum(e.albumA, ids[:11]...)
	s := e.run()
	if !s.GuardTripped[1] || s.Unlinked.AtoB != 0 || s.Result != ResultPartial || len(e.fb.AlbumAssetIDs(e.albumB)) != 12 {
		t.Fatalf("%+v", s)
	}
	if err := e.st.PutMarker("trip", "approve-removals"); err != nil {
		t.Fatal(err)
	}
	s = e.run()
	if s.Unlinked.AtoB != 11 || s.GuardTripped[1] || len(e.fb.AlbumAssetIDs(e.albumB)) != 1 {
		t.Fatalf("%+v", s)
	}
	if ok, _ := e.st.TakeMarker("trip", "approve-removals"); ok {
		t.Error("marker not consumed")
	}
}

func TestPropagateRemovalsFalse(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.PropagateRemovals = false })
	x := e.seedA("x")
	e.run()
	e.fa.RemoveFromAlbum(e.albumA, x)
	for i := 0; i < 2; i++ {
		s := e.run()
		if s.Unlinked.AtoB != 0 || s.Copied.BtoA != 0 || s.Mutations != 0 {
			t.Fatalf("%+v", s)
		}
	}
	if e.inAlbumA(x) || !e.inAlbumB(e.onB("x").ID) {
		t.Error("state changed")
	}
}

func TestBackoffAfterDownloadFailures(t *testing.T) {
	e := newEnv(t)
	x := e.seedA("x")
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + x + "/original", Status: 500, Times: 3})
	s := e.run()
	if s.ItemErrorCount != 1 || s.Result != ResultPartial || s.Copied.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	f := e.loadState().Items[e.checksumA(x)].Failure
	if f == nil || f.Count != 1 || !f.Next.Equal(e.clk.Now().Add(10*time.Minute)) {
		t.Fatalf("failure %+v", f)
	}
	dl := e.countReq(e.fa, "GET", "/original")
	s = e.run()
	if s.Skipped[SkipBackoff] != 1 || e.countReq(e.fa, "GET", "/original") != dl {
		t.Fatalf("%+v", s)
	}
	e.clk.Advance(11 * time.Minute)
	s = e.run()
	if s.Copied.AtoB != 1 || s.ItemErrorCount != 0 {
		t.Fatalf("%+v", s)
	}
	e.noTempFiles()
}

func TestQuotaStopsUploads(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 1 })
	for _, n := range []string{"q1", "q2", "q3"} {
		e.seedA(n)
	}
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets/bulk-upload-check"}) // no-op: consume the check
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets", Status: 400, Body: `{"message":"Quota has been exceeded!"}`, Times: 50})
	s := e.run()
	if s.Skipped[SkipQuota] != 3 || s.Copied.AtoB != 0 || s.Result != ResultPartial || s.ItemErrorCount != 1 {
		t.Fatalf("%+v", s)
	}
	if e.countReq(e.fb, "POST", "/api/assets") != 1 {
		t.Errorf("uploads attempted: %d", e.countReq(e.fb, "POST", "/api/assets"))
	}
	for _, it := range e.loadState().Items {
		if it.Failure != nil {
			t.Error("quota recorded as failure")
		}
	}
}

func TestTooLargeBackoff(t *testing.T) {
	e := newEnv(t)
	e.seedA("big")
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets/bulk-upload-check"})
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets", Status: 413, Times: 50})
	s := e.run()
	if s.Skipped[SkipTooLarge] != 1 {
		t.Fatalf("%+v", s)
	}
	for _, it := range e.loadState().Items {
		if it.Failure == nil || !it.Failure.Next.Equal(e.clk.Now().Add(30*24*time.Hour)) {
			t.Fatalf("%+v", it.Failure)
		}
	}
}

func TestTruncatedDownloadRetried(t *testing.T) {
	e := newEnv(t)
	x := e.seedA("x")
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + x + "/original", TruncateAt: 5, Times: 2})
	s := e.run()
	if s.Copied.AtoB != 1 || s.ItemErrorCount != 0 {
		t.Fatalf("%+v", s)
	}
	e.noTempFiles()
	e2 := newEnv(t)
	y := e2.seedA("y")
	e2.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + y + "/original", TruncateAt: 5, Times: 3})
	s = e2.run()
	if s.Copied.AtoB != 0 || s.ItemErrorCount != 1 || e2.countReq(e2.fb, "POST", "/api/assets") != 0 {
		t.Fatalf("%+v", s)
	}
	e2.noTempFiles()
}

func TestConcurrentMutationDuringPagingProbe(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := 0; i < 1100; i++ {
		n := fmt.Sprintf("p%04d", i)
		ids = append(ids, e.seedA(n))
		e.seedB(n)
	}
	e.run()
	fired := false
	e.fa.OnSearchPage(func(page int) {
		if page == 1 && !fired {
			fired = true
			e.fa.Trash(ids[0]) // shifts offsets: one member falls into a gap
		}
	})
	s := e.run()
	e.fa.OnSearchPage(nil)
	if !fired || s.Unlinked.AtoB != 0 || s.Skipped[SkipStale] != 1 || s.SnapshotConsistent[0] {
		t.Fatalf("fired=%v %+v", fired, s)
	}
	if len(e.fb.AlbumAssetIDs(e.albumB)) != 1100 {
		t.Errorf("B album has %d", len(e.fb.AlbumAssetIDs(e.albumB)))
	}
	// the genuinely trashed member is caught by the next (consistent) cycle
	s = e.run()
	if s.Unlinked.AtoB != 1 || len(e.fb.AlbumAssetIDs(e.albumB)) != 1099 {
		t.Fatalf("%+v", s)
	}
}

func TestCancelMidCopyNoFailureRecorded(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 4 })
	for _, n := range []string{"c1", "c2", "c3"} {
		e.seedA(n)
	}
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets/bulk-upload-check"})
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets", Delay: 1500 * time.Millisecond, Times: 3})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, "uploads in flight", func() bool { return e.countReq(e.fb, "POST", "/api/assets") >= 3 })
		cancel()
	}()
	s, err := e.eng.RunPair(ctx, e.pair())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if s.ItemErrorCount != 0 {
		t.Errorf("%+v", s)
	}
	if !e.stateFileExists() {
		t.Fatal("state not saved")
	}
	for k, it := range e.loadState().Items {
		if it.Failure != nil {
			t.Errorf("failure recorded for %s", k)
		}
	}
	e.noTempFiles()
	s = e.run()
	if s.Copied.AtoB != 3 || s.Result != ResultOK || len(e.fb.AlbumAssetIDs(e.albumB)) != 3 {
		t.Fatalf("%+v", s)
	}
}

func TestIdempotencyAfterPendingDrains(t *testing.T) {
	e := newEnv(t)
	e.seedA("a", withExif("da"))
	e.seedB("b", withExif("db"))
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("live", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	e.settle()
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	s := e.run()
	if s.Mutations != 0 || e.fa.Mutations() != ma || e.fb.Mutations() != mb || s.Result != ResultOK || s.Pending != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDryRunAndPlanPairMutateNothing(t *testing.T) {
	e := newEnv(t)
	e.seedA("a")
	e.seedB("b")
	ma, mb := e.fa.Mutations(), e.fb.Mutations()
	plan, s, err := e.eng.PlanPair(context.Background(), e.pair())
	if err != nil || len(plan.Copies) != 2 || s.Result != ResultDryRun || s.Copied.AtoB != 1 || s.Copied.BtoA != 1 {
		t.Fatalf("%v %+v %+v", err, plan, s)
	}
	dry := e.newEngine(func() Options { o := e.opts; o.DryRun = true; return o }())
	s, err = dry.RunPair(context.Background(), e.pair())
	if err != nil || s.Result != ResultDryRun || s.Copied.AtoB != 1 || s.Mutations != 0 {
		t.Fatalf("%v %+v", err, s)
	}
	// missing album id: config error, nothing mutated
	p := Pair{Name: "trip", A: AlbumRef{ID: e.albumA}}
	if _, _, err = dry.PlanPair(context.Background(), p); !errors.Is(err, ErrConfig) {
		t.Fatalf("missing album id: %v", err)
	}
	if e.fa.Mutations() != ma || e.fb.Mutations() != mb || e.stateFileExists() {
		t.Error("mutated")
	}
}

func TestFingerprintResetOnAlbumChange(t *testing.T) {
	e := newEnv(t)
	e.seedA("x")
	e.run()
	a2 := e.fa.CreateAlbum(e.ua, "Other")
	p := e.pair()
	p.A.ID = a2
	s, err := e.eng.RunPair(context.Background(), p)
	if err != nil || !s.Bootstrap || s.Unlinked.BtoA+s.Unlinked.AtoB != 0 {
		t.Fatalf("%v %+v", err, s)
	}
	ents, _ := os.ReadDir(filepath.Join(e.dir, "pairs"))
	found := false
	for _, x := range ents {
		found = found || strings.Contains(x.Name(), ".fpreset-")
	}
	if !found {
		t.Error("old state not moved aside")
	}
}

func TestExternalLibraryAssetHashedAndCached(t *testing.T) {
	e := newEnv(t)
	e.seedA("ext", func(o *fakeimmich.SeedOpts) { o.LibraryID = "lib" })
	s := e.run()
	if s.Copied.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	b := e.onB("ext")
	if b.Checksum != sha1b64Of(e.content("ext")) {
		t.Errorf("checksum %s", b.Checksum)
	}
	ps := e.loadState()
	if len(ps.Hashes) != 1 {
		t.Fatalf("hashes %v", ps.Hashes)
	}
	dl := e.countReq(e.fa, "GET", "/original")
	s = e.run()
	if s.Copied.AtoB != 0 || s.Mutations != 0 || e.countReq(e.fa, "GET", "/original") != dl {
		t.Fatalf("%+v", s)
	}
}

func TestRequiredPermissionsTable(t *testing.T) {
	base := []string{"album.read", "albumAsset.create", "asset.download", "asset.read", "asset.update", "asset.upload", "user.read"}
	with := func(extra ...string) []string {
		out := append(append([]string{}, base...), extra...)
		sortStrings(out)
		return out
	}
	cases := []struct {
		o    Options
		want []string
	}{
		{Options{}, with()},
		{Options{RestoreTrashed: true}, with("asset.delete")},
		{Options{SyncAlbumInfo: true}, with("album.update")},
		{Options{Discovery: DiscoveryAll}, with("album.create")},
		{Options{Discovery: DiscoveryMarker}, with("album.create")},
		{Options{PropagateRemovals: true}, with("albumAsset.delete")},
		{Options{RestoreTrashed: true, SyncAlbumInfo: true, PropagateRemovals: true},
			with("asset.delete", "album.update", "albumAsset.delete")},
	}
	for i, c := range cases {
		if got := RequiredPermissions(c.o); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%d: %v want %v", i, got, c.want)
		}
	}
}

func TestPreflight(t *testing.T) {
	e := newEnv(t)
	if err := e.eng.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.fa.SetVersion(2, 9, 0)
	err := e.eng.Preflight(context.Background())
	var pe *PreflightError
	if !errors.As(err, &pe) || pe.Side != A || pe.Version.Major != 2 || !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
	e.fa.SetVersion(3, 1, 0)
	if err := e.eng.Preflight(context.Background()); !errors.Is(err, ErrConfig) {
		t.Fatalf("3.1: %v", err)
	}
	e.fa.SetVersion(3, 4, 1)
	// limited key on B
	_, key := e.fb.AddUser("limited", "asset.read", "user.read")
	cl := e.client(e.fb, key)
	eng, _ := New(e.opts, e.clA, cl, e.st, nil)
	err = eng.Preflight(context.Background())
	if !errors.As(err, &pe) || pe.Side != B || !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
	want := []string{}
	for _, p := range RequiredPermissions(e.opts) {
		if p != "asset.read" && p != "user.read" {
			want = append(want, p)
		}
	}
	if !reflect.DeepEqual(pe.Missing, want) {
		t.Errorf("missing %v want %v", pe.Missing, want)
	}
	// "all" is a superset
	_, allKey := e.fb.AddUser("root")
	eng, _ = New(e.opts, e.clA, e.client(e.fb, allKey), e.st, nil)
	if err := eng.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	// RunPair surfaces preflight failures
	e.fa.SetVersion(4, 0, 0)
	e2 := e.newEngine(e.opts)
	if _, err := e2.RunPair(context.Background(), e.pair()); !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
}

func TestAlbumResolutionErrors(t *testing.T) {
	e := newEnv(t)
	e.seedA("x")
	e.run()
	// The album id stored in state is never a fallback for a missing configured id.
	_, err := e.eng.RunPair(context.Background(), Pair{Name: "trip", A: AlbumRef{ID: e.albumA}})
	if !errors.Is(err, ErrConfig) {
		t.Errorf("missing id: %v", err)
	}
	_, err = e.eng.RunPair(context.Background(), Pair{Name: "trip", A: AlbumRef{ID: "00000000-0000-4000-8000-000000000000"}, B: AlbumRef{ID: e.albumB}})
	if !errors.Is(err, ErrConfig) {
		t.Errorf("gone: %v", err)
	}
}

func TestByteBudget(t *testing.T) {
	b := newByteBudget(100)
	ctx := context.Background()
	r1, _ := b.acquire(ctx, 60)
	got := make(chan func())
	go func() { r, _ := b.acquire(ctx, 500); got <- r }() // oversized: needs the whole budget
	select {
	case <-got:
		t.Fatal("acquired while others hold budget")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	r2 := <-got
	if b.inUse() != 100 {
		t.Errorf("inUse %d", b.inUse())
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { _, err := b.acquire(cctx, 1); done <- err }()
	cancel()
	if err := <-done; err == nil {
		t.Error("expected cancel error")
	}
	r2()
}
