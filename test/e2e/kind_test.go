//go:build e2e_kind

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/engine"
	"immich-ferry/test/e2e/testutil"
)

const (
	kindCluster = "immich-ferry-e2e"
	kindCtx     = "kind-immich-ferry-e2e"
	kindNS      = "immich-ferry"
	kindImage   = "e2e-kind"
)

func kroot() string {
	d, _ := os.Getwd()
	return filepath.Join(d, "..", "..")
}

// kindChart is the immich-ferry chart, maintained in github.com/RenzoDemuylder/helm-charts (charts/immich-ferry).
// IMMICH_FERRY_CHART overrides the default, a helm-charts checkout next to this repository.
func kindChart() string {
	if c := os.Getenv("IMMICH_FERRY_CHART"); c != "" {
		return c
	}
	return filepath.Join(kroot(), "..", "helm-charts", "charts", "immich-ferry")
}

func sh(t *testing.T, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = kroot()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func mustSh(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	out, err := sh(t, timeout, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, kindTail(out))
	}
	return out
}

func kindTail(s string) string {
	if len(s) > 3000 {
		return "..." + s[len(s)-3000:]
	}
	return s
}

func kubectl(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	return sh(t, timeout, "kubectl", append([]string{"--context", kindCtx}, args...)...)
}

func TestKindHelmDeploy(t *testing.T) {
	env := testutil.E2EEnv()
	if env["IMMICH_A_URL"] == "" {
		t.Skip("run `make e2e-up` first")
	}
	ctx := context.Background()
	instA, err := testutil.Bootstrap(ctx, env["IMMICH_A_URL"])
	if err != nil {
		t.Fatal(err)
	}
	instB, err := testutil.Bootstrap(ctx, env["IMMICH_B_URL"])
	if err != nil {
		t.Fatal(err)
	}

	// cluster
	clusters := mustSh(t, time.Minute, "kind", "get", "clusters")
	existed := false
	for _, c := range strings.Fields(clusters) {
		if c == kindCluster {
			existed = true
		}
	}
	keep := os.Getenv("KEEP_CLUSTER") == "1"
	var connected []string
	t.Cleanup(func() {
		_, _ = sh(t, 3*time.Minute, "helm", "--kube-context", kindCtx, "uninstall", "sync", "-n", kindNS)
		for _, c := range connected {
			_, _ = sh(t, time.Minute, "docker", "network", "disconnect", "kind", c)
		}
		if !keep {
			_, _ = sh(t, 5*time.Minute, "kind", "delete", "cluster", "--name", kindCluster)
		}
	})
	if !existed {
		mustSh(t, 6*time.Minute, "kind", "create", "cluster", "--name", kindCluster, "--wait", "120s")
	}
	mustSh(t, 15*time.Minute, "docker", "build", "-t", "immich-ferry:"+kindImage, ".")
	mustSh(t, 5*time.Minute, "kind", "load", "docker-image", "immich-ferry:"+kindImage, "--name", kindCluster)

	// network wiring: the Immich containers must be reachable from the kind nodes
	ip := func(svc string) string {
		id := strings.Fields(mustSh(t, time.Minute, "docker", "compose", "-f", "test/e2e/compose.yaml", "ps", "-q", svc))
		if len(id) == 0 {
			t.Fatalf("no container for %s", svc)
		}
		out, _ := sh(t, time.Minute, "docker", "network", "connect", "kind", id[0])
		if out != "" && !strings.Contains(out, "already exists") {
			t.Fatalf("network connect: %s", out)
		}
		connected = append(connected, id[0])
		return strings.TrimSpace(mustSh(t, time.Minute, "docker", "inspect", "-f", `{{(index .NetworkSettings.Networks "kind").IPAddress}}`, id[0]))
	}
	ipA, ipB := ip("immich-a"), ip("immich-b")
	t.Logf("immich A %s, B %s on the kind network", ipA, ipB)

	// users, keys, albums
	opts := defaultOpts(t)
	perms := engine.RequiredPermissions(opts)
	ua, err := testutil.NewUser(ctx, instA, "kind-a")
	if err != nil {
		t.Fatal(err)
	}
	ub, err := testutil.NewUser(ctx, instB, "kind-b")
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
	nameA, nameB := fmt.Sprintf("kind-a-%d", n), fmt.Sprintf("kind-b-%d", n)
	seed0 := photo(t, ua, "kind-seed.jpg", t0)
	albA, err := ua.API.CreateAlbum(ctx, nameA, "", seed0.ID)
	if err != nil {
		t.Fatal(err)
	}
	albB, err := ub.API.CreateAlbum(ctx, nameB, "")
	if err != nil {
		t.Fatal(err)
	}

	chart := kindChart()
	if _, err := os.Stat(filepath.Join(chart, "Chart.yaml")); err != nil {
		t.Skipf("immich-ferry chart not found at %s (clone github.com/RenzoDemuylder/helm-charts next to this repo or set IMMICH_FERRY_CHART): %v", chart, err)
	}
	mustSh(t, 2*time.Minute, "helm", "dependency", "build", chart)
	// the image loaded into kind is docker.io/library/immich-ferry:<kindImage>
	mustSh(t, 8*time.Minute, "helm", "upgrade", "--install", "sync", chart, "--kube-context", kindCtx,
		"-n", kindNS, "--create-namespace",
		"--set", "image.registry=docker.io", "--set", "image.repository=library/immich-ferry",
		"--set", "image.tag="+kindImage, "--set", "image.pullPolicy=IfNotPresent",
		"--set-string", "immich.a.url=http://"+ipA+":2283", "--set-string", "immich.b.url=http://"+ipB+":2283",
		"--set-string", "immich.a.apiKey="+keyA, "--set-string", "immich.b.apiKey="+keyB,
		"--set-string", "album.pairName=kind", "--set-string", "album.a.id="+albA, "--set-string", "album.b.id="+albB,
		"--set", "sync.interval=10s", "--set", "persistence.size=100Mi", "--set", "log.format=text",
		"--wait", "--timeout", "5m")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := kubectl(t, time.Minute, "-n", kindNS, "logs", "-l", "app.kubernetes.io/instance=sync", "--tail", "60")
			d, _ := kubectl(t, time.Minute, "-n", kindNS, "describe", "pods")
			t.Logf("pod logs:\n%s\ndescribe:\n%s", out, kindTail(d))
		}
	})
	mustSh(t, 5*time.Minute, "kubectl", "--context", kindCtx, "-n", kindNS, "rollout", "status", "deploy/sync-immich-ferry", "--timeout", "4m")

	testutil.WaitAlbumHas(t, ub.API, albB, seed0.Sum, 2*time.Minute)
	fresh := photo(t, ua, "kind-fresh.jpg", t0.Add(time.Hour))
	if _, err := ua.API.AddToAlbum(ctx, albA, fresh.ID); err != nil {
		t.Fatal(err)
	}
	testutil.WaitAlbumHas(t, ub.API, albB, fresh.Sum, 2*time.Minute)

	// delete the pod: the PVC keeps the state, the new pod's first cycle is a no-op
	oldPod := strings.TrimSpace(mustSh(t, time.Minute, "kubectl", "--context", kindCtx, "-n", kindNS, "get", "pod",
		"-l", "app.kubernetes.io/instance=sync", "-o", "jsonpath={.items[0].metadata.name}"))
	mustSh(t, 3*time.Minute, "kubectl", "--context", kindCtx, "-n", kindNS, "delete", "pod", oldPod, "--wait=true")
	mustSh(t, 5*time.Minute, "kubectl", "--context", kindCtx, "-n", kindNS, "rollout", "status", "deploy/sync-immich-ferry", "--timeout", "4m")
	newPod := ""
	testutil.Eventually(t, time.Minute, 2*time.Second, func() (bool, string) {
		out, _ := kubectl(t, time.Minute, "-n", kindNS, "get", "pod", "-l", "app.kubernetes.io/instance=sync", "-o", "jsonpath={.items[*].metadata.name}")
		for _, p := range strings.Fields(out) {
			if p != oldPod {
				newPod = p
				return true, ""
			}
		}
		return false, "new pod not found: " + out
	})
	if newPod == oldPod {
		t.Fatalf("pod not replaced")
	}

	// port-forward /status
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	pfCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pf := exec.CommandContext(pfCtx, "kubectl", "--context", kindCtx, "-n", kindNS, "port-forward", "pod/"+newPod, fmt.Sprintf("%d:8080", port))
	var pfOut bytes.Buffer
	pf.Stdout, pf.Stderr = &pfOut, &pfOut
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = pf.Wait() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	restart := time.Now().Add(-5 * time.Second)
	testutil.Eventually(t, 3*time.Minute, 2*time.Second, func() (bool, string) {
		resp, err := http.Get(base + "/status")
		if err != nil {
			return false, err.Error() + " " + pfOut.String()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var d statusDoc
		if json.Unmarshal(b, &d) != nil {
			return false, string(b)
		}
		for name, p := range d.Pairs {
			if p.Started.After(restart) && !p.Finished.IsZero() {
				if p.Mutations != 0 {
					t.Fatalf("first cycle of the new pod: pair %s mutations=%d (state not kept on the PVC?)", name, p.Mutations)
				}
				return true, ""
			}
		}
		return false, "no finished cycle yet: " + string(b)
	})
}
