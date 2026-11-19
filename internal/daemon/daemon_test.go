package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"immich-ferry/internal/config"
	"immich-ferry/internal/engine"
	"immich-ferry/internal/immich"
)

type fakeRunner struct {
	mu        sync.Mutex
	pfErr     error
	pfCalls   int
	results   map[string]engine.RunSummary
	errs      map[string]error
	runs      []string
	onRunPair func(ctx context.Context, p engine.Pair)
	pairs     func(static []engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error)
}

func (f *fakeRunner) Preflight(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pfCalls++
	return f.pfErr
}

func (f *fakeRunner) Pairs(_ context.Context, static []engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
	if f.pairs != nil {
		return f.pairs(static)
	}
	return static, engine.DiscoveryReport{}, nil
}

func (f *fakeRunner) RunPair(ctx context.Context, p engine.Pair) (engine.RunSummary, error) {
	f.mu.Lock()
	f.runs = append(f.runs, p.Name)
	s, e := f.results[p.Name], f.errs[p.Name]
	cb := f.onRunPair
	f.mu.Unlock()
	if cb != nil {
		cb(ctx, p)
	}
	return s, e
}

func testCfg(pairs ...string) *config.Config {
	c := &config.Config{Interval: 20 * time.Millisecond, HTTPAddr: "127.0.0.1:0"}
	c.Instances[0] = config.Instance{URL: "http://a", APIKey: "supersecret"}
	c.Instances[1] = config.Instance{URL: "http://b", APIKey: "supersecret"}
	for _, p := range pairs {
		c.Pairs = append(c.Pairs, engine.Pair{Name: p})
	}
	return c
}

func newD(t *testing.T, r Runner, pairs ...string) (*Daemon, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	reg := prometheus.NewRegistry()
	d := New(testCfg(pairs...), r, reg, nil)
	var buf bytes.Buffer
	d.SetOutput(&buf)
	return d, reg, &buf
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

func TestMetricsFromSummary(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	f := &fakeRunner{results: map[string]engine.RunSummary{"p": {
		Pair: "p", Result: engine.ResultOK, Started: now.Add(-5 * time.Second), Finished: now,
		AlbumIDs: [2]string{"x", "y"}, AlbumSizes: [2]int{10, 12}, SnapshotConsistent: [2]bool{true, false}, GuardTripped: [2]bool{false, true},
		Copied: engine.PerDir{AtoB: 3, BtoA: 1}, Unlinked: engine.PerDir{AtoB: 2}, MetaUpdates: engine.PerDir{BtoA: 4},
		BytesDown: 100, BytesUp: 90, Pending: 7, Skipped: map[engine.SkipReason]int{engine.SkipBlocked: 2},
		ItemErrors:     []engine.ItemError{{Kind: immich.KindTransient}, {Kind: immich.KindTransient}, {Kind: immich.KindAuth}},
		ItemErrorCount: 5,
	}}}
	d, reg, _ := newD(t, f, "p")
	if code := d.Once(context.Background()); code != 0 {
		t.Fatalf("code %d", code)
	}
	cases := map[string]float64{
		`immich_ferry_runs_total{pair="p",result="ok"}`:                    1,
		`immich_ferry_last_success_timestamp_seconds{pair="p"}`:            1_700_000_100,
		`immich_ferry_run_duration_seconds{pair="p"}`:                      5,
		`immich_ferry_assets_copied_total{direction="a_to_b",pair="p"}`:    3,
		`immich_ferry_assets_copied_total{direction="b_to_a",pair="p"}`:    1,
		`immich_ferry_bytes_total{kind="down",pair="p"}`:                   100,
		`immich_ferry_bytes_total{kind="up",pair="p"}`:                     90,
		`immich_ferry_unlinked_total{direction="a_to_b",pair="p"}`:         2,
		`immich_ferry_metadata_updates_total{direction="b_to_a",pair="p"}`: 4,
		`immich_ferry_skipped_total{pair="p",reason="blocked"}`:            2,
		`immich_ferry_item_errors_total{kind="transient",pair="p"}`:        2,
		`immich_ferry_item_errors_total{kind="auth",pair="p"}`:             1,
		`immich_ferry_item_errors_total{kind="other",pair="p"}`:            2,
		`immich_ferry_album_assets{pair="p",side="b"}`:                     12,
		`immich_ferry_pending{pair="p"}`:                                   7,
		`immich_ferry_guard_tripped{pair="p",side="b"}`:                    1,
		`immich_ferry_guard_tripped{pair="p",side="a"}`:                    0,
		`immich_ferry_snapshot_consistent{pair="p",side="a"}`:              1,
		`immich_ferry_snapshot_consistent{pair="p",side="b"}`:              0,
		`immich_ferry_preflight_ok{side="a"}`:                              1,
		`immich_ferry_preflight_ok{side="b"}`:                              1,
	}
	got := gather(t, reg)
	for k, want := range cases {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: got %v (present=%v) want %v", k, g, ok, want)
		}
	}
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			var lbl []string
			for _, l := range m.Label {
				lbl = append(lbl, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
			}
			key := mf.GetName() + "{" + strings.Join(lbl, ",") + "}"
			switch {
			case m.Counter != nil:
				got[key] = m.Counter.GetValue()
			case m.Gauge != nil:
				got[key] = m.Gauge.GetValue()
			}
		}
	}
	return got
}

func TestPreflightErrorMetric(t *testing.T) {
	f := &fakeRunner{pfErr: &engine.PreflightError{Side: engine.B, Err: errors.New("down")}}
	d, reg, _ := newD(t, f, "p")
	if code := d.Once(context.Background()); code != 1 {
		t.Fatalf("code %d", code)
	}
	g := gather(t, reg)
	if g[`immich_ferry_preflight_ok{side="a"}`] != 1 || g[`immich_ferry_preflight_ok{side="b"}`] != 0 {
		t.Errorf("%v", g)
	}
	f.pfErr = errors.New("unknown side")
	d.Once(context.Background())
	g = gather(t, reg)
	if g[`immich_ferry_preflight_ok{side="a"}`] != 0 || g[`immich_ferry_preflight_ok{side="b"}`] != 0 {
		t.Errorf("%v", g)
	}
}

func TestOnceExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		pf     error
		res    engine.Result
		err    error
		strict bool
		want   int
	}{
		{"ok", nil, engine.ResultOK, nil, false, 0},
		{"dry", nil, engine.ResultDryRun, nil, true, 0},
		{"partial", nil, engine.ResultPartial, nil, false, 0},
		{"partial strict", nil, engine.ResultPartial, nil, true, 1},
		{"failed", nil, engine.ResultFailed, nil, false, 1},
		{"run error", nil, "", errors.New("boom"), false, 1},
		{"run config error", nil, "", fmt.Errorf("x: %w", engine.ErrConfig), false, 2},
		{"preflight config", fmt.Errorf("x: %w", engine.ErrConfig), "", nil, false, 2},
		{"preflight missing perms", &engine.PreflightError{Missing: []string{"asset.read"}, Err: errors.New("m")}, "", nil, false, 2},
		{"preflight transport", &engine.PreflightError{Err: errors.New("dial")}, "", nil, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{pfErr: c.pf, results: map[string]engine.RunSummary{"p": {Result: c.res}}, errs: map[string]error{"p": c.err}}
			d, _, buf := newD(t, f, "p")
			d.SetStrict(c.strict)
			if got := d.Once(context.Background()); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
			if c.pf == nil {
				var line map[string]any
				if err := json.Unmarshal(buf.Bytes(), &line); err != nil || line["pair"] != "p" {
					t.Fatalf("summary line: %q %v", buf.String(), err)
				}
			}
		})
	}
}

func TestOnceMultiPairContinuesAndPrefersConfig(t *testing.T) {
	f := &fakeRunner{
		results: map[string]engine.RunSummary{"a": {Result: engine.ResultFailed}},
		errs:    map[string]error{"b": engine.ErrConfig},
	}
	d, _, buf := newD(t, f, "a", "b", "c")
	if got := d.Once(context.Background()); got != 2 {
		t.Fatalf("got %d", got)
	}
	if len(f.runs) != 3 || strings.Count(buf.String(), "\n") != 3 {
		t.Fatalf("runs=%v out=%q", f.runs, buf.String())
	}
}

func TestHealthzStaleLogic(t *testing.T) {
	var now time.Time = time.Unix(1_000_000, 0)
	var cmu sync.Mutex
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	adv := func(d time.Duration) { cmu.Lock(); now = now.Add(d); cmu.Unlock() }

	d, _, _ := newD(t, &fakeRunner{}, "p")
	d.cfg.Interval = time.Hour
	d.clock = clock
	h := d.Handler()
	if c, _ := get(t, h, "/healthz"); c != 200 {
		t.Fatalf("idle not started: %d", c)
	}
	// Active cycle, fresh heartbeat.
	d.setInCycle(true)
	adv(9 * time.Minute)
	if c, _ := get(t, h, "/healthz"); c != 200 {
		t.Fatalf("active fresh: %d", c)
	}
	// Progress resets.
	d.Progress(engine.ProgressEvent{Pair: "p", Phase: engine.PhaseCopy, Done: 1, Total: 5})
	adv(9 * time.Minute)
	if c, _ := get(t, h, "/healthz"); c != 200 {
		t.Fatalf("after progress: %d", c)
	}
	adv(2 * time.Minute)
	c, body := get(t, h, "/healthz")
	if c != 503 || !strings.Contains(body, "no progress") {
		t.Fatalf("stale: %d %s", c, body)
	}
	// Idle between cycles: fine until interval+10m.
	d.mu.Lock()
	d.started = true
	d.cycleStart = clock()
	d.mu.Unlock()
	d.setInCycle(false)
	adv(time.Hour + 9*time.Minute)
	if c, _ := get(t, h, "/healthz"); c != 200 {
		t.Fatalf("idle fresh: %d", c)
	}
	adv(2 * time.Minute)
	c, body = get(t, h, "/healthz")
	if c != 503 || !strings.Contains(body, "not started a cycle") {
		t.Fatalf("idle stale: %d %s", c, body)
	}
}

func TestReadyzAndStatusAndMetricsEndpoints(t *testing.T) {
	f := &fakeRunner{results: map[string]engine.RunSummary{"p": {Result: engine.ResultOK}}, pfErr: errors.New("immich down")}
	d, _, _ := newD(t, f, "p")
	h := d.Handler()
	d.Once(context.Background()) // preflight fails
	if c, b := get(t, h, "/readyz"); c != 200 || b != "ok" {
		t.Fatalf("readyz %d %s", c, b)
	}
	c, b := get(t, h, "/status")
	if c != 200 {
		t.Fatal(c)
	}
	if strings.Contains(b, "supersecret") {
		t.Fatalf("status leaks secret: %s", b)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(b), &st); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "config", "preflight", "current", "pairs"} {
		if _, ok := st[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if pf := st["preflight"].(map[string]any); pf["ok"] != false || !strings.Contains(fmt.Sprint(pf["error"]), "immich down") {
		t.Errorf("preflight %v", pf)
	}
	f.pfErr = nil
	d.Once(context.Background())
	_, b = get(t, h, "/status")
	if !strings.Contains(b, `"p":{`) {
		t.Errorf("pairs missing: %s", b)
	}
	c, b = get(t, h, "/metrics")
	if c != 200 || !strings.Contains(b, "immich_ferry_runs_total") {
		t.Fatalf("metrics %d", c)
	}
}

func TestRunStopsOnCancelAndServes(t *testing.T) {
	started := make(chan struct{}, 1)
	f := &fakeRunner{results: map[string]engine.RunSummary{"p": {Result: engine.ResultOK}}}
	f.onRunPair = func(ctx context.Context, p engine.Pair) {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	cfg := testCfg("p")
	cfg.Interval = time.Hour
	d := New(cfg, f, prometheus.NewRegistry(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- d.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no cycle")
	}
	cancel()
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunHTTPServing(t *testing.T) {
	// Pick a free port, then run on it.
	ts := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(ts.URL, "http://")
	ts.Close()
	cfg := testCfg("p")
	cfg.HTTPAddr = addr
	cfg.Interval = time.Hour
	d := New(cfg, &fakeRunner{results: map[string]engine.RunSummary{"p": {Result: engine.ResultOK}}}, prometheus.NewRegistry(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(b) != "ok" {
				t.Fatalf("readyz %d %s", resp.StatusCode, b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
}

func TestLoopRetriesPreflightAndRecovers(t *testing.T) {
	f := &fakeRunner{pfErr: errors.New("down"), results: map[string]engine.RunSummary{"p": {Result: engine.ResultOK}}}
	cfg := testCfg("p")
	cfg.Interval = 10 * time.Millisecond
	d := New(cfg, f, prometheus.NewRegistry(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.loop(ctx); close(done) }()
	time.Sleep(40 * time.Millisecond)
	f.mu.Lock()
	if len(f.runs) != 0 {
		t.Error("pairs ran despite failing preflight")
	}
	f.pfErr = nil
	f.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.runs)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pair never ran after preflight recovered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestProgressConcurrent(t *testing.T) {
	d, _, _ := newD(t, &fakeRunner{}, "p")
	h := d.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				d.Progress(engine.ProgressEvent{Pair: "p", Phase: engine.PhaseCopy, Done: j, Total: 200})
				get(t, h, "/healthz")
				get(t, h, "/status")
			}
		}(i)
	}
	wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phase != engine.PhaseCopy || d.heartbeat.IsZero() {
		t.Fatalf("phase=%q hb=%v", d.phase, d.heartbeat)
	}
}

func TestHealthzIdleAfterLongCycle(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var cmu sync.Mutex
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	adv := func(d time.Duration) { cmu.Lock(); now = now.Add(d); cmu.Unlock() }
	d, _, _ := newD(t, &fakeRunner{}, "p")
	d.cfg.Interval = 15 * time.Minute
	d.clock = clock
	h := d.Handler()
	d.mu.Lock()
	d.started = true
	d.cycleStart = clock()
	d.mu.Unlock()
	d.setInCycle(true)
	// 30m cycle (> interval+10m) with steady progress.
	for i := 0; i < 30; i++ {
		adv(time.Minute)
		d.Progress(engine.ProgressEvent{Pair: "p", Phase: engine.PhaseCopy, Done: i, Total: 30})
		if c, _ := get(t, h, "/healthz"); c != 200 {
			t.Fatalf("active %d: %d", i, c)
		}
	}
	d.setInCycle(false)
	d.mu.Lock()
	d.idleWait = 15 * time.Minute
	d.mu.Unlock()
	adv(14 * time.Minute)
	if c, b := get(t, h, "/healthz"); c != 200 {
		t.Fatalf("idle after long cycle: %d %s", c, b)
	}
	adv(12 * time.Minute) // 26m > 15m wait + 10m
	if c, _ := get(t, h, "/healthz"); c != 503 {
		t.Fatalf("idle stale should 503, got %d", c)
	}
}

func TestBeatRefreshesHeartbeat(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var cmu sync.Mutex
	clock := func() time.Time { cmu.Lock(); defer cmu.Unlock(); return now }
	adv := func(d time.Duration) { cmu.Lock(); now = now.Add(d); cmu.Unlock() }
	d, _, _ := newD(t, &fakeRunner{}, "p")
	d.clock = clock
	h := d.Handler()
	d.setInCycle(true)
	for i := 0; i < 6; i++ { // 18m of silent client waiting, beating every 3m
		adv(3 * time.Minute)
		d.Beat()
		if c, _ := get(t, h, "/healthz"); c != 200 {
			t.Fatalf("step %d: %d", i, c)
		}
	}
	adv(11 * time.Minute)
	if c, _ := get(t, h, "/healthz"); c != 503 {
		t.Fatalf("no beats should go stale: %d", c)
	}
}

func TestMetricsFailedRunKeepsSnapshotGauges(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	f := &fakeRunner{results: map[string]engine.RunSummary{"p": {Pair: "p", Result: engine.ResultOK, Finished: now,
		AlbumIDs: [2]string{"x", "y"}, AlbumSizes: [2]int{10, 12}, SnapshotConsistent: [2]bool{true, true}, GuardTripped: [2]bool{true, false}}}}
	d, reg, _ := newD(t, f, "p")
	d.Once(context.Background())
	f.mu.Lock()
	f.results["p"] = engine.RunSummary{Pair: "p", Result: engine.ResultFailed, Finished: now.Add(time.Hour)}
	f.mu.Unlock()
	d.Once(context.Background())
	got := gather(t, reg)
	for k, want := range map[string]float64{
		`immich_ferry_album_assets{pair="p",side="a"}`:          10,
		`immich_ferry_guard_tripped{pair="p",side="a"}`:         1,
		`immich_ferry_snapshot_consistent{pair="p",side="b"}`:   1,
		`immich_ferry_last_success_timestamp_seconds{pair="p"}`: 1_700_000_100,
		`immich_ferry_last_run_timestamp_seconds{pair="p"}`:     1_700_000_100 + 3600,
	} {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: got %v present=%v want %v", k, g, ok, want)
		}
	}
}

func TestMetricsPartialAllFailedNotSuccess(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	f := &fakeRunner{results: map[string]engine.RunSummary{"p": {Pair: "p", Result: engine.ResultPartial, Finished: now, ItemErrorCount: 3}}}
	d, reg, _ := newD(t, f, "p")
	d.Once(context.Background())
	got := gather(t, reg)
	if _, ok := got[`immich_ferry_last_success_timestamp_seconds{pair="p"}`]; ok {
		t.Fatal("partial run with item errors stamped last_success")
	}
	if got[`immich_ferry_last_run_timestamp_seconds{pair="p"}`] != 1_700_000_100 {
		t.Fatalf("last_run missing: %v", got)
	}
}

func TestOnceDiscovery(t *testing.T) {
	disc := func(names ...string) ([]engine.Pair, engine.DiscoveryReport) {
		rep := engine.DiscoveryReport{Mode: engine.DiscoveryAll}
		var ps []engine.Pair
		for _, n := range names {
			ps = append(ps, engine.Pair{Name: n})
			rep.Pairs = append(rep.Pairs, engine.DiscoveredPairInfo{Pair: n})
		}
		return ps, rep
	}
	t.Run("runs discovered pairs and forgets dropped ones", func(t *testing.T) {
		round := 0
		f := &fakeRunner{results: map[string]engine.RunSummary{"album-1": {Result: engine.ResultOK}, "album-2": {Result: engine.ResultOK}}}
		f.pairs = func(static []engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
			if len(static) != 0 {
				t.Errorf("static pairs passed in discovery mode: %v", static)
			}
			round++
			if round == 1 {
				ps, rep := disc("album-1", "album-2")
				rep.Created = []engine.AlbumNote{{Side: "b", AlbumID: "x"}}
				return ps, rep, nil
			}
			ps, rep := disc("album-1")
			rep.Unpaired = []engine.AlbumNote{{Pair: "album-2", Reason: "album no longer selected"}}
			return ps, rep, nil
		}
		d, reg, buf := newD(t, f)
		d.cfg.Engine.Discovery = engine.DiscoveryAll
		if code := d.Once(context.Background()); code != 0 {
			t.Fatalf("code %d\n%s", code, buf)
		}
		if !strings.Contains(buf.String(), `"discovery":{"mode":"all"`) || !strings.Contains(buf.String(), `"pair":"album-2"`) {
			t.Fatalf("output:\n%s", buf)
		}
		got := gather(t, reg)
		if got[`immich_ferry_discovered_pairs{}`] != 2 || got[`immich_ferry_albums_created_total{side="b"}`] != 1 ||
			got[`immich_ferry_runs_total{pair="album-2",result="ok"}`] != 1 {
			t.Fatalf("metrics after round 1: %v", got)
		}
		if code := d.Once(context.Background()); code != 0 {
			t.Fatalf("code %d", code)
		}
		got = gather(t, reg)
		for k := range got {
			if strings.Contains(k, `pair="album-2"`) {
				t.Errorf("series of dropped pair kept: %s", k)
			}
		}
		if got[`immich_ferry_discovered_pairs{}`] != 1 {
			t.Errorf("discovered_pairs %v", got[`immich_ferry_discovered_pairs{}`])
		}
		_, body := get(t, d.Handler(), "/status")
		if strings.Contains(body, `"album-2":{`) || !strings.Contains(body, `"discovery":{"error":""`) {
			t.Errorf("status:\n%s", body)
		}
	})
	t.Run("discovery error fails the cycle", func(t *testing.T) {
		f := &fakeRunner{}
		f.pairs = func([]engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
			return nil, engine.DiscoveryReport{Mode: engine.DiscoveryAll}, errors.New("list albums on side b: boom")
		}
		d, reg, buf := newD(t, f)
		d.cfg.Engine.Discovery = engine.DiscoveryAll
		if code := d.Once(context.Background()); code != 1 {
			t.Fatalf("code %d", code)
		}
		if len(f.runs) != 0 || !strings.Contains(buf.String(), "boom") {
			t.Fatalf("runs %v out %s", f.runs, buf)
		}
		if gather(t, reg)[`immich_ferry_discovery_ok{}`] != 0 {
			t.Error("discovery_ok not 0")
		}
	})
	t.Run("strict fails on skipped albums", func(t *testing.T) {
		f := &fakeRunner{results: map[string]engine.RunSummary{"album-1": {Result: engine.ResultOK}}}
		f.pairs = func([]engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
			ps, rep := disc("album-1")
			rep.Skipped = []engine.AlbumNote{{Side: "a", Name: "Trip", Reason: "several albums named"}}
			return ps, rep, nil
		}
		d, _, _ := newD(t, f)
		d.cfg.Engine.Discovery = engine.DiscoveryAll
		if code := d.Once(context.Background()); code != 0 {
			t.Fatalf("non-strict code %d", code)
		}
		d.SetStrict(true)
		if code := d.Once(context.Background()); code != 1 {
			t.Fatalf("strict code %d", code)
		}
	})
}
