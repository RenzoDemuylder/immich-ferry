package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// Tests for the rarely executed paths of copy.go: copy-phase budget expiry,
// consecutive transient abort, album add batching, flush on cancellation,
// upload check failures and copyExisting's restore paths.

// cpHook wraps an Immich client; a nil hook passes the call through.
type cpHook struct {
	Immich
	onCheck    func(ctx context.Context, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error)
	onUpload   func(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error)
	onDownload func(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error)
	onAdd      func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error)
	onGet      func(ctx context.Context, id string) (immich.Asset, error)
	onRestore  func(ctx context.Context, ids []string) error
}

func (h *cpHook) UploadCheck(ctx context.Context, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error) {
	if h.onCheck != nil {
		return h.onCheck(ctx, items)
	}
	return h.Immich.UploadCheck(ctx, items)
}

func (h *cpHook) Upload(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
	if h.onUpload != nil {
		return h.onUpload(ctx, r, onBytes)
	}
	return h.Immich.Upload(ctx, r, onBytes)
}

func (h *cpHook) DownloadOriginal(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
	if h.onDownload != nil {
		return h.onDownload(ctx, id, w, onBytes)
	}
	return h.Immich.DownloadOriginal(ctx, id, w, onBytes)
}

func (h *cpHook) AddToAlbum(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
	if h.onAdd != nil {
		return h.onAdd(ctx, albumID, ids)
	}
	return h.Immich.AddToAlbum(ctx, albumID, ids)
}

func (h *cpHook) GetAsset(ctx context.Context, id string) (immich.Asset, error) {
	if h.onGet != nil {
		return h.onGet(ctx, id)
	}
	return h.Immich.GetAsset(ctx, id)
}

func (h *cpHook) RestoreFromTrash(ctx context.Context, ids []string) error {
	if h.onRestore != nil {
		return h.onRestore(ctx, ids)
	}
	return h.Immich.RestoreFromTrash(ctx, ids)
}

func cpTransient(msg string) error {
	return immich.WithKind(immich.KindTransient, errors.New(msg))
}

// cpSeedMany seeds n assets on A (album A) and returns their keys.
func (e *env) cpSeedMany(prefix string, n int) []string {
	keys := make([]string, n)
	for i := range n {
		keys[i] = e.checksumA(e.seedA(fmt.Sprintf("%s%03d", prefix, i)))
	}
	return keys
}

// cpUploadsB counts upload requests (POST .../assets, not .../trash/restore/assets) to B.
func (e *env) cpUploadsB() int {
	n := 0
	for _, r := range e.fb.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/assets") && !strings.HasSuffix(r.Path, "/restore/assets") {
			n++
		}
	}
	return n
}

func cpItemErrors(s RunSummary, op string) []ItemError {
	var out []ItemError
	for _, ie := range s.ItemErrors {
		if ie.Op == op {
			out = append(out, ie)
		}
	}
	return out
}

// 1. CopyPhaseBudget expires mid-phase: the rest of the batch is deferred in
// the workers (deferTask), later batches by the copy-phase loop; deferred keys
// keep their previous Intent, nothing is half-done, and the next run copies them.
func TestCopyPhaseBudgetExpiryDefersRest(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 1; o.CopyPhaseBudget = time.Minute })
	const n = copyBatchSize + 2 // two batches: the second is deferred by copyPhase itself
	keys := e.cpSeedMany("p", n)

	// Plant a previous write-ahead Intent for every key: a deferred task must
	// leave it exactly as it was.
	started := e.clk.Now().Add(-time.Hour)
	ps := state.NewPairState("trip")
	for _, k := range keys {
		ps.Item(k).Intent = &state.Intent{To: uint8(B), Started: started, Source: state.AssetRef{ID: "prev-" + k, Checksum: k}, UploadStarted: true}
	}
	if err := e.st.Save("trip", ps); err != nil {
		t.Fatal(err)
	}

	var uploads atomic.Int32
	hb := &cpHook{Immich: e.clB}
	hb.onUpload = func(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
		res, err := e.clB.Upload(ctx, r, onBytes)
		if uploads.Add(1) == 1 {
			e.clk.Advance(2 * time.Minute) // the budget elapses during the first copy
		}
		return res, err
	}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	if uploads.Load() != 1 || s.Copied.AtoB != 1 || s.Deferred != n-1 || s.ItemErrorCount != 0 || s.Result != ResultPartial {
		t.Fatalf("uploads=%d summary=%+v", uploads.Load(), s)
	}
	st := e.loadState()
	copied := 0
	for _, k := range keys {
		it := st.Items[k]
		if it == nil {
			t.Fatalf("no state item for %s", k)
		}
		if it.InBoth {
			copied++
			if it.Intent != nil {
				t.Errorf("copied key %s kept its intent", k)
			}
			if !e.inAlbumB(it.Refs[B][0].ID) {
				t.Errorf("copied key %s not in album B", k)
			}
			continue
		}
		in := it.Intent
		if in == nil || in.To != uint8(B) || !in.UploadStarted || !in.Started.Equal(started) || in.Source.ID != "prev-"+k {
			t.Errorf("deferred key %s: intent not restored: %+v", k, in)
		}
		if it.Failure != nil || it.Blocked != nil || it.Pending != nil || it.Refs[B] != nil {
			t.Errorf("deferred key %s half-done: %+v", k, it)
		}
	}
	if copied != 1 {
		t.Fatalf("%d keys InBoth, want 1", copied)
	}
	if got := len(e.fb.AlbumAssetIDs(e.albumB)); got != 1 || e.cpUploadsB() != 1 {
		t.Fatalf("album B has %d assets, %d uploads", got, e.cpUploadsB())
	}
	e.noTempFiles()

	e.eng = e.engineWith(e.clA, e.clB)
	s = e.run()
	if s.Copied.AtoB != n-1 || s.Deferred != 0 || s.ItemErrorCount != 0 {
		t.Fatalf("second run: %+v", s)
	}
	if got := len(e.fb.AlbumAssetIDs(e.albumB)); got != n || e.cpUploadsB() != n {
		t.Fatalf("album B has %d assets, %d uploads; want %d", got, e.cpUploadsB(), n)
	}
}

// 2. maxConsecTransient consecutive transient failures abort the copy phase:
// the remaining tasks are deferred (not attempted), the failures are item
// errors, and a later run without faults copies everything.
func TestCopyConsecutiveTransientAborts(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 1 })
	const n = maxConsecTransient + 2
	e.cpSeedMany("t", n)

	var downloads atomic.Int32
	ha := &cpHook{Immich: e.clA}
	ha.onDownload = func(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
		downloads.Add(1)
		return 0, cpTransient("boom")
	}
	e.eng = e.engineWith(ha, e.clB)
	s := e.run()
	if s.ItemErrorCount != maxConsecTransient || len(cpItemErrors(s, "download")) != maxConsecTransient ||
		s.Deferred != n-maxConsecTransient || s.Copied.AtoB != 0 || s.Result != ResultPartial {
		t.Fatalf("%+v", s)
	}
	if got := downloads.Load(); got != int32(maxConsecTransient*downloadAttempts) {
		t.Fatalf("%d download calls, want %d (deferred tasks must not be attempted)", got, maxConsecTransient*downloadAttempts)
	}
	failed := 0
	for _, it := range e.loadState().Items {
		if it.Failure != nil {
			failed++
			if it.Failure.Op != copyKeyPlaceholderOp || it.Intent != nil {
				t.Errorf("failure %+v intent %+v", it.Failure, it.Intent)
			}
		}
	}
	if failed != maxConsecTransient || e.cpUploadsB() != 0 {
		t.Fatalf("%d failures recorded, %d uploads", failed, e.cpUploadsB())
	}
	e.noTempFiles()

	e.clk.Advance(time.Hour) // past the failure backoff
	e.eng = e.engineWith(e.clA, e.clB)
	s = e.run()
	if s.Copied.AtoB != n || s.ItemErrorCount != 0 || s.Deferred != 0 || s.Result != ResultOK {
		t.Fatalf("second run: %+v", s)
	}
	for _, it := range e.loadState().Items {
		if it.Failure != nil {
			t.Errorf("failure kept after success: %+v", it.Failure)
		}
	}
}

// 3. More than albumFlushThreshold copies: adds are flushed from queueAdd once
// the queue reaches the threshold, in several PUT requests; all copies end up
// in the album.
func TestCopyAlbumAddBatching(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 4 })
	const n = albumFlushThreshold + 1
	e.cpSeedMany("b", n)

	var mu sync.Mutex
	var sizes []int
	fromQueue := false
	hb := &cpHook{Immich: e.clB}
	hb.onAdd = func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
		buf := make([]byte, 64<<10)
		stack := string(buf[:runtime.Stack(buf, false)])
		mu.Lock()
		sizes = append(sizes, len(ids))
		if len(ids) >= albumFlushThreshold && strings.Contains(stack, ").queueAdd(") {
			fromQueue = true
		}
		mu.Unlock()
		return e.clB.AddToAlbum(ctx, albumID, ids)
	}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	if s.Copied.AtoB != n || s.ItemErrorCount != 0 || s.Result != ResultOK {
		t.Fatalf("%+v", s)
	}
	if got := len(e.fb.AlbumAssetIDs(e.albumB)); got != n {
		t.Fatalf("album B has %d assets, want %d", got, n)
	}
	puts := e.countReq(e.fb, "PUT", "/albums/"+e.albumB+"/assets")
	mu.Lock()
	defer mu.Unlock()
	if puts < 2 || len(sizes) != puts {
		t.Fatalf("%d PUT requests, sizes %v", puts, sizes)
	}
	if !fromQueue {
		t.Fatalf("no threshold flush from queueAdd; batch sizes %v", sizes)
	}
	for _, it := range e.loadState().Items {
		if !it.InBoth || it.Pending == nil {
			t.Fatalf("item not completed: %+v", it)
		}
	}
}

// 4. The run is cancelled after an upload reached the server but before its
// album add: flushAdds uses a fresh context, so the upload still ends up in
// the album and is recorded; cancellation is no item failure; the next run
// does not re-upload it.
func TestCopyFlushOnCancellation(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 1 })
	keys := e.cpSeedMany("c", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var uploads atomic.Int32
	hb := &cpHook{Immich: e.clB}
	hb.onUpload = func(uctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
		res, err := e.clB.Upload(uctx, r, onBytes)
		uploads.Add(1)
		cancel() // the upload is on the server; the album add has not happened yet
		return res, err
	}
	e.eng = e.engineWith(e.clA, hb)
	s, err := e.eng.RunPair(ctx, e.pair())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation error, got %v", err)
	}
	if uploads.Load() != 1 || s.Copied.AtoB != 1 || s.ItemErrorCount != 0 || s.Deferred != 1 {
		t.Fatalf("uploads=%d summary=%+v", uploads.Load(), s)
	}
	st := e.loadState()
	var done, rest *state.Item
	for _, k := range keys {
		if it := st.Items[k]; it != nil && it.InBoth {
			done = it
		} else {
			rest = it
		}
	}
	if done == nil {
		t.Fatal("uploaded key not recorded as InBoth")
	}
	if done.Intent != nil || done.Failure != nil || done.Pending == nil || !e.inAlbumB(done.Refs[B][0].ID) {
		t.Fatalf("uploaded item: %+v", done)
	}
	if rest != nil && (rest.Failure != nil || rest.Intent != nil || rest.Blocked != nil) {
		t.Fatalf("deferred item recorded a failure: %+v", rest)
	}
	e.noTempFiles()

	e.eng = e.engineWith(e.clA, e.clB)
	s = e.run()
	if s.Copied.AtoB != 1 || s.ItemErrorCount != 0 || e.cpUploadsB() != 2 {
		t.Fatalf("second run: uploads=%d summary=%+v", e.cpUploadsB(), s)
	}
	if got := len(e.fb.AlbumAssetIDs(e.albumB)); got != 2 || len(e.fb.AssetsOf(e.ub)) != 2 {
		t.Fatalf("album B %d, assets on B %d", got, len(e.fb.AssetsOf(e.ub)))
	}
}

// 5a. The bulk upload check request fails: every task of the batch fails with
// op "uploadcheck" and is retried (and copied) by a later run.
func TestCopyUploadCheckRequestFails(t *testing.T) {
	e := newEnv(t)
	keys := e.cpSeedMany("u", 2)
	hb := &cpHook{Immich: e.clB}
	hb.onCheck = func(context.Context, []immich.UploadCheckItem) ([]immich.UploadCheckResult, error) {
		return nil, cpTransient("check down")
	}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	ies := cpItemErrors(s, "uploadcheck")
	if s.ItemErrorCount != 2 || len(ies) != 2 || s.Copied.AtoB != 0 || e.cpUploadsB() != 0 {
		t.Fatalf("%+v", s)
	}
	for _, ie := range ies {
		if ie.Side != B || ie.Kind != immich.KindTransient {
			t.Errorf("item error %+v", ie)
		}
	}
	st := e.loadState()
	for _, k := range keys {
		if it := st.Items[k]; it == nil || it.Failure == nil || it.Intent != nil || it.InBoth {
			t.Fatalf("key %s: %+v", k, it)
		}
	}

	e.clk.Advance(time.Hour)
	e.eng = e.engineWith(e.clA, e.clB)
	if s = e.run(); s.Copied.AtoB != 2 || s.ItemErrorCount != 0 {
		t.Fatalf("second run: %+v", s)
	}
}

// 5b. The upload check returns no result for one key: only that task fails
// ("upload check returned no result"); the others are copied.
func TestCopyUploadCheckMissingResult(t *testing.T) {
	e := newEnv(t)
	keys := e.cpSeedMany("m", 3)
	drop := keys[1]
	hb := &cpHook{Immich: e.clB}
	hb.onCheck = func(ctx context.Context, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error) {
		res, err := e.clB.UploadCheck(ctx, items)
		out := res[:0:0]
		for _, x := range res {
			if x.ID != drop {
				out = append(out, x)
			}
		}
		return out, err
	}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	ies := cpItemErrors(s, "uploadcheck")
	if s.ItemErrorCount != 1 || len(ies) != 1 || ies[0].Key != drop || !strings.Contains(ies[0].Msg, "upload check returned no result") ||
		s.Copied.AtoB != 2 || e.cpUploadsB() != 2 {
		t.Fatalf("%+v", s)
	}
	st := e.loadState()
	for _, k := range keys {
		it := st.Items[k]
		if k == drop {
			if it == nil || it.Failure == nil || it.InBoth {
				t.Fatalf("dropped key: %+v", it)
			}
		} else if it == nil || !it.InBoth {
			t.Fatalf("key %s not copied: %+v", k, it)
		}
	}
}

// 6. copyExisting with a target duplicate: get_existing failure and the
// restore paths for a trashed duplicate (RestoreTrashed on, not bootstrap).
func TestCopyExistingRestorePaths(t *testing.T) {
	cases := []struct {
		name        string
		trashed     bool
		hook        func(h *cpHook, bid string)
		wantOp      string // item error op; "" = blocked target_trashed
		wantTrashed bool   // B duplicate still trashed after the run
	}{
		{"get_existing_fails", false, func(h *cpHook, bid string) {
			h.onGet = func(ctx context.Context, id string) (immich.Asset, error) {
				if id == bid {
					return immich.Asset{}, cpTransient("get down")
				}
				return h.Immich.GetAsset(ctx, id)
			}
		}, "get_existing", false},
		{"restore_fails", true, func(h *cpHook, _ string) {
			h.onRestore = func(context.Context, []string) error { return cpTransient("restore down") }
		}, "restore", true},
		{"reread_after_restore_fails", true, func(h *cpHook, bid string) {
			var n atomic.Int32
			h.onGet = func(ctx context.Context, id string) (immich.Asset, error) {
				if id == bid && n.Add(1) > 1 {
					return immich.Asset{}, cpTransient("get down")
				}
				return h.Immich.GetAsset(ctx, id)
			}
		}, "restore", false}, // the restore itself took effect on the server
		{"restore_leaves_trashed", true, func(h *cpHook, _ string) {
			h.onRestore = func(context.Context, []string) error { return nil } // "success", nothing restored
		}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.warmup() // clears bootstrap: restores are allowed
			key := e.checksumA(e.seedA("x"))
			bid := e.seed(e.fb, e.ub, "", "x")
			if c.trashed {
				e.fb.Trash(bid)
			}
			uploadsBefore := e.cpUploadsB()
			hb := &cpHook{Immich: e.clB}
			c.hook(hb, bid)
			e.eng = e.engineWith(e.clA, hb)
			s := e.run()
			if s.Copied.AtoB != 0 || s.Restored.AtoB != 0 || s.Linked.AtoB != 0 || e.cpUploadsB() != uploadsBefore {
				t.Fatalf("%+v", s)
			}
			b, _ := e.fb.Asset(bid)
			if e.inAlbumB(bid) || b.IsTrashed != c.wantTrashed || len(e.fb.AssetsOf(e.ub)) != 2 {
				t.Fatalf("B duplicate: inAlbum=%v %+v", e.inAlbumB(bid), b)
			}
			it := e.stateItem(key)
			if it.InBoth || it.Intent != nil || it.Pending != nil || it.Ours[B] != "" {
				t.Fatalf("state: %+v", it)
			}
			if c.wantOp != "" {
				ies := cpItemErrors(s, c.wantOp)
				if s.ItemErrorCount != 1 || len(ies) != 1 || ies[0].Key != key || ies[0].Side != B {
					t.Fatalf("%+v", s)
				}
				if it.Failure == nil || it.Blocked != nil {
					t.Fatalf("state: %+v", it)
				}
				// A later run without faults reuses (and restores) the duplicate.
				e.clk.Advance(time.Hour)
				e.eng = e.engineWith(e.clA, e.clB)
				s = e.run()
				b, _ = e.fb.Asset(bid)
				if s.Copied.AtoB != 1 || s.ItemErrorCount != 0 || b.IsTrashed || !e.inAlbumB(bid) || e.cpUploadsB() != uploadsBefore {
					t.Fatalf("retry: %+v asset %+v", s, b)
				}
				return
			}
			if s.ItemErrorCount != 0 || s.Skipped[SkipTargetTrash] != 1 {
				t.Fatalf("%+v", s)
			}
			if it.Blocked == nil || it.Blocked.Reason != string(SkipTargetTrash) || it.Blocked.Side != uint8(B) || it.Failure != nil {
				t.Fatalf("state: %+v blocked %+v", it, it.Blocked)
			}
		})
	}
}

// copyNew: the run is cancelled after ensureMotion succeeded (existing hidden
// motion on B) and before the byte budget is acquired: budget.acquire fails
// with the cancellation, which is neither an item error nor a recorded
// failure; the next run copies the still, linked to the existing motion.
func TestCopyBudgetAcquireCanceled(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 1 })
	e.seedLiveA()
	mb := e.seedMotionB(immich.VisibilityHidden)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := &cpHook{Immich: e.clB}
	hb.onGet = func(gctx context.Context, id string) (immich.Asset, error) {
		a, err := e.clB.GetAsset(gctx, id)
		if id == mb {
			cancel() // useExistingMotion returns mb; acquire is next
		}
		return a, err
	}
	e.eng = e.engineWith(e.clA, hb)
	s, err := e.eng.RunPair(ctx, e.pair())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if s.ItemErrorCount != 0 || s.Copied.AtoB != 0 || e.cpUploadsB() != 0 {
		t.Fatalf("%+v", s)
	}
	key := e.checksumA(e.onA("still").ID)
	if it := e.loadState().Items[key]; it != nil && (it.Failure != nil || it.Intent != nil || it.InBoth) {
		t.Fatalf("state: %+v", it)
	}
	e.noTempFiles()

	e.eng = e.engineWith(e.clA, e.clB)
	s = e.run()
	if s.Copied.AtoB != 1 || s.ItemErrorCount != 0 {
		t.Fatalf("second run: %+v", s)
	}
	if st := e.targetStill(); st.LivePhotoVideoID != mb || !e.inAlbumB(st.ID) {
		t.Fatalf("still %+v", st)
	}
}
