package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

type hookImmich struct {
	Immich
	onGet      func(id string)
	onUpload   func(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error)
	onDownload func(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error)
}

func (h *hookImmich) GetAsset(ctx context.Context, id string) (immich.Asset, error) {
	if h.onGet != nil {
		h.onGet(id)
	}
	return h.Immich.GetAsset(ctx, id)
}

func (h *hookImmich) Upload(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
	if h.onUpload != nil {
		return h.onUpload(ctx, r, onBytes)
	}
	return h.Immich.Upload(ctx, r, onBytes)
}

func (h *hookImmich) DownloadOriginal(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
	if h.onDownload != nil {
		return h.onDownload(ctx, id, w, onBytes)
	}
	return h.Immich.DownloadOriginal(ctx, id, w, onBytes)
}

func (e *env) engineWith(a, b Immich) *Engine {
	o := e.opts
	eng, err := New(o, a, b, e.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	return eng
}

// 1. Album info: a transient failure writing the rename to B is lost forever.
func TestRegressionAlbumInfoRenameNotLostOnFailedPatch(t *testing.T) {
	e := newEnv(t)
	e.seedA("x")
	e.run()
	e.run()
	ctx := context.Background()
	if _, err := e.clA.UpdateAlbumInfo(ctx, e.albumA, immich.AlbumInfoUpdate{Name: immich.Some("Renamed A")}); err != nil {
		t.Fatal(err)
	}
	e.fb.Inject(fakeimmich.Fault{Method: "PATCH", PathPrefix: "/albums/", Status: 500, Times: 2})
	s := e.run()
	t.Logf("run1 result=%s itemErrors=%d", s.Result, s.ItemErrorCount)
	for i := 0; i < 3; i++ {
		s = e.run()
	}
	al, _ := e.fb.Album(e.albumB)
	ala, _ := e.fa.Album(e.albumA)
	if al.Name != "Renamed A" {
		t.Fatalf("BUG: rename lost: A=%q B=%q; last run %+v", ala.Name, al.Name, s.AlbumInfoUpdated)
	}
}

// 2. Metadata: partial failure (one side's PUT fails) loses the other side's change.
func TestRegressionMetaPartialFailureKeepsOtherSideChange(t *testing.T) {
	e, a, b := pairedWithExif(t)
	setDesc(e.fa, a, "from-a")
	e.fb.Edit(b, func(x *fakeimmich.FakeAsset) { r := 4; x.Exif.Rating = &r })
	e.fb.Inject(fakeimmich.Fault{Method: "PUT", PathPrefix: "/assets/" + b, Status: 500, Times: 2})
	s := e.run()
	t.Logf("run1 %+v errs=%d", s.MetaUpdates, s.ItemErrorCount)
	for i := 0; i < 3; i++ {
		e.run()
	}
	da, db := derefS(e.onA("x").Exif.Description), derefS(e.onB("x").Exif.Description)
	if db != "from-a" {
		t.Fatalf("BUG: description edit on A lost: A=%q B=%q", da, db)
	}
}

// 3. hiddenElsewhere fails open: transient upload-check error => wrong unlink.
func TestRegressionHiddenElsewhereUnknownDoesNotUnlink(t *testing.T) {
	e := newEnv(t)
	still := e.seedA("still")
	vid := e.seedA("vid", func(o *fakeimmich.SeedOpts) { o.Type = "VIDEO"; o.Filename = "vid.mp4" })
	e.fa.SetContentID(still, "c1")
	e.fa.SetContentID(vid, "c1")
	e.run()
	sb := e.onB("still")
	vb, _ := e.findByFile(e.fb, e.ub, "vid.mp4")
	e.fb.SetContentID(sb.ID, "c1")
	e.fb.SetContentID(vb.ID, "c1")
	e.fb.Extract(sb.ID)
	e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets/bulk-upload-check", Status: 503, Times: 2})
	s := e.run()
	if !e.inAlbumA(vid) {
		t.Fatalf("BUG: video wrongly unlinked from A after transient upload-check error: %+v", s.Unlinked)
	}
}

// 4. Read-back baseline absorbs a concurrent edit of a non-written field.
func TestRegressionReadBackDoesNotAbsorbConcurrentEdit(t *testing.T) {
	e, a, b := pairedWithExif(t)
	setDesc(e.fa, a, "from-a")
	var once sync.Once
	hb := &hookImmich{Immich: e.clB, onGet: func(id string) {
		if id == b {
			once.Do(func() { e.fb.Edit(b, func(x *fakeimmich.FakeAsset) { r := 5; x.Exif.Rating = &r }) })
		}
	}}
	e.eng = e.engineWith(e.clA, hb)
	e.run()
	e.eng = e.engineWith(e.clA, e.clB)
	for i := 0; i < 3; i++ {
		e.run()
	}
	ra := e.onA("x").Exif.Rating
	if ra == nil || *ra != 5 {
		t.Fatalf("BUG: B rating edit made during cycle never reached A (A rating=%v)", ra)
	}
}

// 5. Upload retried after server stored it => duplicate => treated as reused, no Pending.
func TestRegressionUploadRetryDuplicateIsOurs(t *testing.T) {
	for _, dup := range []bool{false, true} {
		e := newEnv(t)
		a := e.seedA("x", withExif("orig"))
		edited := time.Date(2020, 5, 5, 5, 5, 0, 0, time.UTC)
		e.fa.Edit(a, func(x *fakeimmich.FakeAsset) { x.Exif.DateTimeOriginal = &edited })
		hb := &hookImmich{Immich: e.clB}
		if dup {
			hb.onUpload = func(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
				if _, err := e.clB.Upload(ctx, r, onBytes); err != nil {
					return immich.UploadResult{}, err
				}
				return e.clB.Upload(ctx, r, onBytes) // client-level retry after lost response
			}
		}
		e.eng = e.engineWith(e.clA, hb)
		s := e.run()
		t.Logf("dup=%v pending=%d", dup, s.Pending)
		e.eng = e.engineWith(e.clA, e.clB)
		for i := 0; i < 4; i++ {
			e.extractAll()
			e.run()
		}
		bd := e.onB("x").Exif.DateTimeOriginal
		if bd == nil || !bd.Equal(edited) {
			t.Errorf("dup=%v: BUG: B date %v, want source-edited %v", dup, bd, edited)
		}
	}
}

// 6. Unknown file size bypasses the tmp byte budget entirely.
func TestRegressionUnknownSizeReservesBudget(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TmpBudgetBytes = 300; o.Concurrency = 4 })
	for i := 0; i < 8; i++ {
		e.seedA(string(rune('a' + i)))
	}
	var inflight, maxIn atomic.Int64
	ha := &hookImmich{Immich: e.clA, onDownload: func(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
		n := inflight.Add(1)
		for {
			m := maxIn.Load()
			if n <= m || maxIn.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		defer inflight.Add(-1)
		return e.clA.DownloadOriginal(ctx, id, w, onBytes)
	}}
	e.eng = e.engineWith(ha, e.clB)
	e.run()
	if maxIn.Load() > 1 {
		t.Fatalf("BUG: %d concurrent downloads with a 300-byte budget and ~212-byte files (size unknown => no reservation)", maxIn.Load())
	}
}

type checkHook struct {
	Immich
	onCheck func()
}

func (h *checkHook) UploadCheck(ctx context.Context, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error) {
	if h.onCheck != nil {
		h.onCheck()
	}
	return h.Immich.UploadCheck(ctx, items)
}

// 7. Crash after write-ahead: stale Intent makes a reused pre-existing duplicate
// look like our upload => Pending => source-wins overwrites target's own metadata.
func TestRegressionStaleIntentDoesNotOverwriteReusedDuplicate(t *testing.T) {
	e := newEnv(t)
	e.seedA("x", withExif("A-desc"))
	bid := e.seed(e.fb, e.ub, "", "x", withExif("B-own")) // pre-existing on B, not in album
	path := e.dir + "/pairs/trip.json"
	var saved []byte
	var once sync.Once
	hb := &checkHook{Immich: e.clB, onCheck: func() {
		once.Do(func() {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			saved = b
		})
	}}
	e.eng = e.engineWith(e.clA, hb)
	e.run()
	// simulate SIGKILL right after the write-ahead save (before album add / final save)
	if err := os.WriteFile(path, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	e.fb.RemoveFromAlbum(e.albumB, bid)
	e.eng = e.engineWith(e.clA, e.clB)
	for i := 0; i < 4; i++ {
		e.extractAll()
		e.run()
	}
	if d := derefS(e.onB("x").Exif.Description); d != "B-own" {
		t.Fatalf("BUG: pre-existing B asset's own description overwritten: %q", d)
	}
}

// 8. Hash failure of an external asset hides the key on that side; the other
// side's copy of the same content is then uploaded as a duplicate.
func TestRegressionHashFailureSkipsCopyIntoSide(t *testing.T) {
	e := newEnv(t)
	ext := e.seedA("ext", func(o *fakeimmich.SeedOpts) { o.LibraryID = "lib" })
	e.seedB("ext")
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + ext + "/original", Status: 500, Times: 3})
	s := e.run()
	n := len(e.fa.AlbumAssetIDs(e.albumA))
	t.Logf("copied BtoA=%d album A size=%d", s.Copied.BtoA, n)
	if n != 1 {
		t.Fatalf("BUG: album A now holds %d assets for one photo", n)
	}
}

func withSize(n int64) func(*fakeimmich.SeedOpts) {
	return func(o *fakeimmich.SeedOpts) {
		o.Exif = &immich.Exif{FileSizeInByte: &n}
	}
}

// Known size above the temp budget: fail fast without downloading, 30 day backoff.
func TestRegressionTooLargeForBudgetFailsFast(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TmpBudgetBytes = 100 })
	e.seedA("big", withSize(1000))
	s := e.run()
	if s.Skipped[SkipTooLarge] != 1 {
		t.Fatalf("%+v", s)
	}
	if n := e.countReq(e.fa, "GET", "/original"); n != 0 {
		t.Fatalf("downloaded %d times despite known oversize", n)
	}
	for _, it := range e.loadState().Items {
		if it.Failure == nil || !it.Failure.Next.Equal(e.clk.Now().Add(tooLargeBackoff)) {
			t.Fatalf("%+v", it.Failure)
		}
	}
	e.noTempFiles()
}

// Unknown size: the download is aborted once it exceeds the budget.
func TestRegressionDownloadExceedingBudgetAborts(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TmpBudgetBytes = 100 })
	e.seedA("x")
	s := e.run()
	if s.Skipped[SkipTooLarge] != 1 || s.Copied.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if len(e.fb.AssetsOf(e.ub)) != 0 {
		t.Fatal("asset uploaded despite budget")
	}
	for _, it := range e.loadState().Items {
		if it.Failure == nil || !it.Failure.Next.Equal(e.clk.Now().Add(tooLargeBackoff)) {
			t.Fatalf("%+v", it.Failure)
		}
	}
	e.noTempFiles()
}

// Leftover temp files of ours are removed on engine start; foreign files stay.
func TestRegressionLeftoverTempFilesCleaned(t *testing.T) {
	e := newEnv(t)
	ours := e.tmp + "/immich-ferry-123.tmp"
	other := e.tmp + "/keep.txt"
	for _, p := range []string{ours, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.eng = e.newEngine(e.opts)
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("own leftover temp file not removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("foreign file removed")
	}
}

// Two pairs resolving to the same album are a config error.
func TestRegressionAlbumCollisionAcrossPairs(t *testing.T) {
	e := newEnv(t)
	e.seedA("x")
	e.run()
	p2 := Pair{Name: "other", A: AlbumRef{ID: e.albumA}, B: AlbumRef{ID: e.albumB}}
	_, err := e.eng.RunPair(context.Background(), p2)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "trip") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("want ErrConfig naming both pairs, got %v", err)
	}
}

// Dry run / plan never moves a corrupt state file aside.
func TestRegressionDryRunLeavesCorruptStateAlone(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.DryRun = true })
	e.seedA("x")
	path := e.dir + "/pairs/trip.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()
	if _, _, err := e.eng.PlanPair(context.Background(), e.pair()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{not json" {
		t.Fatalf("corrupt state file was touched: %v %q", err, b)
	}
}

// External-library source: the download is verified against the content key.
func TestRegressionExternalSourceDownloadVerified(t *testing.T) {
	e := newEnv(t)
	e.seedA("ext", func(o *fakeimmich.SeedOpts) { o.LibraryID = "lib" })
	var n atomic.Int64
	ha := &hookImmich{Immich: e.clA, onDownload: func(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
		if n.Add(1) == 1 { // first download is the hash pass
			return e.clA.DownloadOriginal(ctx, id, w, onBytes)
		}
		k, err := w.Write([]byte("corrupted content"))
		return int64(k), err
	}}
	e.eng = e.engineWith(ha, e.clB)
	s := e.run()
	if s.Copied.AtoB != 0 || len(e.fb.AssetsOf(e.ub)) != 0 {
		t.Fatalf("corrupt external download was uploaded: %+v", s)
	}
}
