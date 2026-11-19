// Package daemon runs the sync loop and serves /healthz /readyz /metrics /status.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"immich-ferry/internal/config"
	"immich-ferry/internal/engine"
)

// Version is reported by /status; set by the CLI.
var Version = "dev"

// stallLimit is how long an active cycle may go without a progress event before /healthz fails
// (max(10m, 3x the client stall timeout of ~2m)).
const stallLimit = 10 * time.Minute

// Runner is the engine surface the daemon needs (satisfied by *engine.Engine).
type Runner interface {
	Preflight(ctx context.Context) error
	// Pairs returns the pairs to sync this cycle (the configured ones, or discovered albums).
	Pairs(ctx context.Context, static []engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error)
	RunPair(ctx context.Context, p engine.Pair) (engine.RunSummary, error)
}

// Daemon is the long-running loop plus HTTP endpoints.
type Daemon struct {
	cfg *config.Config
	r   Runner
	log *slog.Logger
	m   *metrics
	reg prometheus.Gatherer

	clock  func() time.Time
	out    io.Writer
	strict bool

	mu         sync.Mutex
	started    bool // Run loop active
	inCycle    bool
	heartbeat  time.Time
	cycleStart time.Time     // last time the loop began an iteration
	idleSince  time.Time     // when the last cycle ended
	idleWait   time.Duration // planned sleep after the last cycle
	curPair    string
	phase      engine.Phase
	done       int
	total      int
	pfOK       bool
	pfSet      bool
	pfErr      string
	summaries  map[string]engine.RunSummary
	runErrs    map[string]string
	discovery  *engine.DiscoveryReport // last discovery report (discovery enabled only)
	discErr    string
}

// New creates a Daemon and registers its metrics on reg.
func New(cfg *config.Config, r Runner, reg prometheus.Registerer, log *slog.Logger) *Daemon {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d := &Daemon{
		cfg: cfg, r: r, log: log, m: newMetrics(reg), clock: time.Now, out: os.Stdout,
		summaries: map[string]engine.RunSummary{}, runErrs: map[string]string{},
	}
	if g, ok := reg.(prometheus.Gatherer); ok {
		d.reg = g
	}
	return d
}

// SetStrict makes a partial run count as failure (exit 1) in Once.
func (d *Daemon) SetStrict(v bool) { d.strict = v }

// SetOutput sets where Once prints per-pair JSON summaries (default stdout).
func (d *Daemon) SetOutput(w io.Writer) { d.out = w }

// SetGatherer sets the gatherer served on /metrics (defaults to the registerer when it is also a Gatherer).
func (d *Daemon) SetGatherer(g prometheus.Gatherer) { d.reg = g }

// Progress records a progress event (heartbeat for /healthz). Non-blocking and concurrency-safe.
func (d *Daemon) Progress(ev engine.ProgressEvent) {
	now := d.clock()
	d.mu.Lock()
	d.heartbeat = now
	d.curPair = ev.Pair
	d.phase = ev.Phase
	d.done, d.total = ev.Done, ev.Total
	d.mu.Unlock()
}

// Beat refreshes the /healthz heartbeat without changing the reported progress. The
// Immich clients call it while they legitimately wait (long upload responses,
// Retry-After sleeps), so a slow-but-alive cycle is not killed by the liveness probe.
func (d *Daemon) Beat() {
	now := d.clock()
	d.mu.Lock()
	d.heartbeat = now
	d.mu.Unlock()
}

func (d *Daemon) beat(pair string, ph engine.Phase) {
	d.mu.Lock()
	d.heartbeat = d.clock()
	d.curPair, d.phase, d.done, d.total = pair, ph, 0, 0
	d.mu.Unlock()
}

func (d *Daemon) setInCycle(v bool) {
	d.mu.Lock()
	d.inCycle = v
	d.heartbeat = d.clock()
	if !v {
		d.idleSince, d.idleWait = d.heartbeat, d.cfg.Interval
		d.curPair, d.phase, d.done, d.total = "", "", 0, 0
	}
	d.mu.Unlock()
}

func (d *Daemon) doPreflight(ctx context.Context) error {
	d.beat("", engine.PhasePreflight)
	err := d.r.Preflight(ctx)
	if ctx.Err() != nil && err != nil {
		return err
	}
	d.mu.Lock()
	d.pfSet = true
	d.pfOK = err == nil
	if err != nil {
		d.pfErr = err.Error()
	} else {
		d.pfErr = ""
	}
	d.mu.Unlock()
	if err == nil {
		d.m.setPreflight(true, true)
		return nil
	}
	var pe *engine.PreflightError
	if errors.As(err, &pe) {
		d.m.setPreflight(pe.Side != engine.A, pe.Side != engine.B)
	} else {
		d.m.setPreflight(false, false)
	}
	d.log.Error("preflight failed", "op", "preflight", "err", err)
	return err
}

func (d *Daemon) doPair(ctx context.Context, p engine.Pair) (engine.RunSummary, error) {
	d.beat(p.Name, engine.PhasePreflight)
	s, err := d.r.RunPair(ctx, p)
	if s.Pair == "" {
		s.Pair = p.Name
	}
	if ctx.Err() != nil && err != nil {
		d.log.Info("pair run interrupted", "pair", p.Name, "err", err)
		return s, err
	}
	d.m.observe(p.Name, s, err != nil)
	d.mu.Lock()
	d.summaries[p.Name] = s
	if err != nil {
		d.runErrs[p.Name] = err.Error()
	} else {
		delete(d.runErrs, p.Name)
	}
	d.mu.Unlock()
	if err != nil {
		d.log.Error("pair run failed", "pair", p.Name, "op", "run", "err", err)
	} else {
		d.log.Info("pair run finished", "pair", p.Name, "result", string(s.Result), "mutations", s.Mutations,
			"copied_a_to_b", s.Copied.AtoB, "copied_b_to_a", s.Copied.BtoA)
	}
	return s, err
}

// doPairs resolves this cycle's pairs. With discovery it records the report, and forgets
// status and metrics of pairs that are no longer synced.
func (d *Daemon) doPairs(ctx context.Context) ([]engine.Pair, engine.DiscoveryReport, error) {
	if d.cfg.Engine.Discovery != engine.DiscoveryOff {
		d.beat("", engine.PhaseDiscovery)
	}
	pairs, rep, err := d.r.Pairs(ctx, d.cfg.Pairs)
	if d.cfg.Engine.Discovery == engine.DiscoveryOff {
		return pairs, rep, err
	}
	if err != nil && ctx.Err() != nil {
		return nil, rep, err
	}
	d.m.observeDiscovery(rep, err)
	if err != nil {
		d.log.Error("album discovery failed", "op", "discovery", "err", err)
		d.mu.Lock()
		d.discErr = err.Error()
		d.mu.Unlock()
		return nil, rep, err
	}
	active := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		active[p.Name] = true
	}
	d.mu.Lock()
	d.discovery, d.discErr = &rep, ""
	var gone []string
	for name := range d.summaries {
		if !active[name] {
			gone = append(gone, name)
		}
	}
	for name := range d.runErrs {
		if !active[name] {
			gone = append(gone, name)
		}
	}
	for _, name := range gone {
		delete(d.summaries, name)
		delete(d.runErrs, name)
	}
	d.mu.Unlock()
	for _, n := range rep.Unpaired {
		gone = append(gone, n.Pair)
	}
	for _, name := range gone {
		d.m.forgetPair(name)
	}
	return pairs, rep, nil
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run runs the loop until ctx is canceled. It returns nil on clean shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	d.log.Info("http server listening", "addr", ln.Addr().String())

	d.mu.Lock()
	d.started = true
	d.cycleStart = d.clock()
	d.heartbeat = d.cycleStart
	d.mu.Unlock()

	lctx, stop := context.WithCancel(ctx)
	defer stop()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		d.loop(lctx)
	}()
	var serveErr error
	select {
	case <-loopDone:
	case serveErr = <-srvErr:
		// Server died unexpectedly: stop the loop and report.
		stop()
	}
	<-loopDone
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

func (d *Daemon) loop(ctx context.Context) {
	for ctx.Err() == nil {
		d.mu.Lock()
		d.cycleStart = d.clock()
		d.mu.Unlock()
		d.setInCycle(true)
		wait := jitter(d.cfg.Interval)
		if err := d.doPreflight(ctx); err != nil {
			wait = min(d.cfg.Interval, time.Minute)
		} else if pairs, _, err := d.doPairs(ctx); err != nil {
			wait = min(d.cfg.Interval, time.Minute)
		} else {
			for _, p := range pairs {
				if ctx.Err() != nil {
					break
				}
				d.doPair(ctx, p)
			}
		}
		d.setInCycle(false)
		d.mu.Lock()
		d.idleWait = wait
		d.mu.Unlock()
		sleep(ctx, wait)
	}
}

// Once runs a single cycle for all pairs and returns the process exit code.
func (d *Daemon) Once(ctx context.Context) int {
	d.setInCycle(true)
	defer d.setInCycle(false)
	if err := d.doPreflight(ctx); err != nil {
		var pe *engine.PreflightError
		if errors.Is(err, engine.ErrConfig) || (errors.As(err, &pe) && len(pe.Missing) > 0) {
			return 2
		}
		return 1
	}
	code := 0
	bump := func(c int) {
		if c == 2 || (c == 1 && code == 0) {
			code = c
		}
	}
	enc := json.NewEncoder(d.out)
	pairs, rep, err := d.doPairs(ctx)
	if d.cfg.Engine.Discovery != engine.DiscoveryOff {
		line := map[string]any{"discovery": rep}
		if err != nil {
			line["error"] = err.Error()
		}
		_ = enc.Encode(line)
	}
	if err != nil {
		if errors.Is(err, engine.ErrConfig) {
			return 2
		}
		return 1
	}
	if rep.Problems() && d.strict {
		bump(1)
	}
	for _, p := range pairs {
		if ctx.Err() != nil {
			bump(1)
			break
		}
		s, err := d.doPair(ctx, p)
		line := map[string]any{"pair": p.Name, "result": string(s.Result), "summary": s}
		switch {
		case err != nil:
			line["error"] = err.Error()
			if s.Result == "" {
				line["result"] = string(engine.ResultFailed)
			}
			if errors.Is(err, engine.ErrConfig) {
				bump(2)
			} else {
				bump(1)
			}
		case s.Result == engine.ResultFailed:
			bump(1)
		case s.Result == engine.ResultPartial && d.strict:
			bump(1)
		}
		_ = enc.Encode(line)
	}
	return code
}

// Handler returns the HTTP handler (/healthz /readyz /metrics /status).
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", d.healthz)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/status", d.status)
	if d.reg != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(d.reg, promhttp.HandlerOpts{}))
	}
	return mux
}

func (d *Daemon) healthz(w http.ResponseWriter, _ *http.Request) {
	now := d.clock()
	d.mu.Lock()
	reason := ""
	switch {
	case d.inCycle && now.Sub(d.heartbeat) > stallLimit:
		reason = fmt.Sprintf("no progress for %s in active cycle (pair=%q phase=%q)", now.Sub(d.heartbeat).Round(time.Second), d.curPair, d.phase)
	case d.started && !d.inCycle && !d.idleSince.IsZero() && now.Sub(d.idleSince) > d.idleWait+stallLimit:
		// Measured from when the last cycle ended, not from when it began: a long cycle must not
		// make the following sleep look stale.
		reason = fmt.Sprintf("loop has not started a cycle for %s (planned wait %s)", now.Sub(d.idleSince).Round(time.Second), d.idleWait.Round(time.Second))
	case d.started && !d.inCycle && d.idleSince.IsZero() && now.Sub(d.cycleStart) > d.cfg.Interval+stallLimit:
		reason = fmt.Sprintf("loop has not started a cycle for %s", now.Sub(d.cycleStart).Round(time.Second))
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	if reason != "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unhealthy: "+reason)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

func (d *Daemon) status(w http.ResponseWriter, _ *http.Request) {
	now := d.clock()
	d.mu.Lock()
	cur := map[string]any{"pair": d.curPair, "phase": string(d.phase), "done": d.done, "total": d.total, "sinceProgress": ""}
	if d.inCycle {
		cur["sinceProgress"] = now.Sub(d.heartbeat).Round(time.Millisecond).String()
	}
	pf := map[string]any{"ok": d.pfOK, "error": d.pfErr, "checked": d.pfSet}
	pairs := make(map[string]any, len(d.summaries))
	for k, v := range d.summaries {
		pairs[k] = v
	}
	errs := make(map[string]string, len(d.runErrs))
	for k, v := range d.runErrs {
		errs[k] = v
	}
	out := map[string]any{
		"version": Version, "config": config.Redacted(d.cfg), "preflight": pf, "current": cur, "pairs": pairs, "errors": errs,
	}
	if d.cfg.Engine.Discovery != engine.DiscoveryOff {
		out["discovery"] = map[string]any{"last": d.discovery, "error": d.discErr}
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
