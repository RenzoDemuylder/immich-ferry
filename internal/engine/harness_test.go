package engine

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t              *testing.T
	fa, fb         *fakeimmich.Server
	ua, ub         string
	clA, clB       *immich.Client
	dir, tmp       string
	st             *state.Store
	clk            *fakeClock
	eng            *Engine
	opts           Options
	albumA, albumB string
	n              int
	keyA, keyB     string
}

func baseOptions() Options {
	return Options{Concurrency: 2, PropagateRemovals: true, MaxRemovalRatio: 0.2, MaxRemovalsMin: 10,
		ConflictWinner: A, SyncMetadata: true, SyncAlbumInfo: true, RestoreTrashed: true,
		PendingMetaTimeout: time.Hour, CopyPhaseBudget: time.Hour, CheckpointEvery: 100,
		CheckpointInterval: 30 * time.Second, BackoffBase: 10 * time.Minute, BackoffMax: 24 * time.Hour,
		BlockedRecheck: 24 * time.Hour}
}

func newEnv(t *testing.T, mods ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, clk: &fakeClock{t: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}}
	o := baseOptions()
	for _, m := range mods {
		m(&o)
	}
	o.Now = e.clk.Now
	e.tmp = t.TempDir()
	o.TmpDir = e.tmp
	e.opts = o
	e.fa, e.fb = fakeimmich.New(t), fakeimmich.New(t)
	perms := RequiredPermissions(o)
	e.ua, e.keyA = e.fa.AddUser("alice", perms...)
	e.ub, e.keyB = e.fb.AddUser("bob", perms...)
	e.clA = e.client(e.fa, e.keyA)
	e.clB = e.client(e.fb, e.keyB)
	e.albumA = e.fa.CreateAlbum(e.ua, "Trip")
	e.albumB = e.fb.CreateAlbum(e.ub, "Trip")
	e.dir = t.TempDir()
	st, err := state.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.eng = e.newEngine(o)
	return e
}

func (e *env) client(s *fakeimmich.Server, key string) *immich.Client {
	c, err := immich.New(immich.Config{BaseURL: s.URL(), APIKey: key, MaxRetries: 1,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) newEngine(o Options) *Engine {
	o.Now = e.clk.Now
	o.TmpDir = e.tmp
	eng, err := New(o, e.clA, e.clB, e.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	return eng
}

func (e *env) pair() Pair {
	return Pair{Name: "trip", A: AlbumRef{ID: e.albumA}, B: AlbumRef{ID: e.albumB}}
}

func (e *env) run() RunSummary {
	e.t.Helper()
	s, err := e.eng.RunPair(context.Background(), e.pair())
	if err != nil {
		e.t.Fatalf("RunPair: %v (summary %+v)", err, s)
	}
	return s
}

func (e *env) content(name string) []byte {
	return []byte("content-of-" + name + "-" + strings.Repeat("x", 200))
}

func (e *env) seed(srv *fakeimmich.Server, owner, album, name string, mods ...func(*fakeimmich.SeedOpts)) string {
	e.n++
	o := fakeimmich.SeedOpts{Owner: owner, Filename: name + ".jpg", Content: e.content(name),
		FileCreatedAt: time.Date(2024, 3, 1, 10, e.n, 0, 0, time.UTC)}
	for _, m := range mods {
		m(&o)
	}
	id := srv.SeedAsset(o)
	if album != "" {
		srv.AddToAlbum(album, id)
	}
	return id
}

func (e *env) seedA(name string, mods ...func(*fakeimmich.SeedOpts)) string {
	return e.seed(e.fa, e.ua, e.albumA, name, mods...)
}

func (e *env) seedB(name string, mods ...func(*fakeimmich.SeedOpts)) string {
	return e.seed(e.fb, e.ub, e.albumB, name, mods...)
}

// withExif gives a seeded asset extracted exif consistent with its fileCreatedAt.
func withExif(desc string) func(*fakeimmich.SeedOpts) {
	return func(o *fakeimmich.SeedOpts) {
		t := o.FileCreatedAt
		tz := "UTC+0"
		ex := &immich.Exif{DateTimeOriginal: &t, TimeZone: &tz}
		if desc != "" {
			ex.Description = &desc
		}
		o.Exif = ex
	}
}

func (e *env) findBy(srv *fakeimmich.Server, owner, name string) (fakeimmich.FakeAsset, bool) {
	for _, a := range srv.AssetsOf(owner) {
		if a.OriginalFileName == name+".jpg" {
			return a, true
		}
	}
	return fakeimmich.FakeAsset{}, false
}

func (e *env) onA(name string) fakeimmich.FakeAsset {
	e.t.Helper()
	a, ok := e.findBy(e.fa, e.ua, name)
	if !ok {
		e.t.Fatalf("asset %s not on A", name)
	}
	return a
}

func (e *env) onB(name string) fakeimmich.FakeAsset {
	e.t.Helper()
	a, ok := e.findBy(e.fb, e.ub, name)
	if !ok {
		e.t.Fatalf("asset %s not on B", name)
	}
	return a
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func (e *env) inAlbumA(id string) bool { return contains(e.fa.AlbumAssetIDs(e.albumA), id) }
func (e *env) inAlbumB(id string) bool { return contains(e.fb.AlbumAssetIDs(e.albumB), id) }

func (e *env) loadState() *state.PairState {
	e.t.Helper()
	ro, err := state.OpenReadOnly(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	ps, _, err := ro.Load("trip")
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

func (e *env) noTempFiles() {
	e.t.Helper()
	ents, _ := os.ReadDir(e.tmp)
	if len(ents) != 0 {
		e.t.Errorf("temp files left behind: %v", ents)
	}
}

func (e *env) countReq(srv *fakeimmich.Server, method, suffix string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

func (e *env) stateFileExists() bool {
	ents, _ := os.ReadDir(filepath.Join(e.dir, "pairs"))
	for _, x := range ents {
		if strings.HasSuffix(x.Name(), ".json") {
			return true
		}
	}
	return false
}

// settle runs until no copy/meta work remains: extracts every asset on both
// fakes and runs until Pending == 0.
func (e *env) settle() RunSummary {
	e.t.Helper()
	var s RunSummary
	for i := 0; i < 8; i++ {
		e.extractAll()
		s = e.run()
		if s.Pending == 0 {
			return s
		}
	}
	e.t.Fatalf("pending never reached 0: %+v", s)
	return s
}

func (e *env) extractAll() {
	for _, a := range e.fa.AssetsOf(e.ua) {
		e.fa.Extract(a.ID)
	}
	for _, a := range e.fb.AssetsOf(e.ub) {
		e.fb.Extract(a.ID)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var _ = errors.New
var _ = fmt.Sprint

func sha1b64Of(b []byte) string {
	s := sha1.Sum(b)
	return sha1b64(s[:])
}

func sortStrings(s []string) { sort.Strings(s) }

func (e *env) checksumA(id string) string {
	a, _ := e.fa.Asset(id)
	return a.Checksum
}
