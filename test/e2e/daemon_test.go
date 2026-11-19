//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/engine"
	"immich-ferry/test/e2e/testutil"
)

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..")
}

type composeEnv struct {
	t   *testing.T
	env []string
}

// dc runs `docker compose -f compose.yaml --profile daemon <args>` from the repo root.
func (c *composeEnv) dc(timeout time.Duration, args ...string) (string, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"compose", "-f", filepath.Join(repoRoot(), "test", "e2e", "compose.yaml"), "--profile", "daemon"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), c.env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func (c *composeEnv) mustDC(timeout time.Duration, args ...string) string {
	c.t.Helper()
	out, err := c.dc(timeout, args...)
	if err != nil {
		c.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, tail(out, 3000))
	}
	return out
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

func (c *composeEnv) containerID() string {
	c.t.Helper()
	out := c.mustDC(30*time.Second, "ps", "-a", "-q", "immich-ferry")
	id := strings.TrimSpace(out)
	if id == "" {
		c.t.Fatalf("no immich-ferry container")
	}
	return strings.Fields(id)[0]
}

func (c *composeEnv) exitCode() string {
	c.t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.ExitCode}}|{{.State.Status}}", c.containerID()).CombinedOutput()
	if err != nil {
		c.t.Fatalf("docker inspect: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (c *composeEnv) logs() string {
	out, _ := c.dc(30*time.Second, "logs", "--no-color", "--tail", "80", "immich-ferry")
	return out
}

func httpGet(url string) (int, string, error) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func waitReady(t *testing.T, c *composeEnv, base string) {
	t.Helper()
	testutil.Eventually(t, 3*time.Minute, time.Second, func() (bool, string) {
		code, body, err := httpGet(base + "/readyz")
		if err != nil {
			return false, err.Error()
		}
		if code != 200 {
			return false, fmt.Sprintf("readyz %d %s", code, body)
		}
		return true, ""
	})
}

func getStatus(base string) (*statusDoc, string, error) {
	code, body, err := httpGet(base + "/status")
	if err != nil {
		return nil, "", err
	}
	if code != 200 {
		return nil, body, fmt.Errorf("status %d", code)
	}
	var d statusDoc
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return nil, body, err
	}
	return &d, body, nil
}

func TestDaemonContainer(t *testing.T) {
	instA, instB := e2eInstances(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx := context.Background()
	port := os.Getenv("SYNC_PORT")
	if port == "" {
		port = "22880"
	}
	base := "http://127.0.0.1:" + port

	opts := defaultOpts(t)
	perms := engine.RequiredPermissions(opts)
	ua, err := testutil.NewUser(ctx, instA, "daemon-a")
	if err != nil {
		t.Fatal(err)
	}
	ub, err := testutil.NewUser(ctx, instB, "daemon-b")
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := ua.Key(ctx, perms)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ub.Key(ctx, perms)
	if err != nil {
		t.Fatal(err)
	}
	n := testutil.UniqueSeed()
	nameA, nameB := fmt.Sprintf("daemon-a-%d", n), fmt.Sprintf("daemon-b-%d", n)
	seed0 := photo(t, ua, "daemon-seed.jpg", t0)
	albA, err := ua.API.CreateAlbum(ctx, nameA, "daemon", seed0.ID)
	if err != nil {
		t.Fatal(err)
	}
	albB, err := ub.API.CreateAlbum(ctx, nameB, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &composeEnv{t: t, env: []string{
		"E2E_A_KEY=" + keyA, "E2E_B_KEY=" + keyB,
		"E2E_DAEMON_ALBUM_A=" + albA, "E2E_DAEMON_ALBUM_B=" + albB, "E2E_DAEMON_INTERVAL=10s",
	}}

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon logs:\n%s", c.logs())
		}
		_, _ = c.dc(2*time.Minute, "rm", "-sf", "immich-ferry")
		_, _ = exec.Command("docker", "volume", "rm", "-f", "immich-ferry-e2e_sync-data").CombinedOutput()
	})
	// A leftover container/volume of an earlier run must not leak state into this one.
	_, _ = c.dc(2*time.Minute, "rm", "-sf", "immich-ferry")
	_, _ = exec.Command("docker", "volume", "rm", "-f", "immich-ferry-e2e_sync-data").CombinedOutput()

	t.Log("building image")
	c.mustDC(15*time.Minute, "build", "immich-ferry")
	c.mustDC(3*time.Minute, "up", "-d", "--no-build", "immich-ferry")
	waitReady(t, c, base)

	// initial seed copy, then a fresh asset
	testutil.WaitAlbumHas(t, ub.API, albB, seed0.Sum, 90*time.Second)
	fresh := photo(t, ua, "daemon-fresh.jpg", t0.Add(time.Hour))
	if _, err := ua.API.AddToAlbum(ctx, albA, fresh.ID); err != nil {
		t.Fatal(err)
	}
	got := testutil.WaitAlbumHas(t, ub.API, albB, fresh.Sum, 90*time.Second)
	if got.OriginalFileName != "daemon-fresh.jpg" || got.OwnerID != ub.ID {
		t.Errorf("synced asset: %+v", got)
	}

	// endpoints
	if code, _, err := httpGet(base + "/healthz"); err != nil || code != 200 {
		t.Errorf("/healthz: %d %v", code, err)
	}
	var metrics string
	testutil.Eventually(t, 30*time.Second, time.Second, func() (bool, string) {
		code, body, err := httpGet(base + "/metrics")
		if err != nil || code != 200 {
			return false, fmt.Sprint(code, err)
		}
		metrics = body
		return strings.Contains(body, "immich_ferry_runs_total") && strings.Contains(body, "immich_ferry_assets_copied_total"), "metrics missing series"
	})
	_ = metrics
	st, raw, err := getStatus(base)
	if err != nil || len(st.Pairs) == 0 {
		t.Errorf("/status shows no pair: %v\n%s", err, tail(raw, 1500))
	}

	// graceful stop of an idle daemon
	stopAndCheckExit(t, c)

	// restart: state is persisted in the volume, so the first cycle is a no-op
	restart := time.Now().Add(-2 * time.Second)
	c.mustDC(2*time.Minute, "start", "immich-ferry")
	waitReady(t, c, base)
	waitNoopCycle(t, base, restart, true)

	// stop right after adding a large asset, then converge after restart
	bigData := append(testutil.MP4(testutil.UniqueSeed()), testutil.FreeBox(30<<20)...)
	big := upload(t, ua, "daemon-big.mp4", bigData, testutil.UploadOpts{FileCreatedAt: t0.Add(2 * time.Hour)})
	if _, err := ua.API.AddToAlbum(ctx, albA, big.ID); err != nil {
		t.Fatal(err)
	}
	// Try to catch the daemon inside the copy phase (best effort), then stop.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s, _, err := getStatus(base); err == nil && s.Current.Phase == "copy" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	stopAndCheckExit(t, c)
	restart = time.Now().Add(-2 * time.Second)
	c.mustDC(2*time.Minute, "start", "immich-ferry")
	waitReady(t, c, base)
	testutil.WaitAlbumHas(t, ub.API, albB, big.Sum, 120*time.Second)
	waitNoopCycle(t, base, restart, false)
}

func stopAndCheckExit(t *testing.T, c *composeEnv) {
	t.Helper()
	start := time.Now()
	c.mustDC(90*time.Second, "stop", "immich-ferry")
	d := time.Since(start)
	t.Logf("stop took %s", d)
	if d >= 60*time.Second {
		t.Errorf("stop took %s, want < 60s", d)
	}
	if got := c.exitCode(); !strings.HasPrefix(got, "0|") {
		t.Errorf("container exit state %q, want exit code 0\nlogs:\n%s", got, c.logs())
	}
}

// waitNoopCycle waits for a cycle that started after `since` and expects it to be a clean no-op.
func waitNoopCycle(t *testing.T, base string, since time.Time, strict bool) {
	t.Helper()
	var last string
	testutil.Eventually(t, 90*time.Second, time.Second, func() (bool, string) {
		s, raw, err := getStatus(base)
		if err != nil {
			return false, err.Error()
		}
		last = raw
		for name, p := range s.Pairs {
			if p.Started.After(since) && !p.Finished.IsZero() && p.Pending == 0 {
				if p.Mutations != 0 {
					// the first cycle after a restart may still finish pending work of the previous run; look at the next one
					if strict {
						t.Fatalf("first cycle after restart: pair %s mutations=%d result=%s (state not persisted?)", name, p.Mutations, p.Result)
					}
					since = p.Started
					return false, fmt.Sprintf("pair %s: mutations=%d result=%s", name, p.Mutations, p.Result)
				}
				return true, ""
			}
		}
		return false, "no completed cycle since restart yet"
	})
	_ = last
}
