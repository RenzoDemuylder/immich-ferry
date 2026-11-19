package engine

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

// Tests for the uncovered paths of livephoto.go (ensureMotion / useExistingMotion).

// interceptProxy sits in front of a fake server and lets a test answer selected
// requests itself (e.g. by request body, which fakeimmich.Fault cannot match).
type interceptProxy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hook func(r *http.Request, body []byte) (status int, respBody string, handled bool)
	hits int
}

func newInterceptProxy(t *testing.T, target *fakeimmich.Server) *interceptProxy {
	t.Helper()
	u, err := url.Parse(target.URL())
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	p := &interceptProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		p.mu.Lock()
		h := p.hook
		p.mu.Unlock()
		if h != nil {
			if st, rb, ok := h(r, body); ok {
				p.mu.Lock()
				p.hits++
				p.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st)
				_, _ = io.WriteString(w, rb)
				return
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *interceptProxy) setHook(h func(r *http.Request, body []byte) (int, string, bool)) {
	p.mu.Lock()
	p.hook = h
	p.mu.Unlock()
}

func (p *interceptProxy) hitCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

// routeBThroughProxy rebuilds the engine so that side B talks to the proxy.
func (e *env) routeBThroughProxy() *interceptProxy {
	e.t.Helper()
	p := newInterceptProxy(e.t, e.fb)
	c, err := immich.New(immich.Config{BaseURL: p.srv.URL, APIKey: e.keyB, MaxRetries: 1,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	e.clB = c
	e.eng = e.newEngine(e.opts)
	return p
}

// seedLiveA seeds a hidden motion and a still linked to it in album A.
func (e *env) seedLiveA() (motionID, stillID string) {
	motionID = e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	// a motion part is never an album member
	e.fa.RemoveFromAlbum(e.albumA, motionID)
	stillID = e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = motionID })
	return motionID, stillID
}

// seedMotionB seeds the same motion content on B (outside the album).
func (e *env) seedMotionB(vis immich.Visibility) string {
	return e.seed(e.fb, e.ub, "", "motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", vis
	})
}

func (e *env) motionsOnB() []fakeimmich.FakeAsset {
	var out []fakeimmich.FakeAsset
	for _, a := range e.fb.AssetsOf(e.ub) {
		if a.OriginalFileName == "motion.mp4" {
			out = append(out, a)
		}
	}
	return out
}

func (e *env) warmup() {
	e.t.Helper()
	e.seedA("warmup")
	if s := e.run(); s.Result != ResultOK || s.ItemErrorCount != 0 {
		e.t.Fatalf("warmup: %+v", s)
	}
}

// stillCase selects how the still reaches ensureMotion: copyNew (target lacks
// the still) or copyExisting+linkLive (target holds an unlinked copy).
type stillCase struct {
	name     string
	existing bool
}

var stillCases = []stillCase{{"still_missing", false}, {"still_exists_unlinked", true}}

func (e *env) prepStill(sc stillCase) string {
	if sc.existing {
		return e.seed(e.fb, e.ub, "", "still")
	}
	return ""
}

// targetStill returns B's copy of the still and fails if there is not exactly one.
func (e *env) targetStill() fakeimmich.FakeAsset {
	e.t.Helper()
	var out []fakeimmich.FakeAsset
	for _, a := range e.fb.AssetsOf(e.ub) {
		if a.OriginalFileName == "still.jpg" {
			out = append(out, a)
		}
	}
	if len(out) != 1 {
		e.t.Fatalf("want exactly one still on B, got %d", len(out))
	}
	return out[0]
}

func hasItemErrorOp(s RunSummary, op string) bool {
	for _, ie := range s.ItemErrors {
		if ie.Op == op {
			return true
		}
	}
	return false
}

// 1. Hidden existing target motion is reused (no second upload) and linked.
func TestLiveMotionExistingHiddenReused(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedLiveA()
			mb := e.seedMotionB(immich.VisibilityHidden)
			e.prepStill(sc)
			s := e.run()
			if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
				t.Fatalf("%+v", s)
			}
			if sc.existing && s.Linked.AtoB != 1 {
				t.Fatalf("not linked: %+v", s)
			}
			if n := len(e.motionsOnB()); n != 1 {
				t.Fatalf("motion uploaded again: %d motions on B", n)
			}
			wantUploads := 1 // the still
			if sc.existing {
				wantUploads = 0
			}
			if n := e.countReq(e.fb, "POST", "/assets"); n != wantUploads {
				t.Errorf("uploads=%d want %d", n, wantUploads)
			}
			st := e.targetStill()
			if st.LivePhotoVideoID != mb || !e.inAlbumB(st.ID) {
				t.Fatalf("still=%+v want linked to %s", st, mb)
			}
			m, _ := e.fb.Asset(mb)
			if m.Visibility != immich.VisibilityHidden || e.inAlbumB(mb) || m.IsTrashed {
				t.Fatalf("motion=%+v inAlbum=%v", m, e.inAlbumB(mb))
			}
		})
	}
}

// 2. Trashed hidden target motion with RestoreTrashed outside bootstrap is
// restored, counted and linked.
func TestLiveMotionExistingTrashedRestored(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			e.warmup()
			e.seedLiveA()
			mb := e.seedMotionB(immich.VisibilityHidden)
			e.fb.Trash(mb)
			e.prepStill(sc)
			s := e.run()
			if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
				t.Fatalf("%+v", s)
			}
			if s.Restored.AtoB != 1 || s.Restored.BtoA != 0 {
				t.Fatalf("Restored=%+v", s.Restored)
			}
			if e.countReq(e.fb, "POST", "/trash/restore/assets") != 1 {
				t.Error("expected exactly one restore request")
			}
			m, _ := e.fb.Asset(mb)
			if m.IsTrashed || m.Visibility != immich.VisibilityHidden || e.inAlbumB(mb) {
				t.Fatalf("motion=%+v", m)
			}
			if n := len(e.motionsOnB()); n != 1 {
				t.Fatalf("%d motions on B", n)
			}
			if st := e.targetStill(); st.LivePhotoVideoID != mb {
				t.Fatalf("still not linked to restored motion: %+v", st)
			}
		})
	}
}

// 3. A trashed target motion that must not be restored (RestoreTrashed=false,
// or bootstrap) → still copied without link, motion stays trashed.
func TestLiveMotionExistingTrashedNotRestored(t *testing.T) {
	cases := []struct {
		name      string
		restore   bool
		bootstrap bool
	}{
		{"restore_disabled", false, false},
		{"bootstrap", true, true},
	}
	for _, c := range cases {
		for _, sc := range stillCases {
			t.Run(c.name+"/"+sc.name, func(t *testing.T) {
				e := newEnv(t, func(o *Options) { o.RestoreTrashed = c.restore })
				if !c.bootstrap {
					e.warmup()
				}
				e.seedLiveA()
				mb := e.seedMotionB(immich.VisibilityHidden)
				e.fb.Trash(mb)
				e.prepStill(sc)
				s := e.run()
				if s.Bootstrap != c.bootstrap {
					t.Fatalf("bootstrap=%v", s.Bootstrap)
				}
				if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
					t.Fatalf("still must be copied without link: %+v", s)
				}
				if s.Restored.AtoB != 0 || s.Linked.AtoB != 0 {
					t.Fatalf("%+v", s)
				}
				if e.countReq(e.fb, "POST", "/trash/restore/assets") != 0 {
					t.Error("restore requested")
				}
				m, _ := e.fb.Asset(mb)
				if !m.IsTrashed || e.inAlbumB(mb) {
					t.Fatalf("motion=%+v", m)
				}
				if n := len(e.motionsOnB()); n != 1 {
					t.Fatalf("%d motions on B (must not upload a second copy)", n)
				}
				st := e.targetStill()
				if st.LivePhotoVideoID != "" || !e.inAlbumB(st.ID) {
					t.Fatalf("still=%+v", st)
				}
			})
		}
	}
}

// 4a. The target holds the motion but GET says not found (locked) →
// errVisibleMotion: still copied without link, motion untouched.
func TestLiveMotionExistingUnreadable(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedLiveA()
			mb := e.seedMotionB(immich.VisibilityLocked)
			e.prepStill(sc)
			s := e.run()
			if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 || s.Linked.AtoB != 0 {
				t.Fatalf("%+v", s)
			}
			if n := len(e.motionsOnB()); n != 1 {
				t.Fatalf("%d motions on B", n)
			}
			m, _ := e.fb.Asset(mb)
			if m.Visibility != immich.VisibilityLocked || e.inAlbumB(mb) {
				t.Fatalf("motion=%+v", m)
			}
			if st := e.targetStill(); st.LivePhotoVideoID != "" {
				t.Fatalf("linked unreadable motion: %+v", st)
			}
		})
	}
}

// 4b. Any other GetAsset error on the existing motion fails the task (nothing
// copied, backoff recorded), and a later run completes it.
func TestLiveMotionExistingGetErrorRetried(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedLiveA()
			mb := e.seedMotionB(immich.VisibilityHidden)
			pre := e.prepStill(sc)
			// MaxRetries 1 → two attempts.
			e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + mb, Status: 500, Times: 2})
			s := e.run()
			op := "motion"
			if sc.existing {
				op = "link"
			}
			if s.ItemErrorCount != 1 || !hasItemErrorOp(s, op) || s.Copied.AtoB != 0 || s.Linked.AtoB != 0 {
				t.Fatalf("%+v", s)
			}
			if sc.existing {
				if e.inAlbumB(pre) {
					t.Error("still added to album despite failed link")
				}
			} else if _, ok := e.findBy(e.fb, e.ub, "still"); ok {
				t.Error("still uploaded despite motion failure")
			}
			key := e.checksumA(e.onA("still").ID)
			if it := e.loadState().Items[key]; it == nil || it.Failure == nil || it.Failure.Count != 1 {
				t.Fatalf("failure not recorded: %+v", it)
			}
			e.clk.Advance(48 * time.Hour)
			s = e.run()
			if s.ItemErrorCount != 0 || s.Copied.AtoB != 1 {
				t.Fatalf("retry: %+v", s)
			}
			st := e.targetStill()
			if st.LivePhotoVideoID != mb || !e.inAlbumB(st.ID) || e.inAlbumB(mb) {
				t.Fatalf("retry still=%+v", st)
			}
		})
	}
}

// 5. Restore "succeeds" but the motion is still trashed → refused.
func TestLiveMotionRestoreLeavesTrashedRefused(t *testing.T) {
	e := newEnv(t)
	e.warmup()
	p := e.routeBThroughProxy()
	e.seedLiveA()
	mb := e.seedMotionB(immich.VisibilityHidden)
	e.fb.Trash(mb)
	p.setHook(func(r *http.Request, _ []byte) (int, string, bool) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/trash/restore/assets") {
			return 200, `{"count":0}`, true // pretend success, change nothing
		}
		return 0, "", false
	})
	s := e.run()
	if p.hitCount() != 1 {
		t.Fatalf("restore intercepted %d times", p.hitCount())
	}
	if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 || s.Linked.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	m, _ := e.fb.Asset(mb)
	if !m.IsTrashed || e.inAlbumB(mb) {
		t.Fatalf("motion=%+v", m)
	}
	if n := len(e.motionsOnB()); n != 1 {
		t.Fatalf("%d motions on B", n)
	}
	if st := e.targetStill(); st.LivePhotoVideoID != "" {
		t.Fatalf("linked a trashed motion: %+v", st)
	}
}

// Companion to the test above: the Restored counter should only count a
// restore that took effect (copyExisting counts after its re-check).
func TestLiveMotionRestoreLeavesTrashedNotCounted(t *testing.T) {
	e := newEnv(t)
	e.warmup()
	p := e.routeBThroughProxy()
	e.seedLiveA()
	mb := e.seedMotionB(immich.VisibilityHidden)
	e.fb.Trash(mb)
	p.setHook(func(r *http.Request, _ []byte) (int, string, bool) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/trash/restore/assets") {
			return 200, `{"count":0}`, true
		}
		return 0, "", false
	})
	if s := e.run(); s.Restored.AtoB != 0 {
		t.Fatalf("Restored counted for a restore that did not take effect: %+v", s.Restored)
	}
}

// ensureMotion: the source motion is gone (GET NotFound) → errMotionGone:
// still copied/kept without motion, no error, nothing uploaded for the motion.
func TestLiveMotionSourceGone(t *testing.T) {
	for _, sc := range stillCases {
		t.Run(sc.name, func(t *testing.T) {
			e := newEnv(t)
			mot, _ := e.seedLiveA()
			e.prepStill(sc)
			e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + mot, Status: 400, Times: 100,
				Body: `{"message":"Not found or no asset.read access","statusCode":400,"error":"Bad Request"}`})
			s := e.run()
			if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 || s.Linked.AtoB != 0 {
				t.Fatalf("%+v", s)
			}
			if n := len(e.motionsOnB()); n != 0 {
				t.Fatalf("motion copied: %d", n)
			}
			st := e.targetStill()
			if st.LivePhotoVideoID != "" || !e.inAlbumB(st.ID) {
				t.Fatalf("still=%+v", st)
			}
			e.noTempFiles()
		})
	}
}

// ensureMotion: the motion's UploadCheck fails or returns no result → the
// task fails (item error, backoff), nothing is uploaded or linked.
func TestLiveMotionUploadCheckFailure(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		msg        string
	}{
		{"error_500", `{"message":"boom","statusCode":500,"error":"Internal Server Error"}`, 500, ""},
		{"empty_result", `{"results":[]}`, 200, "returned no result"},
	}
	for _, c := range cases {
		for _, sc := range stillCases {
			t.Run(c.name+"/"+sc.name, func(t *testing.T) {
				e := newEnv(t)
				p := e.routeBThroughProxy()
				mot, _ := e.seedLiveA()
				pre := e.prepStill(sc)
				p.setHook(func(r *http.Request, body []byte) (int, string, bool) {
					if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assets/bulk-upload-check") &&
						bytes.Contains(body, []byte(`"`+mot+`"`)) {
						return c.status, c.body, true
					}
					return 0, "", false
				})
				s := e.run()
				if p.hitCount() == 0 {
					t.Fatal("motion upload check not intercepted")
				}
				op := "motion"
				if sc.existing {
					op = "link"
				}
				if s.ItemErrorCount != 1 || !hasItemErrorOp(s, op) || s.Copied.AtoB != 0 || s.Linked.AtoB != 0 {
					t.Fatalf("%+v", s)
				}
				if c.msg != "" && !strings.Contains(s.ItemErrors[0].Msg, c.msg) {
					t.Errorf("msg=%q", s.ItemErrors[0].Msg)
				}
				if n := len(e.motionsOnB()); n != 0 {
					t.Fatalf("motion uploaded: %d", n)
				}
				if sc.existing {
					if a, _ := e.fb.Asset(pre); a.LivePhotoVideoID != "" || e.inAlbumB(pre) {
						t.Fatalf("still=%+v", a)
					}
				} else if _, ok := e.findBy(e.fb, e.ub, "still"); ok {
					t.Error("still uploaded despite motion failure")
				}
				key := e.checksumA(e.onA("still").ID)
				if it := e.loadState().Items[key]; it == nil || it.Failure == nil {
					t.Fatalf("failure not recorded: %+v", it)
				}
				e.noTempFiles()
			})
		}
	}
}

// A timeline/archive target motion is never linked on the copy paths (link
// repair is covered by TestLinkRepairNeverLinksVisibleMotion).
func TestLiveMotionVisibleTargetNeverLinked(t *testing.T) {
	for _, vis := range []immich.Visibility{immich.VisibilityTimeline, immich.VisibilityArchive} {
		for _, sc := range stillCases {
			t.Run(string(vis)+"/"+sc.name, func(t *testing.T) {
				e := newEnv(t)
				e.seedLiveA()
				mb := e.seedMotionB(vis)
				e.prepStill(sc)
				s := e.run()
				if s.Result != ResultOK || s.ItemErrorCount != 0 || s.Copied.AtoB != 1 || s.Linked.AtoB != 0 {
					t.Fatalf("%+v", s)
				}
				st := e.targetStill()
				if st.LivePhotoVideoID != "" || !e.inAlbumB(st.ID) {
					t.Fatalf("still=%+v", st)
				}
				m, _ := e.fb.Asset(mb)
				if m.Visibility != vis || m.IsTrashed || e.inAlbumB(mb) {
					t.Fatalf("visible motion changed: %+v", m)
				}
				if n := len(e.motionsOnB()); n != 1 {
					t.Fatalf("%d motions on B", n)
				}
			})
		}
	}
}

// Transient errors in ensureMotion/useExistingMotion (source motion GET,
// restore, GET after restore) fail the task: nothing copied, linked or counted,
// failure recorded, motion left as it was.
func TestLiveMotionTransientErrorsFailTask(t *testing.T) {
	cases := []struct {
		name         string
		trashed      bool
		wantRestored int // counted only once the re-read confirms the restore (as copyExisting does)
		setup        func(e *env, p *interceptProxy, mot, mb string)
	}{
		{"source_get_500", false, 0, func(e *env, _ *interceptProxy, mot, _ string) {
			e.fa.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + mot, Status: 500, Times: 2})
		}},
		{"restore_500", true, 0, func(e *env, _ *interceptProxy, _, _ string) {
			e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/trash/restore/assets", Status: 500, Times: 2})
		}},
		{"get_after_restore_500", true, 0, func(e *env, p *interceptProxy, _, mb string) {
			var mu sync.Mutex
			n := 0
			p.setHook(func(r *http.Request, _ []byte) (int, string, bool) {
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/assets/"+mb) {
					return 0, "", false
				}
				mu.Lock()
				defer mu.Unlock()
				n++
				if n == 1 { // the first GET (before the restore) passes
					return 0, "", false
				}
				return 500, `{"message":"boom","statusCode":500,"error":"Internal Server Error"}`, true
			})
		}},
	}
	for _, c := range cases {
		for _, sc := range stillCases {
			t.Run(c.name+"/"+sc.name, func(t *testing.T) {
				e := newEnv(t)
				e.warmup()
				p := e.routeBThroughProxy()
				mot, _ := e.seedLiveA()
				mb := e.seedMotionB(immich.VisibilityHidden)
				if c.trashed {
					e.fb.Trash(mb)
				}
				pre := e.prepStill(sc)
				c.setup(e, p, mot, mb)
				s := e.run()
				op := "motion"
				if sc.existing {
					op = "link"
				}
				if s.ItemErrorCount != 1 || !hasItemErrorOp(s, op) || s.Copied.AtoB != 0 || s.Linked.AtoB != 0 || s.Restored.AtoB != c.wantRestored {
					t.Fatalf("%+v", s)
				}
				if sc.existing {
					if a, _ := e.fb.Asset(pre); a.LivePhotoVideoID != "" || e.inAlbumB(pre) {
						t.Fatalf("still=%+v", a)
					}
				} else if _, ok := e.findBy(e.fb, e.ub, "still"); ok {
					t.Error("still uploaded despite motion failure")
				}
				m, _ := e.fb.Asset(mb)
				// get_after_restore_500: the restore took effect on the server; only the confirming re-read failed.
				stillTrashed := c.trashed && c.name != "get_after_restore_500"
				if m.Visibility != immich.VisibilityHidden || m.IsTrashed != stillTrashed {
					t.Fatalf("motion=%+v", m)
				}
				if n := len(e.motionsOnB()); n != 1 || e.inAlbumB(mb) {
					t.Fatalf("%d motions on B", n)
				}
				key := e.checksumA(e.onA("still").ID)
				if it := e.loadState().Items[key]; it == nil || it.Failure == nil {
					t.Fatalf("failure not recorded: %+v", it)
				}
			})
		}
	}
}
