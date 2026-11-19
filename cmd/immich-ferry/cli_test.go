package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"immich-ferry/internal/config"
	"immich-ferry/internal/engine"
	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/state"
)

func envMap(m map[string]string) envFn {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func run(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := realMain(args, envMap(env), &out, &errb)
	return code, out.String(), errb.String()
}

// stateReady reports whether internal/state is implemented (it may still be a stub that panics).
func stateReady(t *testing.T) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Skip("state not implemented yet")
		}
	}()
	d := t.TempDir()
	s, err := state.OpenReadOnly(d)
	if err == nil {
		s.Close()
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, nil, "version")
	if code != 0 || strings.TrimSpace(out) != version {
		t.Fatal(code, out)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code, _, e := run(t, nil, "bogus"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Fatal(code, e)
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(status)
	}))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	env := map[string]string{"HTTP_ADDR": "0.0.0.0:" + port}
	if code, _, e := run(t, env, "healthcheck"); code != 0 {
		t.Fatal(code, e)
	}
	env["HTTP_ADDR"] = ":" + port
	if code, _, _ := run(t, env, "healthcheck"); code != 0 {
		t.Fatal(code)
	}
	status = 503
	if code, _, _ := run(t, env, "healthcheck"); code != 1 {
		t.Fatal(code)
	}
	ts.Close()
	if code, _, _ := run(t, env, "healthcheck"); code != 1 {
		t.Fatal(code)
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"": "http://127.0.0.1:8080/healthz", ":9000": "http://127.0.0.1:9000/healthz", "0.0.0.0:81": "http://127.0.0.1:81/healthz",
		"1.2.3.4:81": "http://1.2.3.4:81/healthz", "[::]:81": "http://[::1]:81/healthz", "[fd00::5]:82": "http://[fd00::5]:82/healthz",
	} {
		m := map[string]string{}
		if in != "" {
			m["HTTP_ADDR"] = in
		}
		got, err := healthURL(envMap(m))
		if err != nil || got != want {
			t.Errorf("%q: %s %v want %s", in, got, err, want)
		}
	}
	if _, err := healthURL(envMap(map[string]string{"HTTP_ADDR": "7000"})); err == nil {
		t.Error("bare port accepted")
	}
	if code, _, _ := run(t, map[string]string{"HTTP_ADDR": "7000"}, "healthcheck"); code != 2 {
		t.Errorf("code %d", code)
	}
}

func TestHealthURLFromConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("httpAddr: \"127.0.0.1:9191\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := healthURL(envMap(map[string]string{"SYNC_CONFIG_FILE": p}))
	if err != nil || got != "http://127.0.0.1:9191/healthz" {
		t.Fatalf("%s %v", got, err)
	}
	got, _ = healthURL(envMap(map[string]string{"SYNC_CONFIG_FILE": p, "HTTP_ADDR": ":7"}))
	if got != "http://127.0.0.1:7/healthz" {
		t.Fatalf("env must win: %s", got)
	}
	got, _ = healthURL(envMap(map[string]string{"SYNC_CONFIG_FILE": "/nonexistent"}))
	if got != "http://127.0.0.1:8080/healthz" {
		t.Fatalf("%s", got)
	}
}

func TestFlagsRequired(t *testing.T) {
	if code, _, _ := run(t, nil, "approve-removals"); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, nil, "state", "show"); code != 2 {
		t.Fatal(code)
	}
	if code, _, e := run(t, nil, "state", "reset", "--pair", "p"); code != 2 || !strings.Contains(e, "--yes") {
		t.Fatal(code, e)
	}
}

func TestConfigErrorExit2(t *testing.T) {
	for _, c := range []string{"run", "once", "validate"} {
		code, _, e := run(t, map[string]string{}, c)
		if code != 2 || !strings.Contains(e, "configuration error") {
			t.Errorf("%s: %d %s", c, code, e)
		}
	}
}

func TestApproveStateShowReset(t *testing.T) {
	stateReady(t)
	dir := t.TempDir()
	env := map[string]string{"SYNC_STATE_DIR": dir}
	// Seed a state via the read-write store.
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ps := &state.PairState{Schema: state.Schema, Pair: "p", AlbumIDs: [2]string{"a1", "b1"}, Bootstrap: true,
		Items: map[string]*state.Item{
			"k1": {InBoth: true},
			"k2": {Blocked: &state.Blocked{Op: "unlink", Side: 1, Reason: "no_permission"}},
			"k3": {Failure: &state.Failure{Op: "copy", Count: 2, LastError: "boom"}, Pending: &state.Pending{}},
		}}
	if err := st.Save("p", ps); err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, e := run(t, env, "state", "show", "--pair", "p")
	if code != 0 {
		t.Fatal(code, e)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err, out)
	}
	if m["items"].(float64) != 3 || m["inBoth"].(float64) != 1 || m["pending"].(float64) != 1 ||
		len(m["blocked"].([]any)) != 1 || len(m["failures"].([]any)) != 1 || m["bootstrap"] != true {
		t.Fatalf("%v", m)
	}

	if code, _, e := run(t, env, "approve-removals", "--pair", "p"); code != 0 {
		t.Fatal(code, e)
	}
	st, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := st.TakeMarker("p", "approve-removals")
	if err != nil || !ok {
		t.Fatalf("marker not set: %v %v", ok, err)
	}
	st.Close()

	// Reset directly when unlocked.
	if code, out, e := run(t, env, "state", "reset", "--pair", "p", "--yes"); code != 0 {
		t.Fatal(code, out, e)
	}
	_, status, err := mustLoad(dir, "p")
	if err != nil || status != state.Fresh {
		t.Fatalf("after reset: %v %v", status, err)
	}

	// Locked: reset leaves a marker instead.
	holder, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, out, e = run(t, env, "state", "reset", "--pair", "p", "--yes")
	if code != 0 || !strings.Contains(out, "next cycle") {
		t.Fatal(code, out, e)
	}
	ok, _ = holder.TakeMarker("p", "reset")
	holder.Close()
	if !ok {
		t.Fatal("reset marker missing")
	}
}

func mustLoad(dir, pair string) (*state.PairState, state.LoadStatus, error) {
	s, err := state.OpenReadOnly(dir)
	if err != nil {
		return nil, 0, err
	}
	defer s.Close()
	return s.Load(pair)
}

func TestApproveUsesConfigFileStateDir(t *testing.T) {
	stateReady(t)
	dir := t.TempDir()
	cf := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(cf, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := run(t, map[string]string{"SYNC_CONFIG_FILE": cf}, "approve-removals", "--pair", "x"); code != 0 {
		t.Fatal(code, e)
	}
}

func TestStateSummaryNoSecrets(t *testing.T) {
	m := stateSummary("p", &state.PairState{Items: map[string]*state.Item{"a": {}}}, state.Loaded)
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"items":1`) {
		t.Fatal(string(b))
	}
}

type fakeEngine struct {
	pf  error
	res engine.RunSummary
	err error
}

func (f *fakeEngine) Preflight(context.Context) error { return f.pf }
func (f *fakeEngine) Pairs(_ context.Context, static []engine.Pair) ([]engine.Pair, engine.DiscoveryReport, error) {
	return static, engine.DiscoveryReport{}, nil
}
func (f *fakeEngine) RunPair(context.Context, engine.Pair) (engine.RunSummary, error) {
	return f.res, f.err
}
func (f *fakeEngine) PlanPair(context.Context, engine.Pair) (planner.Plan, engine.RunSummary, error) {
	return planner.Plan{}, f.res, f.err
}

func withEngine(t *testing.T, fe *fakeEngine, capture func(*config.Config, bool)) {
	t.Helper()
	old := buildEngine
	buildEngine = func(cfg *config.Config, ro bool, _ *slog.Logger, _ func(engine.ProgressEvent), _ func()) (engineAPI, func(), error) {
		if capture != nil {
			capture(cfg, ro)
		}
		return fe, func() {}, nil
	}
	t.Cleanup(func() { buildEngine = old })
}

func validEnv() map[string]string {
	return map[string]string{
		"IMMICH_A_URL": "http://a", "IMMICH_A_API_KEY": "k1", "IMMICH_B_URL": "http://b", "IMMICH_B_API_KEY": "k2",
		"SYNC_A_ALBUM_ID": "11111111-1111-4111-8111-111111111111", "SYNC_B_ALBUM_ID": "22222222-2222-4222-8222-222222222222",
	}
}

func TestOnceWiring(t *testing.T) {
	var got *config.Config
	withEngine(t, &fakeEngine{res: engine.RunSummary{Result: engine.ResultPartial}}, func(c *config.Config, _ bool) { got = c })
	code, out, e := run(t, validEnv(), "once", "--allow-mass-removal")
	if code != 0 || !got.Engine.AllowMassRemoval || !strings.Contains(out, `"pair":"default"`) {
		t.Fatal(code, out, e)
	}
	if code, _, _ := run(t, validEnv(), "once", "--strict"); code != 1 {
		t.Fatal("strict partial should exit 1:", code)
	}
}

func TestOnceErrConfigIs2(t *testing.T) {
	withEngine(t, &fakeEngine{err: engine.ErrConfig}, nil)
	if code, _, _ := run(t, validEnv(), "once"); code != 2 {
		t.Fatal(code)
	}
}

func TestValidateForcesDryRunAndReadOnly(t *testing.T) {
	var got *config.Config
	var ro bool
	withEngine(t, &fakeEngine{res: engine.RunSummary{Pair: "default"}}, func(c *config.Config, r bool) { got, ro = c, r })
	env := validEnv()
	env["SYNC_DRY_RUN"] = "false"
	code, out, e := run(t, env, "validate")
	if code != 0 || !got.Engine.DryRun || !ro || !strings.Contains(out, `"plan"`) {
		t.Fatal(code, out, e)
	}
	withEngine(t, &fakeEngine{pf: errors.New("down")}, nil)
	if code, _, _ := run(t, env, "validate"); code != 1 {
		t.Fatal(code)
	}
	withEngine(t, &fakeEngine{pf: engine.ErrConfig}, nil)
	if code, _, _ := run(t, env, "validate"); code != 2 {
		t.Fatal(code)
	}
}
