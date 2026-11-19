package immich

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultJSONTimeout  = 60 * time.Second
	defaultStallTimeout = 2 * time.Minute
	defaultMaxRetries   = 4
	defaultBackoffBase  = 500 * time.Millisecond
	defaultBackoffMax   = 30 * time.Second
	maxRetryAfter       = 5 * time.Minute
	maxErrBody          = 64 << 10

	defaultMinStallBytes  = 64 << 10
	defaultHeartbeatEvery = 15 * time.Second
)

// Config configures a Client.
type Config struct {
	BaseURL      string
	APIKey       string
	UserAgent    string
	InsecureTLS  bool
	JSONTimeout  time.Duration
	StallTimeout time.Duration
	// ResponseWait returns how long to wait for the response after the request body was fully sent.
	ResponseWait func(size int64) time.Duration
	MaxRetries   int
	Logger       *slog.Logger

	// BackoffBase and BackoffMax tune retry backoff (defaults 500ms / 30s). Mainly for tests.
	BackoffBase, BackoffMax time.Duration

	// MinStallBytes is the minimum number of bytes that must move per StallTimeout
	// window while a transfer is in its body phase; slower transfers are cancelled
	// as "transfer too slow" (transient). 0 means the default (64 KiB); negative disables.
	MinStallBytes int64

	// Heartbeat, when set, is invoked at least every HeartbeatInterval (default 15s)
	// while the client is legitimately waiting: during active transfers, while waiting
	// for the response after an upload body, and during Retry-After/backoff sleeps.
	// It lets a supervisor's liveness check distinguish a long wait from a hang.
	Heartbeat         func()
	HeartbeatInterval time.Duration
}

// Client talks to one Immich instance. It satisfies engine.Immich.
type Client struct {
	root        string
	cfg         Config
	hc          *http.Client
	log         *slog.Logger
	backoffBase time.Duration
	backoffMax  time.Duration
	minBytes    int64
	beatEvery   time.Duration
}

// NormalizeBaseURL strips a trailing "/" and an optional "/api" suffix.
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/api")
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("immich: invalid base URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("immich: invalid base URL %q: need http(s)://host", raw)
	}
	return s, nil
}

// New creates a Client.
func New(cfg Config) (*Client, error) {
	root, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.JSONTimeout <= 0 {
		cfg.JSONTimeout = defaultJSONTimeout
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStallTimeout
	}
	if cfg.ResponseWait == nil {
		cfg.ResponseWait = func(size int64) time.Duration {
			return 5*time.Minute + time.Duration(size/(5<<20))*time.Second
		}
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "immich-ferry"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in
	}
	// Never follow redirects: they would forward the API key to other hosts and
	// turn POST/PUT/DELETE into GET. A 3xx surfaces as a config-type APIError.
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	c := &Client{root: root, cfg: cfg, hc: hc, log: log,
		backoffBase: cfg.BackoffBase, backoffMax: cfg.BackoffMax,
		minBytes: cfg.MinStallBytes, beatEvery: cfg.HeartbeatInterval}
	if c.minBytes == 0 {
		c.minBytes = defaultMinStallBytes
	}
	if c.beatEvery <= 0 {
		c.beatEvery = defaultHeartbeatEvery
	}
	if c.backoffBase <= 0 {
		c.backoffBase = defaultBackoffBase
	}
	if c.backoffMax <= 0 {
		c.backoffMax = defaultBackoffMax
	}
	return c, nil
}

// BaseURL returns the normalized root URL (no trailing slash, no /api).
func (c *Client) BaseURL() string { return c.root }

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.root + "/api" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	return req, nil
}

// apiError builds an *APIError from a non-2xx response (body is consumed, not closed).
func apiError(method, path string, resp *http.Response) *APIError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	ae := &APIError{Method: method, Path: path, Status: resp.StatusCode,
		CorrelationID: resp.Header.Get("X-Correlation-ID")}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if u, err := url.Parse(loc); err == nil {
			loc = u.Redacted()
		}
		ae.Message = fmt.Sprintf("base URL redirects to %s; configure the final URL", loc)
		return ae
	}
	var env struct {
		Message json.RawMessage `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &env) == nil && len(env.Message) > 0 {
		var s string
		var arr []string
		switch {
		case json.Unmarshal(env.Message, &s) == nil:
			ae.Message = s
		case json.Unmarshal(env.Message, &arr) == nil:
			ae.Message = strings.Join(arr, "; ")
		default:
			ae.Message = string(env.Message)
		}
		for _, e := range env.Errors {
			if e.Message != "" {
				ae.Details = append(ae.Details, e.Message)
			}
		}
	} else {
		ae.Message = strings.TrimSpace(string(b))
		if len(ae.Message) > 300 {
			ae.Message = ae.Message[:300]
		}
		if ae.Message == "" {
			ae.Message = http.StatusText(resp.StatusCode)
		}
	}
	return ae
}

// parseRetryAfter parses a Retry-After header given in (possibly fractional) seconds.
func parseRetryAfter(h string) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(h, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	d := time.Duration(f * float64(time.Second))
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d, true
}

// backoff returns the full-jitter delay for the given 0-based retry number.
func (c *Client) backoff(retry int) time.Duration {
	d := c.backoffBase << uint(min(retry, 20))
	if d <= 0 || d > c.backoffMax {
		d = c.backoffMax
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// beat invokes the configured heartbeat, if any.
func (c *Client) beat() {
	if c.cfg.Heartbeat != nil {
		c.cfg.Heartbeat()
	}
}

// sleepBeat sleeps for d like sleepCtx but beats the heartbeat every beatEvery.
func (c *Client) sleepBeat(ctx context.Context, d time.Duration) error {
	if c.cfg.Heartbeat == nil {
		return sleepCtx(ctx, d)
	}
	for d > 0 {
		c.beat()
		step := min(d, c.beatEvery)
		if err := sleepCtx(ctx, step); err != nil {
			return err
		}
		d -= step
	}
	return ctx.Err()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryDelay decides whether err is retryable and how long to wait.
func (c *Client) retryDelay(err error, retry int) (time.Duration, bool) {
	if KindOf(err) != KindTransient {
		return 0, false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if !retryableStatus(ae.Status) {
			return 0, false
		}
		if ae.retryAfter > 0 {
			return ae.retryAfter, true
		}
	}
	return c.backoff(retry), true
}

// canceledErr wraps a parent-context error so KindOf reports KindCanceled.
func canceledErr(method, path string, parent context.Context) error {
	return fmt.Errorf("immich %s %s: %w", method, path, parent.Err())
}

// doJSON performs one logical JSON call with retries. in may be nil; out may be nil.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("immich %s %s: encode body: %w", method, path, err)
		}
	}
	return c.withRetry(ctx, method, path, func() error {
		return c.jsonAttempt(ctx, method, path, query, payload, out)
	})
}

// doJSONOnce is doJSON without retries, for non-idempotent calls where a retry after
// an ambiguous failure could duplicate the effect (e.g. POST /albums).
func (c *Client) doJSONOnce(ctx context.Context, method, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("immich %s %s: encode body: %w", method, path, err)
	}
	if ctx.Err() != nil {
		return canceledErr(method, path, ctx)
	}
	return c.jsonAttempt(ctx, method, path, nil, payload, out)
}

// withRetry runs attempt until success, a non-retryable error, or MaxRetries retries.
func (c *Client) withRetry(ctx context.Context, method, path string, attempt func() error) error {
	for n := 0; ; n++ {
		if ctx.Err() != nil {
			return canceledErr(method, path, ctx)
		}
		err := attempt()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return canceledErr(method, path, ctx)
		}
		delay, ok := c.retryDelay(err, n)
		if !ok || n >= c.cfg.MaxRetries {
			return err
		}
		c.log.Debug("immich request failed, retrying", "op", method+" "+path, "attempt", n+1, "delay", delay, "error", err)
		if serr := c.sleepBeat(ctx, delay); serr != nil {
			return canceledErr(method, path, ctx)
		}
	}
}

func (c *Client) jsonAttempt(parent context.Context, method, path string, query url.Values, payload []byte, out any) error {
	c.beat()
	ctx, cancel := context.WithTimeout(parent, c.cfg.JSONTimeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return fmt.Errorf("immich %s %s: %w", method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.netErr(parent, method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := apiError(method, path, resp)
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			ae.retryAfter = d
		}
		return ae
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return c.netErr(parent, method, path, fmt.Errorf("decode response: %w", err))
	}
	return nil
}

// netErr wraps a transport-level error. Attempt-level timeouts (parent still alive) are transient.
func (c *Client) netErr(parent context.Context, method, path string, err error) error {
	if parent.Err() != nil {
		return canceledErr(method, path, parent)
	}
	err = fmt.Errorf("immich %s %s: %w", method, path, err)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return transient(err)
	}
	return err
}

func decodeJSON(r io.Reader, out any) error { return json.NewDecoder(r).Decode(out) }
