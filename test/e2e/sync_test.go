//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/engine"
	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
	"immich-ferry/test/e2e/testutil"
)

type tlogWriter struct{ t testing.TB }

func (w tlogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

type syncer struct {
	t          *testing.T
	ctx        context.Context
	instA      *testutil.Instance
	instB      *testutil.Instance
	ua, ub     *testutil.User
	opts       engine.Options
	keyA, keyB string
	eng        *engine.Engine
	st         *state.Store
	dir        string
	name       string
	pair       engine.Pair // name only; album ids are filled in by p()
	albumA     string
	albumBID   string
}

func e2eInstances(t *testing.T) (*testutil.Instance, *testutil.Instance) {
	t.Helper()
	env := testutil.E2EEnv()
	if env["IMMICH_A_URL"] == "" || env["IMMICH_B_URL"] == "" {
		t.Skip("IMMICH_*_URL not set; run `make e2e-up` first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := testutil.Bootstrap(ctx, env["IMMICH_A_URL"])
	if err != nil {
		t.Fatalf("bootstrap A: %v", err)
	}
	b, err := testutil.Bootstrap(ctx, env["IMMICH_B_URL"])
	if err != nil {
		t.Fatalf("bootstrap B: %v", err)
	}
	return a, b
}

// newSyncer creates fresh users on both instances, minimal API keys, an own state dir and an engine.
func newSyncer(t *testing.T, mutate ...func(*engine.Options)) *syncer {
	t.Helper()
	s := &syncer{t: t, ctx: context.Background()}
	s.instA, s.instB = e2eInstances(t)
	var err error
	if s.ua, err = testutil.NewUser(s.ctx, s.instA, "sync-a"); err != nil {
		t.Fatalf("user A: %v", err)
	}
	if s.ub, err = testutil.NewUser(s.ctx, s.instB, "sync-b"); err != nil {
		t.Fatalf("user B: %v", err)
	}
	s.opts = defaultOpts(t)
	for _, m := range mutate {
		m(&s.opts)
	}
	perms := engine.RequiredPermissions(s.opts)
	if s.keyA, err = s.ua.Key(s.ctx, perms); err != nil {
		t.Fatalf("key A: %v", err)
	}
	if s.keyB, err = s.ub.Key(s.ctx, perms); err != nil {
		t.Fatalf("key B: %v", err)
	}
	s.dir = t.TempDir()
	s.name = fmt.Sprintf("e2e-album-%d", testutil.UniqueSeed())
	s.pair = engine.Pair{Name: "e2e-pair"}
	s.eng, s.st = s.buildEngine(s.opts, s.keyA, s.keyB, s.dir)
	return s
}

func (s *syncer) buildEngine(o engine.Options, keyA, keyB, dir string) (*engine.Engine, *state.Store) {
	t := s.t
	t.Helper()
	log := slog.New(slog.NewTextHandler(tlogWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ca, err := immich.New(immich.Config{BaseURL: s.instA.URL, APIKey: keyA, Logger: log.With("side", "a")})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := immich.New(immich.Config{BaseURL: s.instB.URL, APIKey: keyB, Logger: log.With("side", "b")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng, err := engine.New(o, ca, cb, st, log)
	if err != nil {
		t.Fatal(err)
	}
	return eng, st
}

func (s *syncer) mkAlbumA(desc string, assets ...string) string {
	s.t.Helper()
	id, err := s.ua.API.CreateAlbum(s.ctx, s.name, desc, assets...)
	if err != nil {
		s.t.Fatalf("create album A: %v", err)
	}
	s.albumA = id
	return id
}

func (s *syncer) mkAlbumB(desc string, assets ...string) string {
	s.t.Helper()
	id, err := s.ub.API.CreateAlbum(s.ctx, s.name, desc, assets...)
	if err != nil {
		s.t.Fatalf("create album B: %v", err)
	}
	s.albumBID = id
	return id
}

// albumB returns the album on B, creating an empty one on first use (the engine never creates albums).
func (s *syncer) albumB() string {
	s.t.Helper()
	if s.albumBID == "" {
		s.mkAlbumB("")
	}
	return s.albumBID
}

// p returns the pair with both album ids set.
func (s *syncer) p() engine.Pair {
	s.t.Helper()
	if s.albumA == "" {
		s.t.Fatal("album A not created")
	}
	p := s.pair
	p.A, p.B = engine.AlbumRef{ID: s.albumA}, engine.AlbumRef{ID: s.albumB()}
	return p
}

func (s *syncer) settle() {
	s.t.Helper()
	testutil.WaitQueuesIdle(s.t, s.instA.AdminAPI())
	testutil.WaitQueuesIdle(s.t, s.instB.AdminAPI())
}

func (s *syncer) logSum(label string, sum engine.RunSummary) {
	s.t.Logf("%s: result=%s bootstrap=%v pending=%d deferred=%d mutations=%d copied=%+v unlinked=%+v restored=%+v meta=%+v albuminfo=%+v skipped=%v errors=%d guard=%v",
		label, sum.Result, sum.Bootstrap, sum.Pending, sum.Deferred, sum.Mutations, sum.Copied, sum.Unlinked, sum.Restored,
		sum.MetaUpdates, sum.AlbumInfoUpdated, sum.Skipped, sum.ItemErrorCount, sum.GuardTripped)
	for _, e := range sum.ItemErrors {
		s.t.Logf("  item error: %+v", e)
	}
}

// run does one RunPair and fails on error or item errors.
func (s *syncer) run(label string) engine.RunSummary {
	s.t.Helper()
	sum, err := s.eng.RunPair(s.ctx, s.p())
	s.logSum(label, sum)
	if err != nil {
		s.t.Fatalf("%s: RunPair: %v", label, err)
	}
	if sum.ItemErrorCount > 0 {
		s.t.Fatalf("%s: %d item errors: %+v", label, sum.ItemErrorCount, sum.ItemErrors)
	}
	return sum
}

// runUntilQuiet runs the pair until Pending==0 (max 6 runs, waiting for server-side jobs between runs) and
// asserts that one further run performs no mutations.
func (s *syncer) runUntilQuiet() []engine.RunSummary {
	s.t.Helper()
	var all []engine.RunSummary
	s.settle()
	for i := 1; ; i++ {
		sum := s.run(fmt.Sprintf("run %d", i))
		all = append(all, sum)
		s.settle()
		if sum.Pending == 0 {
			break
		}
		if i >= 6 {
			s.t.Fatalf("still pending=%d after %d runs", sum.Pending, i)
		}
	}
	next := s.run("quiet check")
	if next.Mutations != 0 || next.Pending != 0 {
		s.t.Fatalf("not idempotent: next run mutations=%d pending=%d (want 0)", next.Mutations, next.Pending)
	}
	return append(all, next)
}

func (s *syncer) albumSet(api *testutil.API, album string) map[string]testutil.Asset {
	s.t.Helper()
	m, err := testutil.ChecksumsOf(s.ctx, api, album)
	if err != nil {
		s.t.Fatalf("list album %s: %v", album, err)
	}
	return m
}

func (s *syncer) sortedAll(api *testutil.API) []string {
	s.t.Helper()
	l, err := api.AllAssetIDs(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	sort.Strings(l)
	return l
}

func (s *syncer) get(api *testutil.API, id string) *testutil.Asset {
	s.t.Helper()
	a, err := api.GetAssetTyped(s.ctx, id)
	if err != nil {
		s.t.Fatalf("get asset %s: %v", id, err)
	}
	return a
}

func (s *syncer) mustUpdate(api *testutil.API, id string, f map[string]any) {
	s.t.Helper()
	if _, err := api.UpdateAsset(s.ctx, id, f); err != nil {
		s.t.Fatalf("update asset %s: %v", id, err)
	}
}

func (s *syncer) mustAlbumAssets(api *testutil.API, album string) []string {
	s.t.Helper()
	l, err := api.AlbumAssetIDs(s.ctx, album)
	if err != nil {
		s.t.Fatal(err)
	}
	sort.Strings(l)
	return l
}

// ---------------------------------------------------------------------------------------------

func TestSyncInitial(t *testing.T) {
	s := newSyncer(t)
	p1 := photo(t, s.ua, "p1.jpg", t0)
	p2 := photo(t, s.ua, "p2.jpg", t0.Add(time.Hour))
	p3 := photo(t, s.ua, "p3.jpg", t0.Add(2*time.Hour))
	vid := upload(t, s.ua, "clip.mp4", testutil.MP4(testutil.UniqueSeed()), testutil.UploadOpts{FileCreatedAt: t0.Add(3 * time.Hour)})
	mov := upload(t, s.ua, "IMG_live.MOV", testutil.MOV(testutil.UniqueSeed()), testutil.UploadOpts{FileCreatedAt: t0.Add(4 * time.Hour), Visibility: "hidden"})
	liveData := testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed(), DateTimeOriginal: ptr(t0.Add(4 * time.Hour)), OffsetTime: "+00:00"})
	live := upload(t, s.ua, "IMG_live.JPG", liveData, testutil.UploadOpts{FileCreatedAt: t0.Add(4 * time.Hour), LivePhotoVideoID: mov.ID})
	testutil.WaitExtracted(t, s.ua.API, live.ID)
	s.mkAlbumA("initial", p1.ID, p2.ID, p3.ID, vid.ID, live.ID)

	s.runUntilQuiet()

	albB := s.albumB()
	alb, err := s.ub.API.GetAlbum(s.ctx, albB)
	if err != nil || alb.Name != s.name {
		t.Fatalf("B album: %+v err %v", alb, err)
	}
	setB := s.albumSet(s.ub.API, albB)
	if len(setB) != 5 {
		t.Fatalf("B album has %d assets, want 5", len(setB))
	}
	for _, m := range []media{p1, p2, p3, vid, live} {
		b, ok := setB[m.Sum]
		if !ok {
			t.Fatalf("%s: checksum missing on B", m.Name)
		}
		src := s.get(s.ua.API, m.ID)
		if b.OriginalFileName != m.Name {
			t.Errorf("%s: originalFileName %q", m.Name, b.OriginalFileName)
		}
		if !b.FileCreatedAt.Equal(src.FileCreatedAt) {
			t.Errorf("%s: fileCreatedAt B %s != A %s", m.Name, b.FileCreatedAt, src.FileCreatedAt)
		}
		if b.OwnerID != s.ub.ID {
			t.Errorf("%s: owner %s want %s", m.Name, b.OwnerID, s.ub.ID)
		}
	}
	if setB[vid.Sum].Type != "VIDEO" {
		t.Errorf("video type %q", setB[vid.Sum].Type)
	}
	bl := setB[live.Sum]
	if bl.LivePhotoVideoID == nil {
		t.Fatalf("live photo on B has no livePhotoVideoId")
	}
	bm := s.get(s.ub.API, *bl.LivePhotoVideoID)
	if bm.Checksum != mov.Sum || bm.Visibility != "hidden" || bm.IsTrashed {
		t.Errorf("B motion part: checksum ok=%v visibility=%s trashed=%v", bm.Checksum == mov.Sum, bm.Visibility, bm.IsTrashed)
	}
	if got, err := testutil.ProbeChecksum(s.ctx, s.ub.API, albB, mov.Sum); err != nil || len(got) != 0 {
		t.Errorf("motion part is a member of B album: %v err %v", got, err)
	}
	// A is untouched.
	if got := s.mustAlbumAssets(s.ua.API, s.albumA); len(got) != 5 {
		t.Errorf("A album has %d assets, want 5", len(got))
	}
}

func TestSyncAddOnB(t *testing.T) {
	s := newSyncer(t)
	p1 := photo(t, s.ua, "a1.jpg", t0)
	s.mkAlbumA("", p1.ID)
	s.runUntilQuiet()
	albB := s.albumB()

	b1 := photo(t, s.ub, "b1.jpg", t0.Add(time.Hour))
	if _, err := s.ub.API.AddToAlbum(s.ctx, albB, b1.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	setA := s.albumSet(s.ua.API, s.albumA)
	a, ok := setA[b1.Sum]
	if !ok {
		t.Fatalf("B asset did not appear in A album (%d members)", len(setA))
	}
	if a.OwnerID != s.ua.ID || a.OriginalFileName != "b1.jpg" {
		t.Errorf("copy on A: owner %s name %s", a.OwnerID, a.OriginalFileName)
	}
}

func TestSyncRemoveOnA(t *testing.T) {
	s := newSyncer(t)
	p1 := photo(t, s.ua, "r1.jpg", t0)
	p2 := photo(t, s.ua, "r2.jpg", t0.Add(time.Hour))
	p3 := photo(t, s.ua, "r3.jpg", t0.Add(2*time.Hour))
	s.mkAlbumA("", p1.ID, p2.ID, p3.ID)
	s.runUntilQuiet()
	albB := s.albumB()
	setB := s.albumSet(s.ub.API, albB)
	if len(setB) != 3 {
		t.Fatalf("B has %d, want 3", len(setB))
	}
	victimB := setB[p2.Sum].ID

	if _, err := s.ua.API.RemoveFromAlbum(s.ctx, s.albumA, p2.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	setB = s.albumSet(s.ub.API, albB)
	if _, ok := setB[p2.Sum]; ok || len(setB) != 2 {
		t.Fatalf("B album after removal: %d members, victim present=%v", len(setB), ok)
	}
	a := s.get(s.ub.API, victimB)
	if a.IsTrashed {
		t.Errorf("B asset was trashed")
	}
	if all := s.sortedAll(s.ub.API); !contains(all, victimB) {
		t.Errorf("B asset no longer exists")
	}
	if got := s.mustAlbumAssets(s.ua.API, s.albumA); len(got) != 2 {
		t.Errorf("A album has %d, want 2", len(got))
	}
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func TestSyncTrashOnB(t *testing.T) {
	s := newSyncer(t)
	p1 := photo(t, s.ua, "t1.jpg", t0)
	p2 := photo(t, s.ua, "t2.jpg", t0.Add(time.Hour))
	s.mkAlbumA("", p1.ID, p2.ID)
	s.runUntilQuiet()
	albB := s.albumB()
	bID := s.albumSet(s.ub.API, albB)[p2.Sum].ID

	if err := s.ub.API.Trash(s.ctx, bID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	setA := s.albumSet(s.ua.API, s.albumA)
	if _, ok := setA[p2.Sum]; ok || len(setA) != 1 {
		t.Fatalf("A album after trash on B: %d members, victim present=%v", len(setA), ok)
	}
	a := s.get(s.ua.API, p2.ID)
	if a.IsTrashed {
		t.Errorf("A asset trashed")
	}
	if !s.get(s.ub.API, bID).IsTrashed {
		t.Errorf("B asset not trashed (test setup)")
	}
}

func TestSyncPreexistingBAsset(t *testing.T) {
	s := newSyncer(t)
	data := testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed(), DateTimeOriginal: ptr(t0), OffsetTime: "+00:00"})
	uo := testutil.UploadOpts{FileCreatedAt: t0}
	onB := upload(t, s.ub, "shared.jpg", data, uo)
	testutil.WaitExtracted(t, s.ub.API, onB.ID)
	s.mustUpdate(s.ub.API, onB.ID, map[string]any{"description": "keep me"})
	onA := upload(t, s.ua, "shared.jpg", data, uo)
	testutil.WaitExtracted(t, s.ua.API, onA.ID)
	s.mkAlbumA("", onA.ID)
	s.settle()
	before := s.sortedAll(s.ub.API)

	// Reused duplicates are paired in cycle 1 and merged (fill-empty from B) in cycle 2.
	s.run("pairing run")
	s.runUntilQuiet()

	after := s.sortedAll(s.ub.API)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("B asset set changed: before %v after %v", before, after)
	}
	got, ok := s.albumSet(s.ub.API, s.albumB())[onB.Sum]
	if !ok || got.ID != onB.ID {
		t.Fatalf("pre-existing B asset not reused: member=%v id=%s want %s", ok, got.ID, onB.ID)
	}
	a := s.get(s.ub.API, onB.ID)
	if a.Exif == nil || a.Exif.Description == nil || *a.Exif.Description != "keep me" {
		t.Errorf("B description lost: %+v", a.Exif)
	}
}

func TestSyncTrashedDuplicateRestored(t *testing.T) {
	s := newSyncer(t)
	seed0 := photo(t, s.ua, "seed0.jpg", t0)
	s.mkAlbumA("", seed0.ID)
	s.runUntilQuiet() // clears bootstrap

	data := testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed(), DateTimeOriginal: ptr(t0.Add(time.Hour)), OffsetTime: "+00:00"})
	uo := testutil.UploadOpts{FileCreatedAt: t0.Add(time.Hour)}
	onB := upload(t, s.ub, "dup.jpg", data, uo)
	testutil.WaitExtracted(t, s.ub.API, onB.ID)
	if err := s.ub.API.Trash(s.ctx, onB.ID); err != nil {
		t.Fatal(err)
	}
	onA := upload(t, s.ua, "dup.jpg", data, uo)
	testutil.WaitExtracted(t, s.ua.API, onA.ID)
	if _, err := s.ua.API.AddToAlbum(s.ctx, s.albumA, onA.ID); err != nil {
		t.Fatal(err)
	}
	before := s.sortedAll(s.ub.API)

	s.runUntilQuiet()

	a := s.get(s.ub.API, onB.ID)
	if a.IsTrashed {
		t.Errorf("trashed duplicate was not restored")
	}
	got, ok := s.albumSet(s.ub.API, s.albumB())[onB.Sum]
	if !ok || got.ID != onB.ID {
		t.Errorf("restored duplicate not in B album with the same id: ok=%v id=%s want %s", ok, got.ID, onB.ID)
	}
	after := s.sortedAll(s.ub.API)
	if len(after) != len(before)+1 { // +1: the restored asset becomes visible in the non-trashed listing
		t.Errorf("B asset count before(non-trashed)=%d after=%d", len(before), len(after))
	}
}

func TestSyncMetadata(t *testing.T) {
	s := newSyncer(t)
	p := photo(t, s.ua, "meta.jpg", t0)
	s.mkAlbumA("", p.ID)
	s.runUntilQuiet()
	albB := s.albumB()
	bID := s.albumSet(s.ub.API, albB)[p.Sum].ID

	// A -> B
	s.mustUpdate(s.ua.API, p.ID, map[string]any{
		"description": "from A", "rating": 4, "isFavorite": true, "visibility": "archive",
		"latitude": 10.5, "longitude": 20.5, "dateTimeOriginal": "2022-01-02T03:04:05+02:00",
	})
	s.runUntilQuiet()
	b := s.get(s.ub.API, bID)
	// Favorites are per user and never synced.
	checkMeta(t, "A->B", b, "from A", 4, false, "archive", 10.5, 20.5, time.Date(2022, 1, 2, 1, 4, 5, 0, time.UTC), "")

	// B -> A
	s.mustUpdate(s.ub.API, bID, map[string]any{
		"description": "from B", "rating": 2, "visibility": "timeline",
		"latitude": -33.5, "longitude": 151.5, "dateTimeOriginal": "2023-02-03T04:05:06-05:00",
	})
	s.runUntilQuiet()
	a := s.get(s.ua.API, p.ID)
	checkMeta(t, "B->A", a, "from B", 2, true, "timeline", -33.5, 151.5, time.Date(2023, 2, 3, 9, 5, 6, 0, time.UTC), "")

	// conflict: both change the description, winner is A
	s.mustUpdate(s.ua.API, p.ID, map[string]any{"description": "conflict-a"})
	s.mustUpdate(s.ub.API, bID, map[string]any{"description": "conflict-b"})
	s.runUntilQuiet()
	for side, api := range map[string]*testutil.API{"A": s.ua.API, "B": s.ub.API} {
		id := p.ID
		if side == "B" {
			id = bID
		}
		x := s.get(api, id)
		if x.Exif == nil || x.Exif.Description == nil || *x.Exif.Description != "conflict-a" {
			t.Errorf("conflict: side %s description %v, want conflict-a", side, x.Exif)
		}
	}
}

func checkMeta(t *testing.T, label string, a *testutil.Asset, desc string, rating int, fav bool, vis string, lat, lon float64, taken time.Time, tz string) {
	t.Helper()
	if a.Exif == nil {
		t.Errorf("%s: no exif", label)
		return
	}
	e := a.Exif
	if e.Description == nil || *e.Description != desc {
		t.Errorf("%s: description %v want %q", label, e.Description, desc)
	}
	if e.Rating == nil || *e.Rating != rating {
		t.Errorf("%s: rating %v want %d", label, e.Rating, rating)
	}
	if a.IsFavorite != fav {
		t.Errorf("%s: favorite %v want %v", label, a.IsFavorite, fav)
	}
	if a.Visibility != vis {
		t.Errorf("%s: visibility %s want %s", label, a.Visibility, vis)
	}
	if e.Latitude == nil || e.Longitude == nil || absf(*e.Latitude-lat) > 1e-4 || absf(*e.Longitude-lon) > 1e-4 {
		t.Errorf("%s: location %v,%v want %v,%v", label, e.Latitude, e.Longitude, lat, lon)
	}
	if e.DateTimeOriginal == nil || !e.DateTimeOriginal.Equal(taken) {
		t.Errorf("%s: dateTimeOriginal %v want %s", label, e.DateTimeOriginal, taken)
	}
	if tz != "" && (e.TimeZone == nil || *e.TimeZone != tz) {
		got := "<nil>"
		if e.TimeZone != nil {
			got = *e.TimeZone
		}
		t.Errorf("%s: timeZone %q want %s", label, got, tz)
	}
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestSyncAlbumInfo(t *testing.T) {
	s := newSyncer(t)
	p := photo(t, s.ua, "ai.jpg", t0)
	albA := s.mkAlbumA("desc0", p.ID)
	s.runUntilQuiet()
	albB := s.albumB()
	if b, err := s.ub.API.GetAlbum(s.ctx, albB); err != nil || b.Description != "desc0" {
		t.Fatalf("B album after initial sync: %+v err %v", b, err)
	}

	if _, err := s.ua.API.UpdateAlbum(s.ctx, albA, map[string]any{"albumName": s.name + "-ra", "description": "descA"}); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	b, err := s.ub.API.GetAlbum(s.ctx, albB)
	if err != nil || b.Name != s.name+"-ra" || b.Description != "descA" {
		t.Fatalf("A->B album info: %+v err %v", b, err)
	}

	if _, err := s.ub.API.UpdateAlbum(s.ctx, albB, map[string]any{"albumName": s.name + "-rb", "description": "descB"}); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	a, err := s.ua.API.GetAlbum(s.ctx, albA)
	if err != nil || a.Name != s.name+"-rb" || a.Description != "descB" {
		t.Fatalf("B->A album info: %+v err %v", a, err)
	}
}

func TestSyncIdempotent(t *testing.T) {
	s := newSyncer(t)
	p := photo(t, s.ua, "i1.jpg", t0)
	vid := upload(t, s.ua, "i.mp4", testutil.MP4(testutil.UniqueSeed()), testutil.UploadOpts{FileCreatedAt: t0.Add(time.Hour)})
	mov := upload(t, s.ua, "il.MOV", testutil.MOV(testutil.UniqueSeed()), testutil.UploadOpts{FileCreatedAt: t0.Add(2 * time.Hour), Visibility: "hidden"})
	live := upload(t, s.ua, "il.JPG", testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed(), DateTimeOriginal: ptr(t0.Add(2 * time.Hour)), OffsetTime: "+00:00"}),
		testutil.UploadOpts{FileCreatedAt: t0.Add(2 * time.Hour), LivePhotoVideoID: mov.ID})
	testutil.WaitExtracted(t, s.ua.API, live.ID)
	s.mustUpdate(s.ua.API, p.ID, map[string]any{"description": "idem", "rating": 3, "latitude": 1.25, "longitude": 2.5})
	s.mkAlbumA("idem", p.ID, vid.ID, live.ID)
	s.runUntilQuiet()
	for i := 0; i < 3; i++ {
		sum := s.run(fmt.Sprintf("extra %d", i))
		if sum.Mutations != 0 || sum.Pending != 0 {
			t.Fatalf("extra run %d: mutations=%d pending=%d", i, sum.Mutations, sum.Pending)
		}
	}
}

func TestSyncMassRemovalGuard(t *testing.T) {
	s := newSyncer(t)
	var ms []media
	base := t0
	for i := 0; i < 15; i++ {
		ms = append(ms, photoNoWait(t, s.ua, fmt.Sprintf("m%02d.jpg", i), base.Add(time.Duration(i)*time.Minute)))
	}
	s.mkAlbumA("", ids(ms...)...)
	s.runUntilQuiet()
	albB := s.albumB()
	if got := s.mustAlbumAssets(s.ub.API, albB); len(got) != 15 {
		t.Fatalf("B has %d after initial sync, want 15", len(got))
	}
	bBefore := s.sortedAll(s.ub.API)

	if _, err := s.ua.API.RemoveFromAlbum(s.ctx, s.albumA, ids(ms...)...); err != nil {
		t.Fatal(err)
	}
	sum, err := s.eng.RunPair(s.ctx, s.p())
	s.logSum("guarded run", sum)
	t.Logf("guarded run err: %v", err)
	if !sum.GuardTripped[engine.B] && !sum.GuardTripped[engine.A] {
		t.Errorf("guard not tripped: %+v", sum.GuardTripped)
	}
	if got := s.mustAlbumAssets(s.ub.API, albB); len(got) != 15 {
		t.Fatalf("B album has %d after guarded run, want 15 (nothing unlinked)", len(got))
	}

	ro, err := state.OpenReadOnly(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.PutMarker(s.pair.Name, "approve-removals"); err != nil {
		t.Fatal(err)
	}
	_ = ro.Close()
	sum2, err := s.eng.RunPair(s.ctx, s.p())
	s.logSum("approved run", sum2)
	if err != nil {
		t.Fatalf("approved run: %v", err)
	}
	if got := s.mustAlbumAssets(s.ub.API, albB); len(got) != 0 {
		t.Fatalf("B album has %d after approved run, want 0", len(got))
	}
	if after := s.sortedAll(s.ub.API); !reflect.DeepEqual(bBefore, after) {
		t.Errorf("B assets changed by unlinking (must never delete): before %d after %d", len(bBefore), len(after))
	}
	s.runUntilQuiet()
}

func TestSyncDryRunAndPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		dry  bool
	}{{"dry-run", true}, {"plan-only", false}} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := newSyncer(t, func(o *engine.Options) { o.DryRun = tc.dry })
			a1 := photo(t, s.ua, "d1.jpg", t0)
			a2 := photo(t, s.ua, "d2.jpg", t0.Add(time.Hour))
			b1 := photo(t, s.ub, "d3.jpg", t0.Add(2*time.Hour))
			s.mkAlbumA("", a1.ID, a2.ID)
			albB := s.mkAlbumB("", b1.ID)
			s.settle()
			snap := func() [5]any {
				return [5]any{s.sortedAll(s.ua.API), s.sortedAll(s.ub.API), s.mustAlbumAssets(s.ua.API, s.albumA),
					s.mustAlbumAssets(s.ub.API, albB), fmt.Sprint(s.mustAlb(s.ua.API, s.albumA), s.mustAlb(s.ub.API, albB))}
			}
			before := snap()
			if tc.dry {
				sum, err := s.eng.RunPair(s.ctx, s.p())
				s.logSum("dry run", sum)
				if err != nil {
					t.Fatalf("dry RunPair: %v", err)
				}
				if sum.Result != engine.ResultDryRun || sum.Mutations != 0 {
					t.Errorf("dry run result=%s mutations=%d", sum.Result, sum.Mutations)
				}
			}
			plan, sum, err := s.eng.PlanPair(s.ctx, s.p())
			s.logSum("plan", sum)
			if err != nil {
				t.Fatalf("PlanPair: %v", err)
			}
			if n := countCopies(plan); n < 3 {
				t.Errorf("plan has %d copies, want >= 3 (2 A->B, 1 B->A)", n)
			}
			if after := snap(); !reflect.DeepEqual(before, after) {
				t.Errorf("servers changed:\nbefore %v\nafter  %v", before, after)
			}
			if m, _ := filepath.Glob(filepath.Join(s.dir, "pairs", "*.json")); len(m) != 0 {
				t.Errorf("state files written: %v", m)
			}
		})
	}
}

func countCopies(p planner.Plan) int { return len(p.Copies) }

func (s *syncer) mustAlb(api *testutil.API, id string) testutil.Album {
	s.t.Helper()
	a, err := api.GetAlbum(s.ctx, id)
	if err != nil {
		s.t.Fatal(err)
	}
	a.UpdatedAt = time.Time{}
	return *a
}

func TestSyncSharedAlbumEditorUnlinks(t *testing.T) {
	s := newSyncer(t)
	x, err := testutil.NewUser(s.ctx, s.instA, "other")
	if err != nil {
		t.Fatal(err)
	}
	xa := photo(t, x, "x1.jpg", t0)
	id, err := x.API.CreateAlbum(s.ctx, s.name, "", xa.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.albumA = id
	if err := x.API.ShareAlbum(s.ctx, id, s.ua.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	albB := s.albumB()
	b, ok := s.albumSet(s.ub.API, albB)[xa.Sum]
	if !ok {
		t.Fatalf("other user's asset not copied to B")
	}
	if b.OwnerID != s.ub.ID {
		t.Errorf("B copy owner %s, want sync user %s", b.OwnerID, s.ub.ID)
	}

	// Removal on B is propagated by the editor (sync user) to the album owned by another user.
	if _, err := s.ub.API.RemoveFromAlbum(s.ctx, albB, b.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	if got := s.mustAlbumAssets(x.API, id); len(got) != 0 {
		t.Errorf("owner's album still has %v", got)
	}
	if a := s.get(x.API, xa.ID); a.IsTrashed {
		t.Errorf("other user's asset trashed")
	}
}

func TestSyncOwnerUnlinksOtherUsersAsset(t *testing.T) {
	s := newSyncer(t)
	x, err := testutil.NewUser(s.ctx, s.instA, "editor")
	if err != nil {
		t.Fatal(err)
	}
	seed := photo(t, s.ua, "own.jpg", t0)
	albA := s.mkAlbumA("", seed.ID)
	if err := s.ua.API.ShareAlbum(s.ctx, albA, x.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	xa := photo(t, x, "xe.jpg", t0.Add(time.Hour))
	if res, err := x.API.AddToAlbum(s.ctx, albA, xa.ID); err != nil || len(res) != 1 || !res[0].Success {
		t.Fatalf("editor add: %+v err %v", res, err)
	}
	s.runUntilQuiet()
	albB := s.albumB()
	b, ok := s.albumSet(s.ub.API, albB)[xa.Sum]
	if !ok {
		t.Fatalf("editor's asset not copied to B")
	}
	if _, err := s.ub.API.RemoveFromAlbum(s.ctx, albB, b.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	set := s.albumSet(s.ua.API, albA)
	if _, ok := set[xa.Sum]; ok {
		t.Errorf("other user's asset still in owner's album")
	}
	if _, ok := set[seed.Sum]; !ok {
		t.Errorf("own asset vanished")
	}
	if a := s.get(x.API, xa.ID); a.IsTrashed {
		t.Errorf("editor's asset trashed")
	}
}

func TestSyncPreflightMissingPermission(t *testing.T) {
	s := newSyncer(t)
	perms := engine.RequiredPermissions(s.opts)
	var short []string
	for _, p := range perms {
		if p != "albumAsset.delete" {
			short = append(short, p)
		}
	}
	key, err := s.ua.Key(s.ctx, short)
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := s.buildEngine(s.opts, key, s.keyB, t.TempDir())
	err = eng.Preflight(s.ctx)
	var pe *engine.PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight error = %v, want *PreflightError", err)
	}
	if pe.Side != engine.A || !contains(pe.Missing, "albumAsset.delete") {
		t.Errorf("PreflightError side=%v missing=%v", pe.Side, pe.Missing)
	}
	t.Logf("preflight error: %v", err)
}

// TestSyncServiceAccountBothSides: on each instance a main user owns the album and shares it with a dedicated
// sync (service account) user as editor. Assets are copied as the service account, metadata and album info flow
// both ways, and removals by the main users unlink the main users' own assets without trashing them.
func TestSyncServiceAccountBothSides(t *testing.T) {
	s := newSyncer(t)
	ma, err := testutil.NewUser(s.ctx, s.instA, "main-a")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := testutil.NewUser(s.ctx, s.instB, "main-b")
	if err != nil {
		t.Fatal(err)
	}
	pa := photo(t, ma, "main-a.jpg", t0)
	pb := photo(t, mb, "main-b.jpg", t0.Add(time.Hour))
	if s.albumA, err = ma.API.CreateAlbum(s.ctx, s.name, "", pa.ID); err != nil {
		t.Fatal(err)
	}
	if s.albumBID, err = mb.API.CreateAlbum(s.ctx, s.name, "", pb.ID); err != nil {
		t.Fatal(err)
	}
	if err := ma.API.ShareAlbum(s.ctx, s.albumA, s.ua.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := mb.API.ShareAlbum(s.ctx, s.albumBID, s.ub.ID, "editor"); err != nil {
		t.Fatal(err)
	}

	s.runUntilQuiet()
	// The main users see the copies in their albums; the copies belong to the service accounts.
	onB, ok := s.albumSet(mb.API, s.albumBID)[pa.Sum]
	if !ok {
		t.Fatalf("main A's asset not visible to main B in album B")
	}
	if onB.OwnerID != s.ub.ID || onB.OriginalFileName != "main-a.jpg" {
		t.Errorf("copy on B: owner %s name %s, want service account %s", onB.OwnerID, onB.OriginalFileName, s.ub.ID)
	}
	onA, ok := s.albumSet(ma.API, s.albumA)[pb.Sum]
	if !ok {
		t.Fatalf("main B's asset not visible to main A in album A")
	}
	if onA.OwnerID != s.ua.ID || onA.OriginalFileName != "main-b.jpg" {
		t.Errorf("copy on A: owner %s name %s, want service account %s", onA.OwnerID, onA.OriginalFileName, s.ua.ID)
	}

	// Metadata edited by the main users on their own assets reaches the service accounts' copies.
	s.mustUpdate(ma.API, pa.ID, map[string]any{"description": "from main A"})
	s.mustUpdate(mb.API, pb.ID, map[string]any{"description": "from main B"})
	// Album info edited by main A is written to main B's album by the editor service account.
	if _, err := ma.API.UpdateAlbum(s.ctx, s.albumA, map[string]any{"description": "album desc by main A"}); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	if d := descOf(s.get(s.ub.API, onB.ID)); d != "from main A" {
		t.Errorf("B copy description %q", d)
	}
	if d := descOf(s.get(s.ua.API, onA.ID)); d != "from main B" {
		t.Errorf("A copy description %q", d)
	}
	al, err := mb.API.GetAlbum(s.ctx, s.albumBID)
	if err != nil || al.Description != "album desc by main A" {
		t.Errorf("album B after info sync: %+v err %v", al, err)
	} else if !reflect.DeepEqual(albumRoles(al), map[string]string{mb.ID: "owner", s.ub.ID: "editor"}) {
		t.Errorf("album B members changed: %v", albumRoles(al))
	}

	// Main B removes the copy of main A's asset; main A removes the copy of main B's asset. The service accounts
	// unlink the main users' originals from the main users' albums, and nothing is trashed.
	if _, err := mb.API.RemoveFromAlbum(s.ctx, s.albumBID, onB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ma.API.RemoveFromAlbum(s.ctx, s.albumA, onA.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	if got := s.mustAlbumAssets(ma.API, s.albumA); len(got) != 0 {
		t.Errorf("album A still has %v", got)
	}
	if got := s.mustAlbumAssets(mb.API, s.albumBID); len(got) != 0 {
		t.Errorf("album B still has %v", got)
	}
	for _, c := range []struct {
		api *testutil.API
		id  string
	}{{ma.API, pa.ID}, {mb.API, pb.ID}, {s.ua.API, onA.ID}, {s.ub.API, onB.ID}} {
		if a := s.get(c.api, c.id); a.IsTrashed {
			t.Errorf("asset %s trashed", c.id)
		}
	}
}

// TestSyncServiceAccountRoleChanges: a service account that is only a viewer, or whose access was revoked, gets a
// configuration error and mutates nothing; once it is an editor again, sync resumes without unlinking anything.
func TestSyncServiceAccountRoleChanges(t *testing.T) {
	s := newSyncer(t)
	ma, err := testutil.NewUser(s.ctx, s.instA, "main-a")
	if err != nil {
		t.Fatal(err)
	}
	p1 := photo(t, ma, "role1.jpg", t0)
	if s.albumA, err = ma.API.CreateAlbum(s.ctx, s.name, "", p1.ID); err != nil {
		t.Fatal(err)
	}
	albB := s.albumB()

	expectConfigErr := func(label string) {
		t.Helper()
		sum, err := s.eng.RunPair(s.ctx, s.p())
		s.logSum(label, sum)
		if !errors.Is(err, engine.ErrConfig) {
			t.Fatalf("%s: RunPair error = %v, want ErrConfig", label, err)
		}
		if sum.Mutations != 0 {
			t.Errorf("%s: %d mutations, want 0", label, sum.Mutations)
		}
	}

	// Viewer: refused before anything is copied.
	if err := ma.API.ShareAlbum(s.ctx, s.albumA, s.ua.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	s.settle()
	expectConfigErr("viewer")
	if got := s.mustAlbumAssets(s.ub.API, albB); len(got) != 0 {
		t.Fatalf("viewer run copied %v to B", got)
	}

	// Promoted to editor: sync works.
	if err := ma.API.SetAlbumUserRole(s.ctx, s.albumA, s.ua.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	p2 := photo(t, ma, "role2.jpg", t0.Add(time.Hour))
	if _, err := ma.API.AddToAlbum(s.ctx, s.albumA, p2.ID); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	setB := s.albumSet(s.ub.API, albB)
	if _, ok := setB[p1.Sum]; !ok || len(setB) != 2 {
		t.Fatalf("B after editor sync: %v", setB)
	}
	wantB := s.mustAlbumAssets(s.ub.API, albB)

	// Demoted back to viewer while the main user removes an asset: no unlink on B.
	if err := ma.API.SetAlbumUserRole(s.ctx, s.albumA, s.ua.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := ma.API.RemoveFromAlbum(s.ctx, s.albumA, p2.ID); err != nil {
		t.Fatal(err)
	}
	expectConfigErr("demoted to viewer")

	// Access revoked entirely: the album looks gone to the service account, which must not unlink or recreate.
	if err := ma.API.RemoveAlbumUser(s.ctx, s.albumA, s.ua.ID); err != nil {
		t.Fatal(err)
	}
	expectConfigErr("revoked")
	if got := s.mustAlbumAssets(s.ub.API, albB); !reflect.DeepEqual(got, wantB) {
		t.Errorf("B changed while access was missing: %v, want %v", got, wantB)
	}
	if als, err := s.ua.API.ListAlbums(s.ctx); err != nil || len(als) != 0 {
		t.Errorf("service account albums on A after revoke: %+v err %v (want none created)", als, err)
	}

	// Re-shared as editor: the removal made meanwhile is propagated normally, nothing else is lost.
	if err := ma.API.ShareAlbum(s.ctx, s.albumA, s.ua.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	s.runUntilQuiet()
	setB = s.albumSet(s.ub.API, albB)
	if _, ok := setB[p1.Sum]; !ok || len(setB) != 1 {
		t.Errorf("B after re-share: %v, want only %s", setB, p1.Sum)
	}
	if a := s.get(ma.API, p2.ID); a.IsTrashed {
		t.Errorf("main user's removed asset trashed")
	}
}

func descOf(a *testutil.Asset) string {
	if a.Exif == nil || a.Exif.Description == nil {
		return ""
	}
	return *a.Exif.Description
}

// albumRoles maps user id -> role (v3 album responses carry the owner only in albumUsers).
func albumRoles(al *testutil.Album) map[string]string {
	m := make(map[string]string, len(al.Users))
	for _, u := range al.Users {
		m[u.User.ID] = u.Role
	}
	return m
}
