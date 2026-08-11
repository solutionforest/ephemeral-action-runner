package image

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

func TestImageDependencyClassificationIsTypedAndConservative(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{name: "deadline", err: context.DeadlineExceeded, retryable: true},
		{name: "dns", err: &net.DNSError{Err: "no such host", Name: "registry.example"}, retryable: true},
		{name: "DNS resolver failure", err: &net.DNSError{Err: "server misbehaving", Name: "registry.example"}, retryable: true},
		{name: "connection", err: errors.New("dial tcp 192.0.2.1:443: connection refused"), retryable: true},
		{name: "TLS handshake timeout", err: errors.New("net/http: TLS handshake timeout"), retryable: true},
		{name: "truncated response", err: errors.New("unexpected EOF"), retryable: true},
		{name: "EOF response", err: io.EOF, retryable: true},
		{name: "HTTP2 registry stream reset", err: errors.New("stream error: stream ID 29; PROTOCOL_ERROR; received from peer"), retryable: true},
		{name: "missing manifest", err: errors.New("manifest unknown: requested image not found"), retryable: false},
		{name: "authorization", err: errors.New("403 Forbidden: denied: requested access"), retryable: false},
		{name: "TLS trust", err: x509.UnknownAuthorityError{}, retryable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyImageDependencyFailure("registry.example", "pull image", test.err)
			if dependency.IsRetryable(got) != test.retryable {
				t.Fatalf("retryable = %t, want %t: %v", dependency.IsRetryable(got), test.retryable, got)
			}
			_, typed := dependency.AsFailure(got)
			if typed != test.retryable {
				t.Fatalf("typed failure = %t, want %t: %T", typed, test.retryable, got)
			}
		})
	}
}

func TestImageHTTP403RequiresRateLimitEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		header    http.Header
		retryable bool
	}{
		{name: "ordinary forbidden", header: http.Header{}, retryable: false},
		{name: "rate limited", header: http.Header{"X-Ratelimit-Remaining": []string{"0"}}, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusForbidden, Header: test.header}
			err := classifyImageHTTPFailure("api.github.com", "resolve runner", response, errors.New("HTTP 403"))
			if dependency.IsRetryable(err) != test.retryable {
				t.Fatalf("retryable = %t, want %t: %v", dependency.IsRetryable(err), test.retryable, err)
			}
		})
	}
}

func TestBuildxClassificationRequiresRemoteEvidence(t *testing.T) {
	network := errors.New("exit status 1")
	remote := "failed to solve: failed to do request: dial tcp: i/o timeout"
	if err := classifyImageCommandFailure("OCI registry", "Buildx remote image acquisition", network, remote, true); !dependency.IsRetryable(err) {
		t.Fatalf("remote Buildx failure was not retryable: %v", err)
	}
	deterministic := "Dockerfile: RUN ./custom-install.sh returned exit code 1"
	if err := classifyImageCommandFailure("OCI registry", "Buildx remote image acquisition", network, deterministic, true); dependency.IsRetryable(err) {
		t.Fatalf("deterministic Buildx failure was retryable: %v", err)
	}
}
