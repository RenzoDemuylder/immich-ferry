package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"immich-ferry/internal/config"
	"immich-ferry/internal/engine"
	"immich-ferry/internal/state"
)

const usage = `usage: immich-ferry <command> [flags]

commands:
  run                          run the sync loop (default)
  once [--strict] [--allow-mass-removal]
                               run one cycle and exit (0 ok, 1 failed, 2 config)
  validate                     check config, connectivity and print the plan without changing anything
  healthcheck                  probe the local /healthz endpoint
  version                      print the version
  approve-removals --pair NAME approve a guarded mass removal for the next run
  state show --pair NAME       print a summary of the stored state
  state reset --pair NAME --yes  forget the stored state of a pair
`

type envFn = func(string) (string, bool)

// realMain is the testable entry point. It returns the process exit code.
func realMain(args []string, env envFn, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		return cmdRun(args, env, stdout, stderr)
	case "once":
		return cmdOnce(args, env, stdout, stderr)
	case "validate":
		return cmdValidate(args, env, stdout, stderr)
	case "healthcheck":
		return cmdHealthcheck(env, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "approve-removals":
		return cmdApprove(args, env, stderr, stdout)
	case "state":
		return cmdState(args, env, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// healthURL derives the local healthz URL from HTTP_ADDR (env, then SYNC_CONFIG_FILE httpAddr,
// then :8080); needs no other config. A specific bind IP is probed directly; an empty or
// wildcard host maps to loopback.
func healthURL(env envFn) (string, error) {
	addr := config.HTTPAddr(env, os.ReadFile)
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("invalid HTTP_ADDR %q: want host:port", addr)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func cmdHealthcheck(env envFn, stderr io.Writer) int {
	u, err := healthURL(env)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 2
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Fprintf(stderr, "healthcheck: status %d: %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
		return 1
	}
	return 0
}

// openReadOnlyState resolves the state dir without needing API keys.
func openReadOnlyState(env envFn, stderr io.Writer) (*state.Store, string, bool) {
	dir, err := config.StateDir(env, os.ReadFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, "", false
	}
	st, err := state.OpenReadOnly(dir)
	if err != nil {
		fmt.Fprintf(stderr, "error: open state dir %s: %v\n", dir, err)
		return nil, dir, false
	}
	return st, dir, true
}

func cmdApprove(args []string, env envFn, stderr, stdout io.Writer) int {
	fs := newFlags("approve-removals", stderr)
	pair := fs.String("pair", "", "pair name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pair == "" {
		fmt.Fprintln(stderr, "error: --pair is required")
		return 2
	}
	st, dir, ok := openReadOnlyState(env, stderr)
	if !ok {
		return 1
	}
	defer st.Close()
	if err := st.PutMarker(*pair, "approve-removals"); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "approved: the next run of pair %q (state dir %s) may exceed the removal guard once\n", *pair, dir)
	return 0
}

func cmdState(args []string, env envFn, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "show" && args[0] != "reset") {
		fmt.Fprintf(stderr, "usage: immich-ferry state show|reset --pair NAME [--yes]\n")
		return 2
	}
	sub := args[0]
	fs := newFlags("state "+sub, stderr)
	pair := fs.String("pair", "", "pair name")
	yes := fs.Bool("yes", false, "confirm reset")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *pair == "" {
		fmt.Fprintln(stderr, "error: --pair is required")
		return 2
	}
	if sub == "show" {
		return stateShow(*pair, env, stdout, stderr)
	}
	return stateReset(*pair, *yes, env, stdout, stderr)
}

func stateShow(pair string, env envFn, stdout, stderr io.Writer) int {
	st, _, ok := openReadOnlyState(env, stderr)
	if !ok {
		return 1
	}
	defer st.Close()
	ps, status, err := st.Load(pair)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return writeJSON(stdout, stateSummary(pair, ps, status))
}

func stateReset(pair string, yes bool, env envFn, stdout, stderr io.Writer) int {
	if !yes {
		fmt.Fprintln(stderr, "refusing to reset state without --yes (this forgets sync history; removals will not propagate until re-established)")
		return 2
	}
	dir, err := config.StateDir(env, os.ReadFile)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	st, err := state.Open(dir)
	if err == nil {
		defer st.Close()
		if err := st.Reset(pair); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "state of pair %q reset\n", pair)
		return 0
	}
	if !errors.Is(err, state.ErrLocked) {
		fmt.Fprintf(stderr, "error: open state dir %s: %v\n", dir, err)
		return 1
	}
	ro, err := state.OpenReadOnly(dir)
	if err != nil {
		fmt.Fprintf(stderr, "error: open state dir %s: %v\n", dir, err)
		return 1
	}
	defer ro.Close()
	if err := ro.PutMarker(pair, "reset"); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "state is in use by a running daemon; a reset marker was written and the daemon will reset pair %q at its next cycle\n", pair)
	return 0
}

// stateSummary builds a secret-free summary of a pair state.
func stateSummary(pair string, ps *state.PairState, status state.LoadStatus) map[string]any {
	statusName := map[state.LoadStatus]string{state.Fresh: "fresh", state.Loaded: "loaded",
		state.CorruptMovedAside: "corrupt_moved_aside", state.FingerprintReset: "fingerprint_reset"}[status]
	out := map[string]any{"pair": pair, "loadStatus": statusName}
	if ps == nil {
		return out
	}
	inBoth, pending := 0, 0
	blocked := []map[string]any{}
	failures := []map[string]any{}
	for key, it := range ps.Items {
		if it == nil {
			continue
		}
		if it.InBoth {
			inBoth++
		}
		if it.Pending != nil {
			pending++
		}
		if b := it.Blocked; b != nil {
			blocked = append(blocked, map[string]any{"key": key, "op": b.Op, "side": sideName(b.Side), "reason": b.Reason, "since": b.Since})
		}
		if f := it.Failure; f != nil {
			failures = append(failures, map[string]any{"key": key, "op": f.Op, "count": f.Count, "next": f.Next, "lastError": f.LastError})
		}
	}
	sortByKey(blocked)
	sortByKey(failures)
	out["albumIds"] = ps.AlbumIDs
	out["bootstrap"] = ps.Bootstrap
	out["lastSuccess"] = ps.LastSuccess
	out["items"] = len(ps.Items)
	out["inBoth"] = inBoth
	out["pending"] = pending
	out["blocked"] = blocked
	out["failures"] = failures
	return out
}

func sideName(s uint8) string {
	if s == uint8(engine.A) {
		return "a"
	}
	return "b"
}
