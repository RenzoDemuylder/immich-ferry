package fakeimmich

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fault describes an injected failure for the next Times matching requests
// (Times<=0 means 1). Method "" matches any method. PathPrefix is matched
// against the request path with or without the leading "/api".
type Fault struct {
	Method, PathPrefix string
	Status             int           // respond with this status instead of handling
	Times              int           // number of matching requests affected (<=0: 1)
	Delay              time.Duration // sleep before handling (respects request context)
	TruncateAt         int64         // downloads: send only this many bytes then abort the connection
	Body               string        // response body for Status (default JSON message)
	Header             map[string]string
	// AfterBody makes a Status fault drain the whole request body before responding,
	// simulating a server that failed after fully receiving an upload.
	AfterBody bool
}

type faultState struct {
	Fault
	left int
}

type ctxKey int

const faultKey ctxKey = 1

// Inject registers a fault.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := f.Times
	if n <= 0 {
		n = 1
	}
	s.faults = append(s.faults, &faultState{Fault: f, left: n})
}

func (s *Server) matchFault(r *http.Request) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if f.Method != "" && !strings.EqualFold(f.Method, r.Method) {
			continue
		}
		p := r.URL.Path
		if !strings.HasPrefix(p, f.PathPrefix) && !strings.HasPrefix(strings.TrimPrefix(p, "/api"), f.PathPrefix) {
			continue
		}
		c := f.Fault
		f.left--
		if f.left <= 0 {
			s.faults = append(s.faults[:i:i], s.faults[i+1:]...)
		}
		return &c
	}
	return nil
}

func isMutating(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	p := r.URL.Path
	return !strings.HasPrefix(p, "/api/search/") && p != "/api/assets/bulk-upload-check"
}

// handler wraps the router with recording and fault injection.
func (s *Server) handler() http.Handler {
	mux := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &RecordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone(), ContentLength: r.ContentLength}
		isMultipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/")
		if !isMultipart && r.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			rec.Body = b
			rec.BodyBytes = int64(len(b))
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, rec)
		s.mu.Unlock()

		f := s.matchFault(r)
		if f != nil && f.Delay > 0 {
			t := time.NewTimer(f.Delay)
			select {
			case <-r.Context().Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if f != nil && f.Status != 0 {
			if f.AfterBody && r.Body != nil {
				_, _ = io.Copy(io.Discard, r.Body)
			}
			for k, v := range f.Header {
				w.Header().Set(k, v)
			}
			if f.Body == "" {
				writeErr(w, f.Status, "injected fault")
				return
			}
			if strings.HasPrefix(strings.TrimSpace(f.Body), "{") {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(f.Status)
			_, _ = io.WriteString(w, f.Body)
			return
		}
		if isMutating(r) {
			s.mu.Lock()
			s.mutations++
			s.mu.Unlock()
		}
		ctx := context.WithValue(r.Context(), faultKey, f)
		ctx = context.WithValue(ctx, recKey, rec)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func faultFrom(ctx context.Context) *Fault {
	f, _ := ctx.Value(faultKey).(*Fault)
	return f
}
