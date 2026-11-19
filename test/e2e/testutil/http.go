// Package testutil contains raw-HTTP helpers for the immich-ferry e2e tests.
// It is deliberately independent of internal/immich so that the tests verify
// the real server behaviour instead of our own client's assumptions.
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPError is returned for any non-2xx response.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// Message extracts the "message" field of an Immich error body (or the raw body).
func (e *HTTPError) Message() string {
	var m struct {
		Message any `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &m) == nil && m.Message != nil {
		return fmt.Sprint(m.Message)
	}
	return e.Body
}

var defaultClient = &http.Client{Timeout: 120 * time.Second}

// do performs a JSON request. auth is either {"x-api-key": k} or {"Authorization": "Bearer t"}.
// It returns the status code and body; err is *HTTPError for non-2xx, in which case status/body are still returned.
func do(ctx context.Context, hc *http.Client, base, method, path string, hdr map[string]string, body any, out any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/api"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if hc == nil {
		hc = defaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, data, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, data, fmt.Errorf("%s %s: decode: %w (body %.200s)", method, path, err, data)
		}
	}
	return resp.StatusCode, data, nil
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	if he, ok := err.(*HTTPError); ok {
		return he.Status
	}
	return 0
}

// ErrMessage returns the server message carried by err, or err.Error().
func ErrMessage(err error) string {
	if he, ok := err.(*HTTPError); ok {
		return he.Message()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
