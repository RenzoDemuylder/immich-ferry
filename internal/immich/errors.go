package immich

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Kind classifies an error for retry/handling decisions.
type Kind int

// Error kinds. NotFound includes 400 "Not found or no ... access".
const (
	KindUnknown Kind = iota
	KindTransient
	KindAuth
	KindForbidden
	KindNotFound
	KindBadRequest
	KindQuota
	KindTooLarge
	KindCanceled
)

var kindNames = [...]string{"unknown", "transient", "auth", "forbidden", "not_found", "bad_request", "quota", "too_large", "canceled"}

// String returns the lower_snake name of the kind.
func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return "unknown"
	}
	return kindNames[k]
}

// APIError is a non-2xx response from the Immich API.
type APIError struct {
	Method, Path  string
	Status        int
	Message       string
	Details       []string
	CorrelationID string

	retryAfter time.Duration // parsed Retry-After (unexported)
}

// Error implements error.
func (e *APIError) Error() string {
	s := fmt.Sprintf("immich %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Message)
	if len(e.Details) > 0 {
		s += " (" + strings.Join(e.Details, "; ") + ")"
	}
	if e.CorrelationID != "" {
		s += " [correlation " + e.CorrelationID + "]"
	}
	return s
}

// Kind classifies the API error by status and message.
func (e *APIError) Kind() Kind {
	msg := e.Message
	switch {
	case e.Status == http.StatusUnauthorized:
		return KindAuth
	case e.Status == http.StatusForbidden:
		return KindForbidden
	case e.Status == http.StatusNotFound:
		return KindNotFound
	case e.Status == http.StatusRequestEntityTooLarge:
		return KindTooLarge
	case e.Status == http.StatusBadRequest && notFoundRe.MatchString(msg):
		return KindNotFound
	case e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "quota"):
		return KindQuota
	case e.Status >= 300 && e.Status < 400:
		return KindBadRequest // redirect: base URL must be the final URL
	case e.Status == http.StatusTooManyRequests || e.Status >= 500:
		return KindTransient
	case e.Status >= 400 && e.Status < 500:
		return KindBadRequest
	}
	return KindUnknown
}

var notFoundRe = regexp.MustCompile(`(?i)not found|no .* access`)

// retryableStatus reports whether a status is retried by the client.
func retryableStatus(s int) bool {
	switch s {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// kindErr tags an arbitrary error with an explicit Kind.
type kindErr struct {
	kind Kind
	err  error
}

func (e *kindErr) Error() string { return e.err.Error() }
func (e *kindErr) Unwrap() error { return e.err }

// transient wraps err so KindOf reports KindTransient.
func transient(err error) error { return &kindErr{kind: KindTransient, err: err} }

// WithKind tags err with an explicit Kind that KindOf reports (for errors the
// caller classifies itself, e.g. per-id results of a bulk album request).
func WithKind(kind Kind, err error) error { return &kindErr{kind: kind, err: err} }

// KindOf classifies err (APIError, context cancellation, network errors).
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Kind()
	}
	var ke *kindErr
	if errors.As(err, &ke) {
		return ke.kind
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return KindCanceled
	}
	// *url.Error implements net.Error, so unwrap it first and classify the cause:
	// only real network errors are transient; TLS, scheme, header and other
	// malformed-request errors are configuration problems (KindBadRequest).
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		if k := KindOf(ue.Err); k != KindUnknown {
			return k
		}
		return KindBadRequest
	}
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	var rh tls.RecordHeaderError
	if errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he) || errors.As(err, &rh) {
		return KindBadRequest
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout || dns.IsTemporary {
			return KindTransient
		}
		return KindBadRequest
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, net.ErrClosed) {
		return KindTransient
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return KindTransient
	}
	return KindUnknown
}
