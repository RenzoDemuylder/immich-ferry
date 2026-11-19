package immich_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

var t0 = time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)

type env struct {
	srv *fakeimmich.Server
	cl  *immich.Client
	uid string
	key string
	ctx context.Context
}

func newEnv(t *testing.T, perms ...string) *env {
	t.Helper()
	srv := fakeimmich.New(t)
	uid, key := srv.AddUser("alice", perms...)
	return &env{srv: srv, cl: clientFor(t, srv.URL(), key), uid: uid, key: key, ctx: context.Background()}
}

func clientFor(t *testing.T, url, key string) *immich.Client {
	t.Helper()
	cl, err := immich.New(immich.Config{BaseURL: url, APIKey: key, MaxRetries: 3,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func (e *env) seed(name string, i int, extra ...func(*fakeimmich.SeedOpts)) string {
	o := fakeimmich.SeedOpts{Owner: e.uid, Filename: name, Content: []byte(fmt.Sprintf("content-%s-%d", name, i)),
		FileCreatedAt: t0.Add(time.Duration(i) * time.Minute)}
	for _, f := range extra {
		f(&o)
	}
	return e.srv.SeedAsset(o)
}

func tmpFile(t *testing.T, content []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "up.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func jsonEq(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("bad json %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("json mismatch\n got: %s\nwant: %s", gb, wb)
	}
}

func rawPost(t *testing.T, url, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func reqsTo(srv *fakeimmich.Server, method, path string) []fakeimmich.RecordedRequest {
	var out []fakeimmich.RecordedRequest
	for _, r := range srv.Requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}
