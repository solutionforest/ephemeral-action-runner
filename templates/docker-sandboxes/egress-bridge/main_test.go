//go:build linux

package main

import (
	"bufio"
	"bytes"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testConfig() relayConfig {
	return relayConfig{
		SchemaVersion: 1,
		RelayAddress:  "host.docker.internal:43125",
		Token:         testToken,
	}
}

func TestValidateRelayAddressAndToken(t *testing.T) {
	if err := validateRelayAddress("host.docker.internal:43125"); err != nil {
		t.Fatalf("valid relay address rejected: %v", err)
	}
	for _, address := range []string{
		"127.0.0.1:43125",
		"host.docker.internal:0",
		"host.docker.internal:65536",
		"host.docker.internal:not-a-port",
		"host.docker.internal:43125\n",
		"host.docker.internal:43125 extra",
		"host.docker.internal",
	} {
		if err := validateRelayAddress(address); err == nil {
			t.Errorf("invalid relay address accepted: %q", address)
		}
	}
	if !validToken(testToken) {
		t.Fatal("test token is not valid RawURLEncoding")
	}
	for _, token := range []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("A", 42) + "!"} {
		if validToken(token) {
			t.Errorf("invalid token accepted: %q", token)
		}
	}
}

func TestValidateTargetRejectsMalformedAuthorities(t *testing.T) {
	for _, target := range []string{
		"example.test:443",
		"127.0.0.1:1",
		"[2001:db8::1]:443",
	} {
		if got, err := validateTarget(target); err != nil || got != target {
			t.Errorf("valid target rejected: %q: %v", target, err)
		}
	}
	for _, target := range []string{
		"",
		"example.test",
		"example.test:0",
		"example.test:65536",
		"example.test:not-a-port",
		"example.test:+443",
		"example.test:443\n",
		"example.test:443\r\n",
		"example.test: 443",
		"example.test:443 extra",
		"例子.test:443",
		"https://example.test:443",
		"user@example.test:443",
		"[2001:db8::1:443",
	} {
		if _, err := validateTarget(target); err == nil {
			t.Errorf("malformed target accepted: %q", target)
		}
	}
}

func TestReadRequestPreservesBoundedHeaderParsing(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\nbody"))
	req, err := readRequest(reader)
	if err != nil {
		t.Fatalf("readRequest failed: %v", err)
	}
	if req.method != "CONNECT" || req.target != "example.test:443" {
		t.Fatalf("unexpected request: %#v", req)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading buffered bytes failed: %v", err)
	}
	if string(body) != "body" {
		t.Fatalf("buffered bytes were not preserved: %q", body)
	}

	longHeader := "CONNECT example.test:443 HTTP/1.1\r\nX-Test: " + strings.Repeat("x", maxHeaderLine) + "\r\n\r\n"
	if _, err := readRequest(bufio.NewReader(strings.NewReader(longHeader))); err == nil {
		t.Fatal("oversized header accepted")
	}
	for _, raw := range []string{
		"CONNECT example.test:443 HTTP/1.1\n\n",
		"GET /health HTTP/1.1\r\nBadHeader\r\n\r\n",
		"GET /health HTTP/2\r\n\r\n",
	} {
		if _, err := readRequest(bufio.NewReader(strings.NewReader(raw))); err == nil {
			t.Errorf("malformed request accepted: %q", raw)
		}
	}
}

func TestRawConnectTunnelsBytesAlreadyBufferedAfterHeaders(t *testing.T) {
	client, server := net.Pipe()
	relay, relayPeer := net.Pipe()
	defer client.Close()
	defer relayPeer.Close()

	cfg := testConfig()
	b := &bridge{
		configPath: "ignored",
		load:       func(string) (relayConfig, error) { return cfg, nil },
		dial:       func(string, string) (net.Conn, error) { return relay, nil },
	}
	serverDone := make(chan struct{})
	go func() {
		b.handle(server)
		close(serverDone)
	}()

	relayDone := make(chan error, 1)
	go func() {
		defer close(relayDone)
		reader := bufio.NewReader(relayPeer)
		line, err := reader.ReadString('\n')
		if err != nil {
			relayDone <- err
			return
		}
		if want := "EPAR1 " + testToken + " example.test:443\n"; line != want {
			relayDone <- errors.New("unexpected relay handshake: " + line)
			return
		}
		if _, err := io.WriteString(relayPeer, "OK\n"); err != nil {
			relayDone <- err
			return
		}
		payload := make([]byte, len("request-body"))
		if _, err := io.ReadFull(reader, payload); err != nil {
			relayDone <- err
			return
		}
		if string(payload) != "request-body" {
			relayDone <- errors.New("buffered request bytes changed")
			return
		}
		_, err = io.WriteString(relayPeer, "relay-reply")
		_ = relayPeer.Close()
		relayDone <- err
	}()

	request := "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\nrequest-body"
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(client, request)
		writeDone <- err
	}()
	response := make([]byte, len(statusConnected))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatalf("reading CONNECT response failed: %v", err)
	}
	if string(response) != statusConnected {
		t.Fatalf("unexpected CONNECT response: %q", response)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("writing CONNECT request failed: %v", err)
	}
	reply := make([]byte, len("relay-reply"))
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("reading tunneled reply failed: %v", err)
	}
	if string(reply) != "relay-reply" {
		t.Fatalf("unexpected tunneled reply: %q", reply)
	}
	client.Close()
	select {
	case err := <-relayDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not finish")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("bridge handler did not finish")
	}
}

func TestHealthUsesAuthenticatedPingWithoutDestination(t *testing.T) {
	client, server := net.Pipe()
	relay, relayPeer := net.Pipe()
	defer client.Close()
	defer relayPeer.Close()
	cfg := testConfig()
	b := &bridge{
		configPath: "ignored",
		load:       func(string) (relayConfig, error) { return cfg, nil },
		dial:       func(string, string) (net.Conn, error) { return relay, nil },
	}
	done := make(chan struct{})
	go func() {
		b.handle(server)
		close(done)
	}()
	go func() {
		reader := bufio.NewReader(relayPeer)
		line, err := reader.ReadString('\n')
		if err == nil && line == "EPAR1 "+testToken+" PING\n" {
			_, _ = io.WriteString(relayPeer, "PONG\n")
		}
	}()
	if _, err := io.WriteString(client, "GET /health HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != statusHealthy {
		t.Fatalf("unexpected health response: %q", response)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health handler did not finish")
	}
}

func TestConnectRejectsMalformedTargetWithoutDial(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	dialed := false
	b := &bridge{
		configPath: "ignored",
		load:       func(string) (relayConfig, error) { return testConfig(), nil },
		dial: func(string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		},
	}
	done := make(chan struct{})
	go func() {
		b.handle(server)
		close(done)
	}()
	if _, err := io.WriteString(client, "CONNECT example.test:443\n\n"); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != statusBadRequest {
		t.Fatalf("unexpected malformed-target response: %q", response)
	}
	if dialed {
		t.Fatal("malformed target reached relay dialer")
	}
	<-done
}

func TestLoadConfigRejectsSymlinkAndWrongMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root ownership checks require a root Linux test process")
	}
	dir := t.TempDir()
	valid := `{"schemaVersion":1,"relayAddress":"host.docker.internal:43125","token":"` + testToken + `"}`
	path := filepath.Join(dir, "egress-relay.json")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("valid root-owned config rejected: %v", err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("group-readable config accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(link); err == nil {
		t.Fatal("config symlink accepted")
	}
}

func TestLocalCAIsEphemeralRootOnlyAndIssuesMatchingLeaves(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root ownership checks require a root Linux test process")
	}
	directory := t.TempDir()
	certPath := filepath.Join(directory, "relay-ca.crt")
	keyPath := filepath.Join(directory, "relay-ca.key")
	authority, err := loadOrCreateLocalCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{certPath: 0444, keyPath: 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	leaf, err := authority.certificateForHost("registry-1.docker.io")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	if _, err := parsed.Verify(x509.VerifyOptions{DNSName: "registry-1.docker.io", Roots: roots}); err != nil {
		t.Fatalf("issued leaf did not verify: %v", err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{DNSName: "auth.docker.io", Roots: roots}); err == nil {
		t.Fatal("issued leaf verified for an unrelated host")
	}
	firstLeaf := append([]byte(nil), leaf.Certificate[0]...)
	cached := authority.leaves["registry-1.docker.io"]
	cached.Leaf.NotAfter = time.Now().Add(-time.Minute)
	authority.leaves["registry-1.docker.io"] = cached
	renewed, err := authority.certificateForHost("registry-1.docker.io")
	if err != nil {
		t.Fatalf("renew expired leaf: %v", err)
	}
	if bytes.Equal(firstLeaf, renewed.Certificate[0]) {
		t.Fatal("expired cached leaf was not renewed")
	}
}

func TestLoadConfigStrictSchema(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root ownership checks require a root Linux test process")
	}
	cases := []string{
		`{"schemaVersion":1,"relayAddress":"host.docker.internal:43125","token":"` + testToken + `","extra":true}`,
		`{"schemaVersion":2,"relayAddress":"host.docker.internal:43125","token":"` + testToken + `"}`,
		`{"schemaVersion":1,"relayAddress":"host.docker.internal:43125","token":"short"}`,
		`{"schemaVersion":1,"relayAddress":"127.0.0.1:43125","token":"` + testToken + `"}`,
		`{"schemaVersion":1,"relayAddress":"host.docker.internal:43125"}`,
		`{"schemaVersion":1,"schemaVersion":1,"relayAddress":"host.docker.internal:43125","token":"` + testToken + `"}`,
	}
	for _, content := range cases {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Errorf("invalid config accepted: %s", content)
		}
	}
	trailing := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(trailing, []byte(`{"schemaVersion":1,"relayAddress":"host.docker.internal:43125","token":"`+testToken+`"} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(trailing); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestRelayHandshakeDoesNotAcceptUnexpectedResponse(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewReader(peer)
		_, _ = reader.ReadString('\n')
		_, _ = io.WriteString(peer, "NO\n")
	}()
	if err := relayHandshake(client, testToken, "example.test:443", "OK\n"); err == nil {
		t.Fatal("unexpected relay response accepted")
	}
	<-done
}

func TestStatusStringsDoNotContainSecrets(t *testing.T) {
	for _, response := range []string{statusBadRequest, statusMethodNotAllowed, statusBadGateway, statusUnavailable, statusConnected, statusHealthy} {
		if bytes.Contains([]byte(response), []byte(testToken)) {
			t.Fatal("status response contains relay token")
		}
	}
}
