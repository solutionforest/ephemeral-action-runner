package image

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	registrytransport "github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

func classifyImageDependencyFailure(service, operation string, err error) error {
	if err == nil {
		return err
	}
	if _, ok := dependency.AsFailure(err); ok {
		return err
	}
	if !isTransientImageDependencyError(err) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%s attempt timed out: %v", operation, err)
	}
	return &dependency.Failure{Service: service, Operation: operation, Retryable: true, Cause: err}
}

func classifyImageCommandFailure(service, operation string, err error, diagnostic string, requireRemoteEvidence bool) error {
	if err == nil {
		return nil
	}
	combined := strings.TrimSpace(err.Error() + "\n" + diagnostic)
	if requireRemoteEvidence && !containsRemoteFetchEvidence(combined) {
		return err
	}
	return classifyImageDependencyFailure(service, operation, errors.New(combined))
}

func classifyImageHTTPFailure(service, operation string, response *http.Response, err error) error {
	if response == nil {
		return classifyImageDependencyFailure(service, operation, err)
	}
	metadata := dependency.HTTPMetadata{
		StatusCode: response.StatusCode,
		RequestID:  response.Header.Get("X-GitHub-Request-Id"),
	}
	if remaining, parseErr := strconv.Atoi(strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining"))); parseErr == nil {
		metadata.RateLimitRemain = remaining
		metadata.RateLimitKnown = true
	}
	if reset, parseErr := strconv.ParseInt(strings.TrimSpace(response.Header.Get("X-RateLimit-Reset")), 10, 64); parseErr == nil && reset > 0 {
		metadata.RateLimitReset = time.Unix(reset, 0)
	}
	if retryAfter := strings.TrimSpace(response.Header.Get("Retry-After")); retryAfter != "" {
		if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds > 0 {
			metadata.RetryAfter = time.Duration(seconds) * time.Second
		} else if retryAt, parseErr := http.ParseTime(retryAfter); parseErr == nil && retryAt.After(time.Now()) {
			metadata.RetryAfter = time.Until(retryAt)
		}
	}
	failure := dependency.NewHTTPFailure(service, operation, metadata, err)
	if !failure.Retryable {
		return err
	}
	return failure
}

func isTransientImageDependencyError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var certificateVerification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var certificateInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &certificateVerification) || errors.As(err, &unknownAuthority) || errors.As(err, &certificateInvalid) || errors.As(err, &hostname) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "tls handshake timeout") {
		return true
	}
	for _, terminal := range []string{"x509:", "tls:", "certificate signed by unknown authority", "certificate is not valid", "manifest unknown", "no matching manifest", "not found", "unauthorized", "authentication required", "denied: requested access", "insufficient_scope"} {
		if strings.Contains(message, terminal) {
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var registryErr *registrytransport.Error
	if errors.As(err, &registryErr) {
		return registryErr.Temporary() || registryErr.StatusCode == http.StatusRequestTimeout || registryErr.StatusCode == http.StatusTooManyRequests || registryErr.StatusCode >= http.StatusInternalServerError
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	for _, transient := range []string{"could not resolve host", "no such host", "temporary failure in name resolution", "connection refused", "connection reset", "connection timed out", "i/o timeout", "unexpected eof", "network is unreachable", "no route to host", "unexpected status code 408", "unexpected status code 429", "too many requests", "rate limit exceeded", "secondary rate limit", "http 408", "http 429", "http 500", "http 502", "http 503", "http 504", "500 internal server error", "502 bad gateway", "503 service unavailable", "504 gateway timeout", "server misbehaving"} {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

func containsRemoteFetchEvidence(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"failed to do request", "failed to fetch", "failed to authorize", "failed to copy", "pull access", "resolve image config", "load metadata for", "http://", "https://"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
