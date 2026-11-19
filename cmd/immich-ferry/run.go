package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync/atomic"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"immich-ferry/internal/config"
	"immich-ferry/internal/daemon"
	"immich-ferry/internal/engine"
	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// engineAPI is what the commands need from the engine (satisfied by *engine.Engine).
type engineAPI interface {
	daemon.Runner
	PlanPair(ctx context.Context, p engine.Pair) (planner.Plan, engine.RunSummary, error)
}

// buildEngineFn constructs the engine and returns a close function.
type buildEngineFn func(cfg *config.Config, readOnly bool, log *slog.Logger, onProgress func(engine.ProgressEvent), beat func()) (engineAPI, func(), error)

// buildEngine is swapped in tests.
var buildEngine buildEngineFn = defaultBuildEngine

func defaultBuildEngine(cfg *config.Config, readOnly bool, log *slog.Logger, onProgress func(engine.ProgressEvent), beat func()) (engineAPI, func(), error) {
	ua := "immich-ferry/" + version
	var cl [2]*immich.Client
	for i := range cl {
		c, err := immich.New(immich.Config{
			BaseURL: cfg.Instances[i].URL, APIKey: cfg.Instances[i].APIKey, UserAgent: ua,
			InsecureTLS: cfg.Instances[i].InsecureTLS, Logger: log.With("side", string(rune('a'+i))),
			Heartbeat: beat,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("%w: instance %c: %v", engine.ErrConfig, 'a'+i, err)
		}
		cl[i] = c
	}
	var st *state.Store
	var err error
	if readOnly {
		st, err = state.OpenReadOnly(cfg.StateDir)
	} else {
		st, err = state.Open(cfg.StateDir)
	}
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			return nil, nil, fmt.Errorf("state directory %s is locked by another process (is another immich-ferry running?): %w", cfg.StateDir, err)
		}
		return nil, nil, fmt.Errorf("open state dir %s: %w", cfg.StateDir, err)
	}
	opts := cfg.Engine
	opts.OnProgress = onProgress
	eng, err := engine.New(opts, cl[0], cl[1], st, log)
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return eng, func() { _ = st.Close() }, nil
}

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	ho := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, ho))
	}
	return slog.New(slog.NewJSONHandler(w, ho))
}

// loadConfig loads and reports config errors (exit code 2).
func loadConfig(env envFn, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.Load(env, os.ReadFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, false
	}
	return cfg, true
}

func exitCodeFor(err error) int {
	if errors.Is(err, engine.ErrConfig) {
		return 2
	}
	return 1
}

// signalContext cancels on the first SIGINT/SIGTERM and exits 130 immediately on the second.
func signalContext(log *slog.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-ch
		log.Info("shutdown requested, finishing in-flight work (send again to exit immediately)", "signal", s.String())
		cancel()
		<-ch
		log.Warn("second signal received, exiting now")
		os.Exit(130)
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func newDaemon(cfg *config.Config, eng daemon.Runner, log *slog.Logger) *daemon.Daemon {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	d := daemon.New(cfg, eng, reg, log)
	d.SetGatherer(reg)
	return d
}

func cmdRun(args []string, env envFn, stdout, stderr io.Writer) int {
	if err := newFlags("run", stderr).Parse(args); err != nil {
		return 2
	}
	cfg, ok := loadConfig(env, stderr)
	if !ok {
		return 2
	}
	log := newLogger(cfg, stderr)
	daemon.Version = version
	var dp atomic.Pointer[daemon.Daemon]
	eng, closeFn, err := buildEngine(cfg, false, log, func(ev engine.ProgressEvent) {
		if d := dp.Load(); d != nil {
			d.Progress(ev)
		}
	}, func() {
		if d := dp.Load(); d != nil {
			d.Beat()
		}
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCodeFor(err)
	}
	defer closeFn()
	d := newDaemon(cfg, eng, log)
	dp.Store(d)
	ctx, stop := signalContext(log)
	defer stop()
	log.Info("immich-ferry starting", "version", version, "pairs", len(cfg.Pairs), "discovery", string(cfg.Engine.Discovery),
		"interval", cfg.Interval.String())
	if err := d.Run(ctx); err != nil {
		log.Error("daemon stopped", "err", err)
		return 1
	}
	log.Info("immich-ferry stopped")
	return 0
}

func cmdOnce(args []string, env envFn, stdout, stderr io.Writer) int {
	fs := newFlags("once", stderr)
	strict := fs.Bool("strict", false, "treat partial runs as failure (exit 1)")
	allow := fs.Bool("allow-mass-removal", false, "bypass the mass-removal guard for this run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := loadConfig(env, stderr)
	if !ok {
		return 2
	}
	cfg.Engine.AllowMassRemoval = *allow
	log := newLogger(cfg, stderr)
	daemon.Version = version
	var dp atomic.Pointer[daemon.Daemon]
	eng, closeFn, err := buildEngine(cfg, false, log, func(ev engine.ProgressEvent) {
		if d := dp.Load(); d != nil {
			d.Progress(ev)
		}
	}, func() {
		if d := dp.Load(); d != nil {
			d.Beat()
		}
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCodeFor(err)
	}
	defer closeFn()
	d := newDaemon(cfg, eng, log)
	d.SetStrict(*strict)
	d.SetOutput(stdout)
	dp.Store(d)
	ctx, stop := signalContext(log)
	defer stop()
	return d.Once(ctx)
}

func cmdValidate(args []string, env envFn, stdout, stderr io.Writer) int {
	if err := newFlags("validate", stderr).Parse(args); err != nil {
		return 2
	}
	cfg, ok := loadConfig(env, stderr)
	if !ok {
		return 2
	}
	cfg.Engine.DryRun = true
	cfg.Engine.AllowMassRemoval = false
	log := newLogger(cfg, stderr)
	eng, closeFn, err := buildEngine(cfg, true, log, nil, nil)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCodeFor(err)
	}
	defer closeFn()
	ctx, stop := signalContext(log)
	defer stop()
	if err := eng.Preflight(ctx); err != nil {
		fmt.Fprintln(stderr, "preflight failed:", err)
		var pe *engine.PreflightError
		if errors.Is(err, engine.ErrConfig) || (errors.As(err, &pe) && len(pe.Missing) > 0) {
			return 2
		}
		return 1
	}
	code := 0
	pairs, rep, err := eng.Pairs(ctx, cfg.Pairs)
	if cfg.Engine.Discovery != engine.DiscoveryOff {
		out := map[string]any{"discovery": rep}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(stdout, out)
	}
	if err != nil {
		fmt.Fprintln(stderr, "album discovery failed:", err)
		return exitCodeFor(err)
	}
	for _, p := range pairs {
		plan, sum, err := eng.PlanPair(ctx, p)
		out := map[string]any{"pair": p.Name, "plan": planSummary(plan), "summary": sum}
		if err != nil {
			out["error"] = err.Error()
			if c := exitCodeFor(err); c > code {
				code = c
			}
		}
		writeJSON(stdout, out)
	}
	if code == 0 {
		fmt.Fprintln(stderr, "validation ok: configuration, connectivity and permissions verified (nothing was changed)")
	}
	return code
}

func planSummary(p planner.Plan) map[string]any {
	return map[string]any{
		"copies": len(p.Copies), "removals": len(p.Removals), "metadata": len(p.Meta), "albumInfo": len(p.AlbumInfo),
		"guardTripped": p.GuardTripped, "warnings": p.Warnings, "pending": p.Pending, "skipped": p.Skipped,
	}
}

func writeJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}

func sortByKey(l []map[string]any) {
	sort.Slice(l, func(i, j int) bool { return fmt.Sprint(l[i]["key"]) < fmt.Sprint(l[j]["key"]) })
}
