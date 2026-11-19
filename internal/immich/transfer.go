package immich

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	errStalled      = errors.New("transfer stalled: no bytes moved within stall timeout")
	errResponseWait = errors.New("timed out waiting for server response")
	errTooSlow      = errors.New("transfer too slow: fewer bytes than the minimum moved within the stall window")
)

// watchdog cancels a request context when no progress is observed (body phase),
// when too few bytes move per stall window (trickling), or when the response does
// not arrive in time (wait phase). It also drives the client heartbeat.
type watchdog struct {
	stall    time.Duration
	minBytes int64        // <=0 disables the trickle check
	expected int64        // total body bytes expected (0 unknown); caps the per-window threshold
	last     atomic.Int64 // unix nanos of last progress
	bytes    atomic.Int64
	phase    atomic.Int32 // 0 body, 1 wait, 2 stopped
	deadline atomic.Int64 // unix nanos, wait phase
	cancel   context.CancelCauseFunc
	done     chan struct{}
}

func (c *Client) newWatchdog(stall time.Duration, expected int64, cancel context.CancelCauseFunc) *watchdog {
	w := &watchdog{stall: stall, minBytes: c.minBytes, expected: expected, cancel: cancel, done: make(chan struct{})}
	w.touch()
	iv := min(max(stall/4, time.Millisecond), time.Second)
	hb, every := c.cfg.Heartbeat, c.beatEvery
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		winStart := time.Now()
		var winBase int64
		lastBeat := time.Now()
		for {
			select {
			case <-w.done:
				return
			case <-t.C:
				tn := time.Now()
				now := tn.UnixNano()
				switch w.phase.Load() {
				case 0:
					if now-w.last.Load() > int64(w.stall) {
						w.cancel(errStalled)
						return
					}
					if tn.Sub(winStart) >= w.stall {
						got := w.bytes.Load() - winBase
						need := w.minBytes
						if w.expected > 0 {
							need = min(need, w.expected-winBase)
						}
						if w.minBytes > 0 && need > 0 && got < need {
							w.cancel(errTooSlow)
							return
						}
						winStart, winBase = tn, w.bytes.Load()
					}
				case 1:
					if now > w.deadline.Load() {
						w.cancel(errResponseWait)
						return
					}
				}
				if hb != nil && tn.Sub(lastBeat) >= every {
					lastBeat = tn
					hb()
				}
			}
		}
	}()
	return w
}

func (w *watchdog) touch() { w.last.Store(time.Now().UnixNano()) }

// progress records n bytes moved.
func (w *watchdog) progress(n int) {
	w.bytes.Add(int64(n))
	w.touch()
}

func (w *watchdog) startWait(d time.Duration) {
	w.deadline.Store(time.Now().Add(d).UnixNano())
	w.phase.Store(1)
}

func (w *watchdog) stop() {
	if w.phase.Swap(2) != 2 {
		close(w.done)
	}
}

// xferErr classifies an error from a streaming request.
func (c *Client) xferErr(parent, rctx context.Context, method, path string, err error) error {
	if parent.Err() != nil {
		return canceledErr(method, path, parent)
	}
	if cause := context.Cause(rctx); cause != nil && (errors.Is(cause, errStalled) || errors.Is(cause, errResponseWait) || errors.Is(cause, errTooSlow)) {
		return transient(fmt.Errorf("immich %s %s: %w", method, path, cause))
	}
	return fmt.Errorf("immich %s %s: %w", method, path, err)
}

// DownloadOriginal streams the original into w and returns bytes written; onBytes reports progress.
// It does not retry: the caller owns the destination and must recreate it.
func (c *Client) DownloadOriginal(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error) {
	path := "/assets/" + url.PathEscape(id) + "/original"
	if ctx.Err() != nil {
		return 0, canceledErr("GET", path, ctx)
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	wd := c.newWatchdog(c.cfg.StallTimeout, 0, cancel)
	defer wd.stop()

	req, err := c.newRequest(rctx, "GET", path, nil, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, c.xferErr(ctx, rctx, "GET", path, err)
	}
	defer resp.Body.Close()
	wd.touch()
	if resp.ContentLength > 0 {
		wd.expected = resp.ContentLength
	}
	if resp.StatusCode != http.StatusOK {
		ae := apiError("GET", path, resp)
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			ae.retryAfter = d
		}
		return 0, ae
	}
	buf := make([]byte, 256<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			wd.progress(n)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, fmt.Errorf("immich download %s: write: %w", id, werr)
			}
			total += int64(n)
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, c.xferErr(ctx, rctx, "GET", path, rerr)
		}
	}
	if resp.ContentLength >= 0 && total != resp.ContentLength {
		return total, transient(fmt.Errorf("immich GET %s: short download: got %d of %d bytes", path, total, resp.ContentLength))
	}
	return total, nil
}

// ---- upload ----

type uploadPlan struct {
	head, trailer []byte
	size          int64
	boundary      string
}

func (p uploadPlan) contentLength() int64 { return int64(len(p.head)) + p.size + int64(len(p.trailer)) }

// planUpload precomputes the multipart framing so Content-Length is exact.
func planUpload(r UploadRequest) (uploadPlan, error) {
	if r.File == nil {
		return uploadPlan{}, errors.New("immich upload: nil file")
	}
	st, err := r.File.Stat()
	if err != nil {
		return uploadPlan{}, fmt.Errorf("immich upload: stat: %w", err)
	}
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return uploadPlan{}, err
	}
	boundary := "immichferry" + hex.EncodeToString(rb[:])
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return uploadPlan{}, err
	}
	fields := [][2]string{
		{"fileCreatedAt", r.FileCreatedAt.Format(time.RFC3339Nano)},
		{"fileModifiedAt", r.FileModifiedAt.Format(time.RFC3339Nano)},
		{"filename", r.Filename},
	}
	if r.DurationMS != nil {
		fields = append(fields, [2]string{"duration", strconv.FormatInt(*r.DurationMS, 10)})
	}
	if r.IsFavorite != nil {
		fields = append(fields, [2]string{"isFavorite", strconv.FormatBool(*r.IsFavorite)})
	}
	if r.Visibility != "" {
		fields = append(fields, [2]string{"visibility", string(r.Visibility)})
	}
	if r.LivePhotoVideoID != "" {
		fields = append(fields, [2]string{"livePhotoVideoId", r.LivePhotoVideoID})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return uploadPlan{}, err
		}
	}
	if _, err := mw.CreateFormFile("assetData", r.Filename); err != nil {
		return uploadPlan{}, err
	}
	head := append([]byte(nil), buf.Bytes()...)
	if err := mw.Close(); err != nil {
		return uploadPlan{}, err
	}
	trailer := append([]byte(nil), buf.Bytes()[len(head):]...)
	return uploadPlan{head: head, trailer: trailer, size: st.Size(), boundary: boundary}, nil
}

type fileProgress struct {
	r       io.Reader
	wd      *watchdog
	onBytes func(int64)
}

func (f *fileProgress) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.wd.progress(n)
		if f.onBytes != nil {
			f.onBytes(int64(n))
		}
	}
	return n, err
}

type bodyEnd struct {
	r     io.Reader
	onEOF func()
}

func (b *bodyEnd) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.onEOF()
	}
	return n, err
}

// Upload uploads an asset (multipart, text fields first). The body is streamed
// from r.File with an exact Content-Length. Transient failures are retried by
// seeking the file back to 0. onBytes counts file bytes handed to the transport
// (retries count again).
func (c *Client) Upload(ctx context.Context, r UploadRequest, onBytes func(int64)) (UploadResult, error) {
	const method, path = "POST", "/assets"
	plan, err := planUpload(r)
	if err != nil {
		return UploadResult{}, err
	}
	var res UploadResult
	sentFull := false // the previous attempt handed the whole body to the transport
	err = c.withRetry(ctx, method, path, func() error {
		if sentFull && r.Checksum != "" {
			// The body may have landed even though the attempt failed (response timeout/5xx):
			// avoid re-sending multi-GB files if the server already has the asset.
			if id, ok := c.existingByChecksum(ctx, r.Checksum); ok {
				res = UploadResult{ID: id, Duplicate: true}
				return nil
			}
		}
		var aerr error
		res, sentFull, aerr = c.uploadAttempt(ctx, r, plan, onBytes)
		return aerr
	})
	return res, err
}

func (c *Client) existingByChecksum(ctx context.Context, checksum string) (string, bool) {
	rs, err := c.UploadCheck(ctx, []UploadCheckItem{{ID: "x", Checksum: checksum}})
	if err != nil {
		c.log.Debug("immich upload: post-failure duplicate check failed; re-uploading", "error", err)
		return "", false
	}
	for _, r := range rs {
		if !r.Accept && r.AssetID != "" {
			return r.AssetID, true
		}
	}
	return "", false
}

func (c *Client) uploadAttempt(ctx context.Context, r UploadRequest, plan uploadPlan, onBytes func(int64)) (UploadResult, bool, error) {
	const method, path = "POST", "/assets"
	if _, err := r.File.Seek(0, io.SeekStart); err != nil {
		return UploadResult{}, false, fmt.Errorf("immich upload: seek: %w", err)
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var sent atomic.Bool
	wd := c.newWatchdog(c.cfg.StallTimeout, plan.size, cancel)
	defer wd.stop()

	fp := &fileProgress{r: io.LimitReader(r.File, plan.size), wd: wd, onBytes: onBytes}
	body := &bodyEnd{
		r:     io.MultiReader(bytes.NewReader(plan.head), fp, bytes.NewReader(plan.trailer)),
		onEOF: func() { sent.Store(true); wd.startWait(c.cfg.ResponseWait(plan.size)) },
	}
	req, err := c.newRequest(rctx, method, path, nil, io.NopCloser(body))
	if err != nil {
		return UploadResult{}, false, err
	}
	req.ContentLength = plan.contentLength()
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+plan.boundary)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.hc.Do(req)
	if err != nil {
		return UploadResult{}, sent.Load(), c.xferErr(ctx, rctx, method, path, err)
	}
	defer resp.Body.Close()
	wd.startWait(c.cfg.JSONTimeout) // reading the small response
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		ae := apiError(method, path, resp)
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			ae.retryAfter = d
		}
		return UploadResult{}, sent.Load(), ae
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return UploadResult{}, sent.Load(), c.xferErr(ctx, rctx, method, path, fmt.Errorf("decode response: %w", err))
	}
	return UploadResult{ID: out.ID, Duplicate: out.Status == "duplicate"}, true, nil
}
