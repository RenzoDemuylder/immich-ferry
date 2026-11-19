package immich_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"immich-ferry/internal/immich"
)

func TestDownloadStallWatchdog(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stop sending
	}))
	defer ts.Close()
	// Trickle check off: 1000 bytes is also below the per-window minimum, so on a slow
	// runner "too slow" could fire before "stalled" (covered by TestDownloadTricklingTooSlow).
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 150 * time.Millisecond, MinStallBytes: -1})
	start := time.Now()
	var sb strings.Builder
	n, err := cl.DownloadOriginal(context.Background(), "id", &sb, nil)
	if immich.KindOf(err) != immich.KindTransient || n != 1000 || time.Since(start) > 5*time.Second {
		t.Fatalf("n=%d err=%v kind=%v", n, err, immich.KindOf(err))
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("%v", err)
	}
}

func TestDownloadParentCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := cl.DownloadOriginal(ctx, "id", io.Discard, nil)
	if immich.KindOf(err) != immich.KindCanceled {
		t.Fatalf("%v", err)
	}
}

func TestUploadStallThenRetry(t *testing.T) {
	var n atomic.Int32
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
		if n.Add(1) == 1 {
			hang(r, stop) // never read the body
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"abc","status":"created"}`)
	})
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 200 * time.Millisecond,
		MaxRetries: 2, BackoffBase: time.Millisecond})
	f := tmpFile(t, []byte(strings.Repeat("z", 48<<20))) // larger than socket buffers
	var sent atomic.Int64
	res, err := cl.Upload(context.Background(), immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f},
		func(k int64) { sent.Add(k) })
	if err != nil || res.ID != "abc" || n.Load() != 2 {
		t.Fatalf("%+v %v n=%d", res, err, n.Load())
	}
	if sent.Load() < 48<<20 {
		t.Fatalf("sent %d", sent.Load())
	}
}

func TestUploadStallExhausted(t *testing.T) {
	var n atomic.Int32
	ts := newServer(t, func(w http.ResponseWriter, r *http.Request, stop <-chan struct{}) {
		n.Add(1)
		hang(r, stop)
	})
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 100 * time.Millisecond, MaxRetries: 1, BackoffBase: time.Millisecond})
	f := tmpFile(t, []byte(strings.Repeat("z", 48<<20)))
	_, err := cl.Upload(context.Background(), immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f}, nil)
	if immich.KindOf(err) != immich.KindTransient || n.Load() != 2 {
		t.Fatalf("%v n=%d", err, n.Load())
	}
}

func TestUploadResponseWait(t *testing.T) {
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if n.Add(1) == 1 {
			<-r.Context().Done() // body consumed, response never comes
			return
		}
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"dup","status":"duplicate"}`)
	}))
	defer ts.Close()
	waits := atomic.Int32{}
	cl, _ := immich.New(immich.Config{BaseURL: ts.URL, APIKey: "k", StallTimeout: 50 * time.Millisecond, MaxRetries: 2,
		BackoffBase: time.Millisecond, ResponseWait: func(int64) time.Duration { waits.Add(1); return 400 * time.Millisecond }})
	f := tmpFile(t, []byte("small"))
	start := time.Now()
	res, err := cl.Upload(context.Background(), immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f}, nil)
	// StallTimeout (50ms) must NOT apply while waiting for the response; ResponseWait (400ms) does.
	if err != nil || !res.Duplicate || res.ID != "dup" || n.Load() != 2 || time.Since(start) < 350*time.Millisecond || waits.Load() < 2 {
		t.Fatalf("%+v %v n=%d el=%v", res, err, n.Load(), time.Since(start))
	}
}

func TestUploadErrorKinds(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   immich.Kind
		calls  int32
	}{
		{413, `{"message":"Payload Too Large"}`, immich.KindTooLarge, 1},
		{400, `{"message":"Quota has been exceeded!"}`, immich.KindQuota, 1},
		{500, `{"message":"x"}`, immich.KindTransient, 4},
	} {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		_, err := clientFor(t, ts.URL, "k").Upload(context.Background(),
			immich.UploadRequest{Filename: "a.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: tmpFile(t, []byte("abc"))}, nil)
		var ae *immich.APIError
		if !errors.As(err, &ae) || immich.KindOf(err) != c.want || n.Load() != c.calls {
			t.Errorf("%d: %v kind=%v calls=%d", c.status, err, immich.KindOf(err), n.Load())
		}
		ts.Close()
	}
}

func TestUploadNilFile(t *testing.T) {
	cl := clientFor(t, "http://127.0.0.1:1", "k")
	if _, err := cl.Upload(context.Background(), immich.UploadRequest{Filename: "a.jpg"}, nil); err == nil {
		t.Fatal("want error")
	}
}

// hang blocks until the request context ends or the test is over; handlers that
// do not read the body never observe client disconnects via r.Context().
func hang(r *http.Request, stop <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-stop:
	}
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, stop <-chan struct{})) *httptest.Server {
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, r, stop) }))
	t.Cleanup(func() { close(stop); ts.Close() })
	return ts
}
