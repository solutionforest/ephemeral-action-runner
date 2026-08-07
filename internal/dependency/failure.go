// Package dependency contains provider-neutral failure, retry, and
// supervision primitives.  It deliberately has no dependency on a provider
// implementation so that GitHub, Docker, and other remote dependencies can
// report the same lifecycle semantics.
package dependency

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Failure is a structured error returned by a dependency operation.
//
// Cause is intentionally kept out of JSON representations.  Callers should
// use errors.As/errors.Is on the original error to inspect it.  RetryAfter and
// RateLimitReset are server hints; a supervisor may use them as a lower bound
// for the next attempt even when they exceed the local backoff cap.
type Failure struct {
	Service         string
	Operation       string
	Retryable       bool
	RetryAfter      time.Duration
	RateLimitReset  time.Time
	RateLimitRemain int
	RateLimitKnown  bool
	RequestID       string
	StatusCode      int
	Cause           error
}

// FailureOption customizes a structured Failure.
type FailureOption func(*Failure)

// WithRetryable marks the failure retryable (or terminal when false).
func WithRetryable(retryable bool) FailureOption {
	return func(failure *Failure) { failure.Retryable = retryable }
}

// WithRetryAfter supplies a Retry-After duration hint.
func WithRetryAfter(delay time.Duration) FailureOption {
	return func(failure *Failure) {
		if delay > 0 {
			failure.RetryAfter = delay
		}
	}
}

// WithRateLimitReset supplies the absolute rate-limit reset time.
func WithRateLimitReset(reset time.Time) FailureOption {
	return func(failure *Failure) { failure.RateLimitReset = reset }
}

// WithRateLimitRemaining supplies rate-limit remaining metadata.  A known
// zero value is evidence that a 403 is a rate-limit response.
func WithRateLimitRemaining(remaining int) FailureOption {
	return func(failure *Failure) {
		failure.RateLimitRemain = remaining
		failure.RateLimitKnown = true
	}
}

// WithRequestID supplies a sanitized request identifier.
func WithRequestID(requestID string) FailureOption {
	return func(failure *Failure) { failure.RequestID = SanitizeRequestID(requestID) }
}

// WithStatusCode supplies the HTTP status associated with the failure.
func WithStatusCode(statusCode int) FailureOption {
	return func(failure *Failure) { failure.StatusCode = statusCode }
}

// NewFailure creates a structured dependency failure.  The cause is wrapped
// and remains discoverable with errors.Is/errors.As.
func NewFailure(service, operation string, cause error, options ...FailureOption) *Failure {
	failure := &Failure{
		Service:   strings.TrimSpace(service),
		Operation: strings.TrimSpace(operation),
		Cause:     cause,
	}
	for _, option := range options {
		if option != nil {
			option(failure)
		}
	}
	if failure.RequestID != "" {
		failure.RequestID = SanitizeRequestID(failure.RequestID)
	}
	return failure
}

// NewTransient creates a retryable provider-neutral failure.  It is a short
// compatibility constructor for providers that only have an operation name.
func NewTransient(operation string, cause error) *Failure {
	return NewFailure("dependency", operation, cause, WithRetryable(true))
}

// Error implements error.
func (failure *Failure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	parts := make([]string, 0, 4)
	if failure.Service != "" {
		parts = append(parts, failure.Service)
	}
	if failure.Operation != "" {
		parts = append(parts, failure.Operation)
	}
	prefix := "dependency failure"
	if len(parts) > 0 {
		prefix += " (" + strings.Join(parts, "/") + ")"
	}
	if failure.StatusCode > 0 {
		prefix += fmt.Sprintf(" HTTP %d", failure.StatusCode)
	}
	if failure.Cause == nil {
		return prefix
	}
	return prefix + ": " + sanitizeErrorText(failure.Cause.Error())
}

// Unwrap exposes the underlying dependency error.
func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// FailureCarrier is implemented by provider-specific errors that can expose
// a structured dependency failure without importing this package's provider.
// GitHub's HTTPError implements this interface.
type FailureCarrier interface {
	DependencyFailure() *Failure
}

// AsFailure extracts the first structured failure from err.  A provider error
// may expose one through FailureCarrier; wrapped errors are handled by
// errors.As as usual.
func AsFailure(err error) (*Failure, bool) {
	if err == nil {
		return nil, false
	}
	var failure *Failure
	if errors.As(err, &failure) && failure != nil {
		return failure, true
	}
	var carrier FailureCarrier
	if errors.As(err, &carrier) && carrier != nil {
		if failure = carrier.DependencyFailure(); failure != nil {
			return failure, true
		}
	}
	return nil, false
}

// IsRetryable reports whether err is a dependency failure suitable for a
// lifecycle retry.  Context cancellation/deadline errors and TLS trust or
// configuration failures are terminal.  Bare transport/network errors are
// retryable because they represent an unavailable remote dependency.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if failure, ok := AsFailure(err); ok {
		return failure.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if terminalTLSError(err) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var operationErr *net.OpError
	if errors.As(err, &operationErr) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return networkErr.Timeout() || networkErr.Temporary()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Timeout()
	}
	return false
}

// IsTransient is a compatibility alias for IsRetryable.
func IsTransient(err error) bool { return IsRetryable(err) }

// IsTypedRetryable reports whether err already carries a structured retryable
// dependency failure. Lifecycle supervisors use this stricter boundary so an
// arbitrary provider or local error cannot enter outage handling merely by
// resembling a network error string.
func IsTypedRetryable(err error) bool {
	failure, ok := AsFailure(err)
	return ok && failure.Retryable
}

// ClassifyHTTPStatus applies the shared HTTP retry policy.  408, 429, and
// 5xx responses are retryable.  A 403 is retryable only when rate-limit
// evidence is present; ordinary authorization/permission failures are
// terminal.  All other statuses are terminal.
func ClassifyHTTPStatus(status int, rateLimitEvidence bool) bool {
	switch {
	case status == 408, status == 429, status >= 500 && status <= 599:
		return true
	case status == 403:
		return rateLimitEvidence
	default:
		return false
	}
}

// HTTPMetadata is the provider-neutral metadata needed to classify an HTTP
// response.  GitHub's HTTPError adapts to this shape through
// NewHTTPFailure/DependencyFailure.
type HTTPMetadata struct {
	StatusCode      int
	RetryAfter      time.Duration
	RateLimitReset  time.Time
	RateLimitRemain int
	RateLimitKnown  bool
	RequestID       string
	Body            string
}

// NewHTTPFailure creates a typed failure from an HTTP response and applies the
// shared status/rate-limit classification rules.
func NewHTTPFailure(service, operation string, metadata HTTPMetadata, cause error) *Failure {
	rateLimitEvidence := metadata.RateLimitKnown && metadata.RateLimitRemain <= 0
	if !rateLimitEvidence && metadata.RateLimitReset.After(time.Now()) {
		rateLimitEvidence = true
	}
	if !rateLimitEvidence && metadata.RetryAfter > 0 {
		rateLimitEvidence = true
	}
	if !rateLimitEvidence {
		body := strings.ToLower(metadata.Body)
		rateLimitEvidence = strings.Contains(body, "rate limit") || strings.Contains(body, "secondary rate")
	}
	return NewFailure(service, operation, cause,
		WithRetryable(ClassifyHTTPStatus(metadata.StatusCode, rateLimitEvidence)),
		WithStatusCode(metadata.StatusCode),
		WithRetryAfter(metadata.RetryAfter),
		WithRateLimitReset(metadata.RateLimitReset),
		WithRateLimitRemainingValue(metadata.RateLimitRemain, metadata.RateLimitKnown),
		WithRequestID(metadata.RequestID),
	)
}

// WithRateLimitRemainingValue records a value and whether the header was
// present.  It is useful when zero is meaningful evidence.
func WithRateLimitRemainingValue(remaining int, known bool) FailureOption {
	return func(failure *Failure) {
		failure.RateLimitRemain = remaining
		failure.RateLimitKnown = known
	}
}

// SanitizeRequestID bounds request identifiers and rejects control characters
// so they can safely appear in logs, status, and durable state.
func SanitizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			return ""
		}
	}
	return value
}

func terminalTLSError(err error) bool {
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return true
	}
	var invalidReason x509.InsecureAlgorithmError
	if errors.As(err, &invalidReason) {
		return true
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "tls handshake timeout") {
		return false
	}
	for _, marker := range []string{
		"x509:", "tls:", "certificate verify failed", "unknown authority", "certificate is not valid",
		"invalid peer certificate", "tls: first record does not look like a tls handshake",
		"tls: bad record", "tls: unsupported", "tls: no renegotiation",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// ParseHTTPStatusCode extracts the first three-digit status from common
// provider/guest error strings.  It is intentionally conservative and is
// useful only for opaque provider responses where no HTTPMetadata is present.
func ParseHTTPStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"status code does not indicate success", "http response code", "http status"} {
		if index := strings.Index(text, marker); index >= 0 {
			for _, field := range strings.Fields(text[index+len(marker):]) {
				field = strings.Trim(field, ":=()[]{}.,;\"'")
				if len(field) != 3 {
					continue
				}
				if value, parseErr := strconv.Atoi(field); parseErr == nil && value >= 100 && value <= 599 {
					return value, true
				}
			}
		}
	}
	return 0, false
}
