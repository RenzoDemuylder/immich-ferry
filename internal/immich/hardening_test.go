package immich_test

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

func TestRedirectNeverForwardsKey(t *testing.T) {
	var seen atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer redir.Close()
	cl, _ := immich.New(immich.Config{BaseURL: redir.URL, APIKey: "secret", MaxRetries: 2, BackoffBase: time.Millisecond})
	_, err := cl.GetAlbum(context.Background(), "x")
	if immich.KindOf(err) != immich.KindBadRequest || !strings.Contains(err.Error(), "base URL redirects to "+target.URL) {
		t.Fatalf("kind=%v err=%v", immich.KindOf(err), err)
	}
	if _, err := cl.Version(context.Background()); immich.KindOf(err) != immich.KindBadRequest {
		t.Fatalf("%v", err)
	}
	if seen.Load() != 0 {
		t.Fatalf("redirect target saw %d requests", seen.Load())
	}
}

func TestCreateAlbumNotRetried(t *testing.T) {
	e := newEnv(t, "album.create")
	e.srv.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/albums", Status: 502})
	_, err := e.cl.CreateAlbum(e.ctx, "A", "", nil)
	if err == nil || immich.KindOf(err) != immich.KindTransient {
		t.Fatalf("err=%v", err)
	}
	if n := len(reqsTo(e.srv, "POST", "/api/albums")); n != 1 {
		t.Fatalf("POST count = %d, want 1", n)
	}
}

func TestHeartbeatDuringRetryAfterSleep(t *testing.T) {
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0.3")
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"major":3,"minor":4,"patch":1}`)
	}))
	defer ts.Close()
	var beats atomic.Int32
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", Heartbeat: func() { beats.Add(1) }, HeartbeatInterval: 20 * time.Millisecond})
	if _, err := cl.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	if beats.Load() < 8 {
		t.Fatalf("beats=%d", beats.Load())
	}
}

func TestHeartbeatDuringUploadResponseWait(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = http_discard(r)
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"abc","status":"created"}`)
	}))
	defer ts.Close()
	var beats atomic.Int32
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 200 * time.Millisecond,
		ResponseWait:      func(int64) time.Duration { return 5 * time.Second },
		Heartbeat:         func() { beats.Add(1) },
		HeartbeatInterval: 40 * time.Millisecond})
	f := tmpFile(t, []byte("hello"))
	res, err := cl.Upload(context.Background(), immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f}, nil)
	if err != nil || res.ID != "abc" {
		t.Fatalf("%+v %v", res, err)
	}
	if beats.Load() < 4 {
		t.Fatalf("beats=%d", beats.Load())
	}
}

func http_discard(r *http.Request) (int64, error) {
	var n int64
	buf := make([]byte, 32<<10)
	for {
		k, err := r.Body.Read(buf)
		n += int64(k)
		if err != nil {
			return n, nil
		}
	}
}

func TestDownloadTricklingTooSlow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer ts.Close()
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 300 * time.Millisecond, MinStallBytes: 1000})
	start := time.Now()
	_, err := cl.DownloadOriginal(context.Background(), "id", &strings.Builder{}, nil)
	if immich.KindOf(err) != immich.KindTransient || !strings.Contains(err.Error(), "too slow") || time.Since(start) > 5*time.Second {
		t.Fatalf("kind=%v err=%v", immich.KindOf(err), err)
	}
}

func TestUploadChecksumSkipsReuploadAfterFullBody(t *testing.T) {
	e := newEnv(t, "asset.upload", "asset.read")
	content := []byte("the-same-bytes")
	id := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "a.jpg", Content: content, FileCreatedAt: t0})
	sum := sha1.Sum(content)
	e.srv.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets", Status: 502, AfterBody: true, Times: 1})
	f := tmpFile(t, content)
	res, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f,
		Checksum: base64.StdEncoding.EncodeToString(sum[:])}, nil)
	if err != nil || !res.Duplicate || res.ID != id {
		t.Fatalf("%+v %v", res, err)
	}
	if n := len(reqsTo(e.srv, "POST", "/api/assets")); n != 1 {
		t.Fatalf("POST /assets count %d, want 1", n)
	}
}

func TestUploadWithoutChecksumStillRetries(t *testing.T) {
	e := newEnv(t, "asset.upload", "asset.read")
	e.srv.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/assets", Status: 502, AfterBody: true})
	f := tmpFile(t, []byte("fresh-bytes"))
	res, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f}, nil)
	if err != nil || res.Duplicate || res.ID == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if n := len(reqsTo(e.srv, "POST", "/api/assets")); n != 2 {
		t.Fatalf("count %d", n)
	}
}

func TestKindOfURLErrors(t *testing.T) {
	ue := func(err error) error { return &url.Error{Op: "Post", URL: "http://x", Err: err} }
	cases := []struct {
		name string
		err  error
		want immich.Kind
	}{
		{"invalid header", ue(errors.New("net/http: invalid header field value for \"x-api-key\"")), immich.KindBadRequest},
		{"scheme", ue(errors.New(`unsupported protocol scheme "ftp"`)), immich.KindBadRequest},
		{"http to https", ue(errors.New("http: server gave HTTP response to HTTPS client")), immich.KindBadRequest},
		{"refused", ue(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), immich.KindTransient},
		{"reset", ue(syscall.ECONNRESET), immich.KindTransient},

		{"dns temp", ue(&net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}), immich.KindTransient},
		{"dns nxdomain", ue(&net.DNSError{Err: "no such host", IsNotFound: true}), immich.KindBadRequest},
		{"wrapped", fmt.Errorf("immich GET /x: %w", ue(syscall.ECONNRESET)), immich.KindTransient},
	}
	for _, c := range cases {
		if got := immich.KindOf(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestInvalidHeaderKeyNotRetried(t *testing.T) {
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer ts.Close()
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "line1\nline2", MaxRetries: 3, BackoffBase: time.Millisecond})
	_, err := cl.Version(context.Background())
	if immich.KindOf(err) != immich.KindBadRequest || n.Load() != 0 {
		t.Fatalf("kind=%v n=%d err=%v", immich.KindOf(err), n.Load(), err)
	}
}
