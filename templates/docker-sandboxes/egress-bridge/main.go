//go:build linux

// epar-egress-bridge is the guest-local endpoint used by the Docker
// Sandboxes template for destinations that need the authenticated host relay.
// It deliberately speaks a small subset of HTTP instead of using net/http:
// retaining the bufio.Reader that consumed the CONNECT headers is what keeps
// bytes sent in the same write as those headers from being lost when the
// connection becomes a byte tunnel.
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	dockerListenAddress   = "127.0.0.1:3129"
	workflowListenAddress = "127.0.0.1:3130"
	configPath            = "/run/epar/egress-relay.json"
	caCertPath            = "/run/epar/egress-relay-ca.crt"
	caKeyPath             = "/run/epar/egress-relay-ca.key"
	upstreamCAPath        = "/opt/epar/trust/ca-bundle.pem"

	// The limits are intentionally small.  CONNECT carries an authority and
	// no request body, so accepting arbitrarily large headers only creates a
	// memory/slowloris surface in a root process.
	maxConfigBytes = 8 << 10
	maxHeaderBytes = 32 << 10
	maxHeaderLine  = 8 << 10
	maxHeaderLines = 128
	maxTargetBytes = 4 << 10

	requestTimeout = 10 * time.Second
	relayTimeout   = 10 * time.Second
	tunnelTimeout  = 5 * time.Minute

	relayHost = "host.docker.internal"

	statusBadRequest       = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	statusMethodNotAllowed = "HTTP/1.1 405 Method Not Allowed\r\nAllow: CONNECT, GET\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	statusBadGateway       = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	statusUnavailable      = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	statusConnected        = "HTTP/1.1 200 Connection Established\r\n\r\n"
	statusHealthy          = "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
)

// relayConfig is the complete on-disk schema.  The host relay address is
// intentionally constrained to host.docker.internal:port: the guest bridge
// must not become a configurable arbitrary host socket.  token is already a
// 32-byte RawURLEncoding value (43 ASCII characters); it is never written to
// logs or included in an error returned to a client.
type relayConfig struct {
	SchemaVersion int    `json:"schemaVersion"`
	RelayAddress  string `json:"relayAddress"`
	Token         string `json:"token"`
}

type request struct {
	method string
	target string
}

type bridge struct {
	configPath string
	dial       func(network, address string) (net.Conn, error)
	load       func(string) (relayConfig, error)
	ca         *localCA
	secure     func(client, upstream net.Conn, target string) (net.Conn, net.Conn, error)
}

type localCA struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	mu          sync.Mutex
	leaves      map[string]tls.Certificate
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *bufferedConn) Read(data []byte) (int, error) {
	return connection.reader.Read(data)
}

func (connection *bufferedConn) CloseWrite() error {
	return closeWrite(connection.Conn)
}

func newBridge(path string) *bridge {
	return &bridge{
		configPath: path,
		dial: func(network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: relayTimeout}
			return d.Dial(network, address)
		},
		load: loadConfig,
	}
}

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "EPAR egress bridge: must run as root")
		os.Exit(1)
	}
	ca, err := loadOrCreateLocalCA(caCertPath, caKeyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "EPAR egress bridge: unable to initialize local TLS authority")
		os.Exit(1)
	}
	dockerListener, err := net.Listen("tcp", dockerListenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "EPAR egress bridge: unable to listen")
		os.Exit(1)
	}
	defer dockerListener.Close()
	workflowListener, err := net.Listen("tcp", workflowListenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "EPAR egress bridge: unable to listen")
		os.Exit(1)
	}
	defer workflowListener.Close()

	dockerBridge := newBridge(configPath)
	dockerBridge.ca = ca
	dockerBridge.secure = dockerBridge.secureTLS
	workflowBridge := newBridge(configPath)
	go serve(workflowListener, workflowBridge)
	serve(dockerListener, dockerBridge)
}

func serve(listener net.Listener, b *bridge) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// A listener close is the only expected terminal condition.  Keep
			// diagnostics deliberately generic: neither request headers nor
			// configuration (which contains the relay token) belong in logs.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go b.handle(conn)
	}
}

func (b *bridge) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	reader := bufio.NewReaderSize(conn, maxHeaderLine)
	req, err := readRequest(reader)
	if err != nil {
		writeResponse(conn, statusBadRequest)
		return
	}

	switch req.method {
	case "GET":
		if req.target != "/health" {
			writeResponse(conn, statusBadRequest)
			return
		}
		if err := b.health(conn); err != nil {
			writeResponse(conn, statusUnavailable)
			return
		}
		writeResponse(conn, statusHealthy)
	case "CONNECT":
		target, err := validateTarget(req.target)
		if err != nil {
			writeResponse(conn, statusBadRequest)
			return
		}
		if err := b.connect(conn, reader, target); err != nil {
			writeResponse(conn, statusBadGateway)
			return
		}
	default:
		writeResponse(conn, statusMethodNotAllowed)
	}
}

func (b *bridge) connect(client net.Conn, buffered *bufio.Reader, target string) error {
	loader := b.load
	if loader == nil {
		loader = loadConfig
	}
	cfg, err := loader(b.configPath)
	if err != nil {
		return err
	}
	relay, err := b.dial("tcp", cfg.RelayAddress)
	if err != nil {
		return err
	}
	defer func() {
		// The tunnel takes ownership of relay after the 200 response.  On
		// every error path before that point it must be closed here.
		if relay != nil {
			relay.Close()
		}
	}()
	if err := relay.SetDeadline(time.Now().Add(relayTimeout)); err != nil {
		return err
	}
	if err := relayHandshake(relay, cfg.Token, target, "OK\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(client, statusConnected); err != nil {
		return err
	}
	_ = client.SetDeadline(time.Time{})
	_ = relay.SetDeadline(time.Time{})

	// Keep the reader when wrapping the client connection: it may already hold
	// TLS ClientHello bytes read ahead while parsing the CONNECT headers.
	downstream := net.Conn(&bufferedConn{Conn: client, reader: buffered})
	activeRelay := relay
	relay = nil
	upstream := net.Conn(activeRelay)
	if b.secure != nil {
		downstream, upstream, err = b.secure(downstream, activeRelay, target)
		if err != nil {
			activeRelay.Close()
			return err
		}
	}
	tunnel(downstream, bufio.NewReader(downstream), upstream)
	return nil
}

func (b *bridge) secureTLS(client, upstream net.Conn, target string) (net.Conn, net.Conn, error) {
	if b.ca == nil {
		return nil, nil, errors.New("local TLS authority is unavailable")
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return nil, nil, errors.New("invalid TLS target")
	}
	certificate, err := b.ca.certificateForHost(host)
	if err != nil {
		return nil, nil, err
	}
	rootPEM, err := os.ReadFile(upstreamCAPath)
	if err != nil {
		return nil, nil, errors.New("upstream CA bundle is unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, nil, errors.New("upstream CA bundle is invalid")
	}
	upstreamTLS := tls.Client(upstream, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: host,
	})
	serverTLS := tls.Server(client, &tls.Config{
		Certificates:     []tls.Certificate{certificate},
		CurvePreferences: []tls.CurveID{tls.CurveP256},
		MinVersion:       tls.VersionTLS12,
	})
	deadline := time.Now().Add(relayTimeout)
	_ = upstreamTLS.SetDeadline(deadline)
	if err := upstreamTLS.Handshake(); err != nil {
		upstreamTLS.Close()
		return nil, nil, errors.New("upstream TLS handshake failed")
	}
	_ = serverTLS.SetDeadline(deadline)
	if err := serverTLS.Handshake(); err != nil {
		upstreamTLS.Close()
		serverTLS.Close()
		return nil, nil, errors.New("downstream TLS handshake failed")
	}
	_ = upstreamTLS.SetDeadline(time.Time{})
	_ = serverTLS.SetDeadline(time.Time{})
	return serverTLS, upstreamTLS, nil
}

func (b *bridge) health(client net.Conn) error {
	loader := b.load
	if loader == nil {
		loader = loadConfig
	}
	cfg, err := loader(b.configPath)
	if err != nil {
		return err
	}
	relay, err := b.dial("tcp", cfg.RelayAddress)
	if err != nil {
		return err
	}
	defer relay.Close()
	if err := relay.SetDeadline(time.Now().Add(relayTimeout)); err != nil {
		return err
	}
	// PING is a control frame understood only by the host relay.  It proves
	// that the authenticated relay endpoint is reachable without selecting a
	// registry destination or causing a host-side DNS/SSRF check.
	return relayHandshake(relay, cfg.Token, "PING", "PONG\n")
}

func relayHandshake(conn net.Conn, token, target, expected string) error {
	if !validToken(token) || !validRelayTarget(target) {
		return errors.New("invalid relay handshake input")
	}
	line := "EPAR1 " + token + " " + target + "\n"
	if _, err := io.WriteString(conn, line); err != nil {
		return err
	}
	response := make([]byte, len(expected))
	if _, err := io.ReadFull(io.LimitReader(conn, int64(len(response))), response); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(response, []byte(expected)) != 1 {
		return errors.New("relay handshake rejected")
	}
	return nil
}

func tunnel(client net.Conn, buffered *bufio.Reader, relay net.Conn) {
	defer relay.Close()
	deadline := time.Now().Add(tunnelTimeout)
	_ = client.SetDeadline(deadline)
	_ = relay.SetDeadline(deadline)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(relay, buffered)
		closeWrite(relay)
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(client, relay)
		closeWrite(client)
	}()
	wait.Wait()
}

func closeWrite(conn net.Conn) error {
	if writer, ok := conn.(interface{ CloseWrite() error }); ok {
		return writer.CloseWrite()
	}
	return nil
}

func loadOrCreateLocalCA(certPath, keyPath string) (*localCA, error) {
	certExists, err := pathExists(certPath)
	if err != nil {
		return nil, err
	}
	keyExists, err := pathExists(keyPath)
	if err != nil {
		return nil, err
	}
	if certExists != keyExists {
		return nil, errors.New("incomplete local TLS authority")
	}
	if !certExists {
		if err := createLocalCA(certPath, keyPath); err != nil {
			return nil, err
		}
	}
	certPEM, err := readRootOwnedFile(certPath, 0444)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readRootOwnedFile(keyPath, 0600)
	if err != nil {
		return nil, err
	}
	certBlock, rest := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid local TLS authority certificate")
	}
	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	now := time.Now()
	if err != nil || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) || !certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("invalid local TLS authority certificate")
	}
	keyBlock, rest := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid local TLS authority key")
	}
	privateKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil || privateKey.Curve != elliptic.P256() {
		return nil, errors.New("invalid local TLS authority key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(privateKey.PublicKey.X) != 0 || publicKey.Y.Cmp(privateKey.PublicKey.Y) != 0 {
		return nil, errors.New("local TLS authority key mismatch")
	}
	return &localCA{certificate: certificate, privateKey: privateKey, leaves: make(map[string]tls.Certificate)}, nil
}

func createLocalCA(certPath, keyPath string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return errors.New("generate local TLS authority key")
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "EPAR ephemeral egress relay"},
		NotBefore:    now.Add(-5 * time.Minute),
		// The authority is destroyed with this exact sandbox. Give it enough
		// validity for a long-idle runner without disrupting dockerd solely to
		// rotate an otherwise unreachable ephemeral key.
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return errors.New("create local TLS authority certificate")
	}
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return errors.New("encode local TLS authority key")
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeExclusiveRootFile(certPath, certPEM, 0444); err != nil {
		return err
	}
	if err := writeExclusiveRootFile(keyPath, keyPEM, 0600); err != nil {
		_ = os.Remove(certPath)
		return err
	}
	return nil
}

func (authority *localCA) certificateForHost(host string) (tls.Certificate, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if certificate, found := authority.leaves[host]; found {
		leaf := certificate.Leaf
		if leaf == nil && len(certificate.Certificate) > 0 {
			leaf, _ = x509.ParseCertificate(certificate.Certificate[0])
		}
		if leaf != nil && time.Now().Add(5*time.Minute).Before(leaf.NotAfter) {
			certificate.Leaf = leaf
			authority.leaves[host] = certificate
			return certificate, nil
		}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, errors.New("generate local TLS leaf key")
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	notAfter := now.Add(24 * time.Hour)
	if authority.certificate.NotAfter.Before(notAfter) {
		notAfter = authority.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if address := net.ParseIP(host); address != nil {
		template.IPAddresses = []net.IP{address}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &privateKey.PublicKey, authority.privateKey)
	if err != nil {
		return tls.Certificate{}, errors.New("create local TLS leaf certificate")
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, errors.New("parse local TLS leaf certificate")
	}
	certificate := tls.Certificate{Certificate: [][]byte{der, authority.certificate.Raw}, PrivateKey: privateKey, Leaf: leaf}
	authority.leaves[host] = certificate
	return certificate, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil || serial.Sign() <= 0 {
		return nil, errors.New("generate certificate serial")
	}
	return serial, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errors.New("inspect local TLS authority file")
}

func writeExclusiveRootFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return errors.New("create local TLS authority file")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return errors.New("secure local TLS authority file")
	}
	if _, err := file.Write(data); err != nil {
		return errors.New("write local TLS authority file")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync local TLS authority file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close local TLS authority file")
	}
	complete = true
	return nil
}

func readRootOwnedFile(path string, mode os.FileMode) ([]byte, error) {
	file, err := openRootOwnedFile(path, mode)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, errors.New("local TLS authority file is too large")
	}
	return data, nil
}

func openRootOwnedFile(path string, mode os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("cannot open root-owned file")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		file.Close()
		return nil, errors.New("root-owned file mode is invalid")
	}
	return file, nil
}

func writeResponse(conn net.Conn, response string) {
	_, _ = io.WriteString(conn, response)
}

func readRequest(reader *bufio.Reader) (request, error) {
	var result request
	var total int
	for lineNumber := 0; lineNumber < maxHeaderLines; lineNumber++ {
		line, err := readLine(reader, maxHeaderLine)
		if err != nil {
			return request{}, err
		}
		total += len(line)
		if total > maxHeaderBytes {
			return request{}, errors.New("request headers too large")
		}
		if lineNumber == 0 {
			result, err = parseRequestLine(line)
			if err != nil {
				return request{}, err
			}
			continue
		}
		if bytes.Equal(line, []byte("\r\n")) {
			return result, nil
		}
		if err := validateHeaderLine(line); err != nil {
			return request{}, err
		}
	}
	return request{}, errors.New("too many request headers")
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, min(limit, 256))
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > limit-len(line) {
			return nil, errors.New("request line too long")
		}
		line = append(line, part...)
		if err == nil {
			if len(line) < 2 || line[len(line)-2] != '\r' {
				return nil, errors.New("request line is not CRLF terminated")
			}
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func parseRequestLine(line []byte) (request, error) {
	line = bytes.TrimSuffix(line, []byte("\r\n"))
	parts := bytes.Split(line, []byte{' '})
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(parts[2]) == 0 {
		return request{}, errors.New("malformed request line")
	}
	if !validHTTPToken(parts[0]) || bytes.IndexByte(parts[1], '\t') >= 0 || bytes.IndexByte(parts[2], '\t') >= 0 {
		return request{}, errors.New("malformed request line")
	}
	version := string(parts[2])
	if version != "HTTP/1.0" && version != "HTTP/1.1" {
		return request{}, errors.New("unsupported HTTP version")
	}
	return request{method: string(parts[0]), target: string(parts[1])}, nil
}

func validateHeaderLine(line []byte) error {
	body := line[:len(line)-2]
	if len(body) == 0 {
		return errors.New("empty header")
	}
	colon := bytes.IndexByte(body, ':')
	if colon <= 0 || !validHTTPToken(body[:colon]) {
		return errors.New("malformed header")
	}
	for _, value := range body[colon+1:] {
		if value < 0x20 && value != '\t' {
			return errors.New("control byte in header")
		}
	}
	return nil
}

func validHTTPToken(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
		if b <= 0x20 || b >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={} \t", rune(b)) {
			return false
		}
	}
	return true
}

func validateTarget(raw string) (string, error) {
	if raw == "" || len(raw) > maxTargetBytes || strings.IndexFunc(raw, func(r rune) bool {
		return r == '\r' || r == '\n' || r == 0 || r < 0x20 || r == 0x7f
	}) >= 0 {
		return "", errors.New("target contains control bytes")
	}
	for _, b := range []byte(raw) {
		if b >= 0x80 {
			return "", errors.New("target must be ASCII")
		}
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || port == "" {
		return "", errors.New("target must be host:port")
	}
	if strings.ContainsAny(host, "/\\@?#") || strings.TrimSpace(host) != host {
		return "", errors.New("target host is malformed")
	}
	if !decimalPort(port) {
		return "", errors.New("target port is invalid")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("target port is invalid")
	}
	// SplitHostPort accepts a bracketed IPv6 zone.  The relay performs DNS
	// and SSRF policy checks; here we only ensure it receives a single safe
	// authority token and not a value that can break the framing line.
	if strings.ContainsAny(raw, " \t") {
		return "", errors.New("target contains whitespace")
	}
	return raw, nil
}

func validRelayTarget(target string) bool {
	return target == "PING" || func() bool {
		_, err := validateTarget(target)
		return err == nil
	}()
}

func loadConfig(path string) (relayConfig, error) {
	file, err := openConfig(path)
	if err != nil {
		return relayConfig{}, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return relayConfig{}, err
	}
	if len(data) > maxConfigBytes {
		return relayConfig{}, errors.New("relay config too large")
	}
	if err := rejectDuplicateTopLevelFields(data); err != nil {
		return relayConfig{}, err
	}
	var cfg relayConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return relayConfig{}, errors.New("invalid relay config")
	}
	if decoder.More() {
		return relayConfig{}, errors.New("trailing relay config")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return relayConfig{}, errors.New("trailing relay config")
	}
	if cfg.SchemaVersion != 1 || !validToken(cfg.Token) {
		return relayConfig{}, errors.New("invalid relay config")
	}
	if err := validateRelayAddress(cfg.RelayAddress); err != nil {
		return relayConfig{}, err
	}
	return cfg, nil
}

func rejectDuplicateTopLevelFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid relay config")
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return errors.New("relay config must be an object")
	}
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return errors.New("invalid relay config")
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("invalid relay config")
		}
		if _, exists := seen[name]; exists {
			return errors.New("duplicate relay config field")
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("invalid relay config")
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return errors.New("invalid relay config")
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("invalid relay config")
	}
	return nil
}

func openConfig(path string) (*os.File, error) {
	// O_NOFOLLOW prevents a final-component symlink race between a path check
	// and open.  f.Stat below then verifies ownership/mode on the descriptor we
	// actually read.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("cannot open relay config")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		file.Close()
		return nil, errors.New("relay config ownership or mode is invalid")
	}
	return file, nil
}

func validateRelayAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, relayHost) || port == "" {
		return errors.New("relay address is invalid")
	}
	if !decimalPort(port) || strings.ContainsAny(address, "\r\n \t") {
		return errors.New("relay address is invalid")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("relay address is invalid")
	}
	return nil
}

func decimalPort(port string) bool {
	if port == "" {
		return false
	}
	for _, b := range []byte(port) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func validToken(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	for _, b := range []byte(token) {
		if !(b >= 'A' && b <= 'Z') && !(b >= 'a' && b <= 'z') && !(b >= '0' && b <= '9') && b != '-' && b != '_' {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
