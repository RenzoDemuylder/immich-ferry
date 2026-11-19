package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

// Tests for the motion upload path of ensureMotion (livephoto.go) and the
// marker handling at the start of a mutating cycle (run.go).

func mmClient(t *testing.T, base, key string) *immich.Client {
	t.Helper()
	c, err := immich.New(immich.Config{BaseURL: base, APIKey: key, MaxRetries: 1,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mmAfterProxy forwards every request to target and calls after once the
// response has been written and flushed to the client.
func mmAfterProxy(t *testing.T, target *fakeimmich.Server, after func(r *http.Request, body []byte)) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target.URL())
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		rp.ServeHTTP(w, r)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		after(r, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mmUploads counts upload requests (POST /api/assets) that reached B.
func (e *env) mmUploads() int {
	n := 0
	for _, r := range e.fb.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/api/assets") {
			n++
		}
	}
	return n
}

func mmIsUpload(r *http.Request) bool {
	return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assets") &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/")
}

func mmStillOp(sc stillCase) string {
	if sc.existing {
		return "link"
	}
	return "motion"
}

// mmStillUntouched asserts the still did not reach B's album (and was neither
// uploaded nor linked).
func (e *env) mmStillUntouched(sc stillCase, pre string) {
	e.t.Helper()
	if sc.existing {
		if a, _ := e.fb.Asset(pre); a.LivePhotoVideoID != "" || e.inAlbumB(pre) {
			e.t.Fatalf("still=%+v inAlbum=%v", a, e.inAlbumB(pre))
		}
	} else if _, ok := e.findBy(e.fb, e.ub, "still"); ok {
		e.t.Fatal("still uploaded despite motion failure")
	}
}

// (A1) The motion's known size exceeds TmpBudgetBytes: checkTmpBudget fails
// fast in ensureMotion. The task is handled as too_large (30 day backoff, item
// error, no download of the motion or the still, nothing uploaded or linked).
func TestMMMotionTooLargeForBudget(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.TmpBudgetBytes = 10_000 })
			big := int64(1 << 20)
			mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
				o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
				o.Exif = &immich.Exif{FileSizeInByte: &big}
			})
			e.fa.RemoveFromAlbum(e.albumA, mot)
			e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
			pre := e.prepStill(sc)

			s := e.run()
			if s.Skipped[SkipTooLarge] != 1 || s.ItemErrorCount != 1 || !hasItemErrorOp(s, mmStillOp(sc)) ||
				s.Copied.AtoB != 0 || s.Linked.AtoB != 0 || s.Result == ResultOK {
				t.Fatalf("%+v", s)
			}
			if n := e.countReq(e.fa, "GET", "/original"); n != 0 {
				t.Fatalf("downloaded %d times despite the motion's known oversize", n)
			}
			if n := e.mmUploads(); n != 0 {
				t.Fatalf("%d uploads to B", n)
			}
			if n := len(e.motionsOnB()); n != 0 {
				t.Fatalf("%d motions on B", n)
			}
			e.mmStillUntouched(sc, pre)
			key := e.checksumA(e.onA("still").ID)
			it := e.loadState().Items[key]
			if it == nil || it.Failure == nil || !it.Failure.Next.Equal(e.clk.Now().Add(tooLargeBackoff)) {
				t.Fatalf("too_large backoff not recorded: %+v", it)
			}
			if it.Intent != nil {
				t.Fatalf("intent left behind: %+v", it.Intent)
			}
			e.noTempFiles()
		})
	}
}

// (A2) Downloading the motion fails every attempt: the task fails (item error,
// failure recorded), no temp file is left, nothing is uploaded or linked, and a
// later run completes it.
func TestMMMotionDownloadErrorRetried(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			mot, _ := e.seedLiveA()
			pre := e.prepStill(sc)
			// fetch makes downloadAttempts (3) attempts; the client does not retry downloads itself.
			const attempts = downloadAttempts
			e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + mot + "/original", Status: 500, Times: attempts})
			s := e.run()
			if n := e.countReq(e.fa, "GET", "/assets/"+mot+"/original"); n != attempts {
				t.Fatalf("motion downloads=%d want %d", n, attempts)
			}
			if s.ItemErrorCount != 1 || !hasItemErrorOp(s, mmStillOp(sc)) || s.Copied.AtoB != 0 || s.Linked.AtoB != 0 {
				t.Fatalf("%+v", s)
			}
			if n := e.mmUploads(); n != 0 {
				t.Fatalf("%d uploads to B", n)
			}
			if n := len(e.motionsOnB()); n != 0 {
				t.Fatalf("%d motions on B", n)
			}
			e.mmStillUntouched(sc, pre)
			e.noTempFiles()
			key := e.checksumA(e.onA("still").ID)
			if it := e.loadState().Items[key]; it == nil || it.Failure == nil || it.Failure.Count != 1 {
				t.Fatalf("failure not recorded: %+v", it)
			}

			e.clk.Advance(48 * time.Hour)
			s = e.run()
			if s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
				t.Fatalf("retry: %+v", s)
			}
			ms := e.motionsOnB()
			if len(ms) != 1 || ms[0].Visibility != immich.VisibilityHidden || e.inAlbumB(ms[0].ID) {
				t.Fatalf("motions on B: %+v", ms)
			}
			if st := e.targetStill(); st.LivePhotoVideoID != ms[0].ID || !e.inAlbumB(st.ID) {
				t.Fatalf("still=%+v", st)
			}
			e.noTempFiles()
		})
	}
}

// (A2b) The motion task waits for the temp budget (held by another download)
// when the run is cancelled: cancellation is never recorded as a failure, no
// temp files remain, nothing is uploaded, and the next run copies everything.
// The cancel fires right after B answered the motion's upload check, so the
// task is (almost always) blocked in budget.acquire; either way the outcome
// must be the same.
func TestMMMotionBudgetWaitCancelled(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TmpBudgetBytes = 10_000 })
	mot, _ := e.seedLiveA()
	blocker := e.seedA("blocker") // no exif: unknown size reserves the whole budget
	// The blocker's download hangs (until its request is cancelled) while holding the budget.
	e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + blocker + "/original", Delay: time.Hour, Times: 1})

	blockerStarted := make(chan struct{})
	var startOnce sync.Once
	pa := newInterceptProxy(t, e.fa)
	pa.setHook(func(r *http.Request, _ []byte) (int, string, bool) {
		if r.Method != "GET" {
			return 0, "", false
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/assets/"+blocker+"/original"):
			startOnce.Do(func() { close(blockerStarted) })
		case strings.HasSuffix(r.URL.Path, "/assets/"+mot):
			// Let the motion task proceed only once the blocker holds the budget.
			select {
			case <-blockerStarted:
			case <-r.Context().Done():
			}
		}
		return 0, "", false
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var checked sync.Once
	pb := mmAfterProxy(t, e.fb, func(r *http.Request, body []byte) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assets/bulk-upload-check") &&
			bytes.Contains(body, []byte(`"`+mot+`"`)) {
			checked.Do(cancel)
		}
	})
	e.clA = mmClient(t, pa.srv.URL, e.keyA)
	e.clB = mmClient(t, pb.URL, e.keyB)
	e.eng = e.newEngine(e.opts)

	s, err := e.eng.RunPair(ctx, e.pair())
	if err == nil && s.Result == ResultOK {
		t.Fatalf("cancelled run reported ok: %+v", s)
	}
	if s.ItemErrorCount != 0 || s.Copied.AtoB != 0 {
		t.Fatalf("cancellation recorded as item error / copy: %+v err=%v", s, err)
	}
	if n := e.mmUploads(); n != 0 {
		t.Fatalf("%d uploads to B", n)
	}
	if n := e.countReq(e.fa, "GET", "/assets/"+mot+"/original"); n != 0 {
		t.Fatalf("motion downloaded %d times while the budget was held", n)
	}
	if len(e.fb.AssetsOf(e.ub)) != 0 {
		t.Fatalf("assets on B: %+v", e.fb.AssetsOf(e.ub))
	}
	e.noTempFiles()
	if e.stateFileExists() {
		for k, it := range e.loadState().Items {
			if it.Failure != nil {
				t.Fatalf("cancellation recorded as failure for %s: %+v", k, it.Failure)
			}
		}
	}

	s = e.run()
	if s.ItemErrorCount != 0 || s.Copied.AtoB != 2 {
		t.Fatalf("second run: %+v", s)
	}
	ms := e.motionsOnB()
	if len(ms) != 1 || ms[0].Visibility != immich.VisibilityHidden || e.inAlbumB(ms[0].ID) {
		t.Fatalf("motions on B: %+v", ms)
	}
	if st := e.targetStill(); st.LivePhotoVideoID != ms[0].ID || !e.inAlbumB(st.ID) {
		t.Fatalf("still=%+v", st)
	}
	e.noTempFiles()
}

// (A3) The motion upload fails: the task fails, nothing is linked or added,
// no temp files remain, and a later run completes it.
func TestMMMotionUploadErrorRetried(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.routeBThroughProxy()
			e.seedLiveA()
			pre := e.prepStill(sc)
			p.setHook(func(r *http.Request, _ []byte) (int, string, bool) {
				if !mmIsUpload(r) {
					return 0, "", false
				}
				_, _ = io.Copy(io.Discard, r.Body)
				return 500, `{"message":"boom","statusCode":500,"error":"Internal Server Error"}`, true
			})
			s := e.run()
			if p.hitCount() == 0 {
				t.Fatal("motion upload not intercepted")
			}
			if s.ItemErrorCount != 1 || !hasItemErrorOp(s, mmStillOp(sc)) || s.Copied.AtoB != 0 || s.Linked.AtoB != 0 {
				t.Fatalf("%+v", s)
			}
			if n := len(e.motionsOnB()); n != 0 {
				t.Fatalf("%d motions on B", n)
			}
			e.mmStillUntouched(sc, pre)
			e.noTempFiles()
			key := e.checksumA(e.onA("still").ID)
			if it := e.loadState().Items[key]; it == nil || it.Failure == nil || it.Failure.Count != 1 {
				t.Fatalf("failure not recorded: %+v", it)
			}

			p.setHook(nil)
			e.clk.Advance(48 * time.Hour)
			s = e.run()
			if s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
				t.Fatalf("retry: %+v", s)
			}
			ms := e.motionsOnB()
			if len(ms) != 1 || ms[0].Visibility != immich.VisibilityHidden || e.inAlbumB(ms[0].ID) {
				t.Fatalf("motions on B: %+v", ms)
			}
			if st := e.targetStill(); st.LivePhotoVideoID != ms[0].ID || !e.inAlbumB(st.ID) {
				t.Fatalf("still=%+v", st)
			}
			e.noTempFiles()
		})
	}
}

// (A4) The motion upload check says "accept" but the upload answers
// "duplicate" (B already holds it): useExistingMotion reuses the existing
// hidden motion (restoring it when trashed), no second motion appears on B.
func TestMMMotionUploadDuplicateReused(t *testing.T) {
	for _, trashed := range []bool{false, true} {
		for _, sc := range stillCases {
			name := "hidden/" + sc.name
			if trashed {
				name = "trashed/" + sc.name
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				if trashed {
					e.warmup() // no restore during bootstrap
				}
				p := e.routeBThroughProxy()
				mot, _ := e.seedLiveA()
				mb := e.seedMotionB(immich.VisibilityHidden)
				if trashed {
					e.fb.Trash(mb)
				}
				pre := e.prepStill(sc)
				uploadsBefore := e.mmUploads()
				p.setHook(func(r *http.Request, body []byte) (int, string, bool) {
					if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assets/bulk-upload-check") &&
						bytes.Contains(body, []byte(`"`+mot+`"`)) {
						return 200, `{"results":[{"id":"` + mot + `","action":"accept"}]}`, true
					}
					return 0, "", false
				})
				s := e.run()
				if p.hitCount() != 1 {
					t.Fatalf("motion upload check intercepted %d times", p.hitCount())
				}
				if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
					t.Fatalf("%+v", s)
				}
				if sc.existing && s.Linked.AtoB != 1 {
					t.Fatalf("not linked: %+v", s)
				}
				wantRestored := 0
				if trashed {
					wantRestored = 1
				}
				if s.Restored.AtoB != wantRestored {
					t.Fatalf("Restored=%+v", s.Restored)
				}
				wantUploads := 2 // motion (answered duplicate) + still
				if sc.existing {
					wantUploads = 1
				}
				if n := e.mmUploads() - uploadsBefore; n != wantUploads {
					t.Fatalf("uploads=%d want %d", n, wantUploads)
				}
				if n := len(e.motionsOnB()); n != 1 {
					t.Fatalf("second motion on B: %d", n)
				}
				m, _ := e.fb.Asset(mb)
				if m.Visibility != immich.VisibilityHidden || m.IsTrashed || e.inAlbumB(mb) {
					t.Fatalf("motion=%+v inAlbum=%v", m, e.inAlbumB(mb))
				}
				st := e.targetStill()
				if st.LivePhotoVideoID != mb || !e.inAlbumB(st.ID) {
					t.Fatalf("still=%+v want linked to %s", st, mb)
				}
				if sc.existing && st.ID != pre {
					t.Fatalf("still re-uploaded: %s vs %s", st.ID, pre)
				}
				e.noTempFiles()
			})
		}
	}
}

// --- markers (run.go) ---

// mmSnapshotDir returns path → content (directories as "<dir>") below root.
func mmSnapshotDir(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[rel] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mmSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if w, ok := after[k]; !ok {
			t.Errorf("state entry %s removed", k)
		} else if w != v {
			t.Errorf("state entry %s changed", k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("state entry %s added", k)
		}
	}
}

// (B) A failing marker read or reset fails the cycle before anything is
// mutated: RunPair errors, neither server sees a mutation, state is untouched.
func TestMMMarkerFailuresAbortCycle(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env) *Engine // returns the engine to run
		check func(t *testing.T, err error)
	}{
		{"reset_marker_unreadable", func(e *env) *Engine {
			// A non-empty directory at the marker path: os.Remove fails (not ErrNotExist).
			if err := os.MkdirAll(filepath.Join(e.dir, "pairs", "trip.reset.marker", "x"), 0o755); err != nil {
				e.t.Fatal(err)
			}
			return e.eng
		}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "reset marker") {
				t.Fatalf("err=%v", err)
			}
		}},
		{"approve_marker_unreadable", func(e *env) *Engine {
			if err := os.MkdirAll(filepath.Join(e.dir, "pairs", "trip.approve-removals.marker", "x"), 0o755); err != nil {
				e.t.Fatal(err)
			}
			return e.eng
		}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "approve-removals marker") {
				t.Fatalf("err=%v", err)
			}
		}},
		{"reset_fails", func(e *env) *Engine {
			// A read-only store refuses Reset; the marker must stay for the next run.
			ro, err := state.OpenReadOnly(e.dir)
			if err != nil {
				e.t.Fatal(err)
			}
			if err := ro.PutMarker("trip", "reset"); err != nil {
				e.t.Fatal(err)
			}
			o := e.opts
			eng, err := New(o, e.clA, e.clB, ro, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				e.t.Fatal(err)
			}
			return eng
		}, func(t *testing.T, err error) {
			if !errors.Is(err, state.ErrReadOnly) {
				t.Fatalf("err=%v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.warmup()
			e.seedA("fresh")
			gone := e.seedB("gone-soon")
			e.run() // gone-soon copied to A
			e.fb.RemoveFromAlbum(e.albumB, gone)
			eng := c.setup(e)
			before := mmSnapshotDir(t, filepath.Join(e.dir, "pairs"))
			ma, mb := e.fa.Mutations(), e.fb.Mutations()
			e.seedA("fresh2")

			s, err := eng.RunPair(context.Background(), e.pair())
			c.check(t, err)
			if s.Result != ResultFailed || s.Mutations != 0 {
				t.Fatalf("%+v", s)
			}
			if e.fa.Mutations() != ma || e.fb.Mutations() != mb {
				t.Fatalf("mutations a %d→%d b %d→%d", ma, e.fa.Mutations(), mb, e.fb.Mutations())
			}
			if _, ok := e.findBy(e.fb, e.ub, "fresh2"); ok {
				t.Fatal("fresh2 copied")
			}
			after := mmSnapshotDir(t, filepath.Join(e.dir, "pairs"))
			mmSameSnapshot(t, before, after)
		})
	}
}

// A reset marker must survive a failed reset, otherwise the operator's
// request is silently dropped and the next cycle syncs with the old state.
func TestMMResetFailureKeepsMarker(t *testing.T) {
	e := newEnv(t)
	e.warmup()
	ro, err := state.OpenReadOnly(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.PutMarker("trip", "reset"); err != nil {
		t.Fatal(err)
	}
	eng, err := New(e.opts, e.clA, e.clB, ro, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunPair(context.Background(), e.pair()); !errors.Is(err, state.ErrReadOnly) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "pairs", "trip.reset.marker")); err != nil {
		t.Fatalf("reset marker consumed although the reset failed: %v", err)
	}
}

// An approve-removals marker survives a cycle that aborts before execution
// (here: snapshot fails) and is consumed by the next cycle that executes.
func TestMMApproveMarkerKeptUntilExecution(t *testing.T) {
	e := newEnv(t)
	e.warmup()
	marker := filepath.Join(e.dir, "pairs", "trip.approve-removals.marker")
	if err := e.st.PutMarker("trip", "approve-removals"); err != nil {
		t.Fatal(err)
	}
	// One non-retried 400 on A's album listing aborts the cycle before execution.
	e.fa.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/search/metadata", Status: 400, Times: 1})
	if _, err := e.eng.RunPair(context.Background(), e.pair()); err == nil {
		t.Fatal("want snapshot error")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approve-removals marker lost by an aborted cycle: %v", err)
	}
	e.run()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approve-removals marker not consumed by an executing cycle: %v", err)
	}
}

// A dry run never consumes markers.
func TestMMDryRunKeepsMarkers(t *testing.T) {
	e := newEnv(t)
	e.warmup()
	for _, m := range []string{"reset", "approve-removals"} {
		if err := e.st.PutMarker("trip", m); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.eng.PlanPair(context.Background(), e.pair()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"reset", "approve-removals"} {
		if _, err := os.Stat(filepath.Join(e.dir, "pairs", "trip."+m+".marker")); err != nil {
			t.Fatalf("%s marker consumed by a plan-only run: %v", m, err)
		}
	}
}
