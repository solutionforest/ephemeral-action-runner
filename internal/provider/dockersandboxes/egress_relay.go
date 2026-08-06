package dockersandboxes

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	relayProtocolPrefix = "EPAR1 "
	relayHeaderLimit    = 1024
	relayMaxConnections = 128
	relayDialTimeout    = 15 * time.Second
	relayHeaderTimeout  = 10 * time.Second
	relayIdleTimeout    = 5 * time.Minute
	guestRelayPort      = 3129
)

type egressRelay struct {
	listener net.Listener
	port     int
	provider *Provider
	closed   chan struct{}
	sem      chan struct{}
	once     sync.Once
}

type guestRelayConfiguration struct {
	SchemaVersion int    `json:"schemaVersion"`
	RelayAddress  string `json:"relayAddress"`
	Token         string `json:"token"`
}

func (p *Provider) ActivateHostTrustRuntime(ctx context.Context, instance provider.Instance) (activationErr error) {
	if !p.hostTrustRelayEnabled {
		if p.logger != nil {
			p.logger.Info("Docker Sandboxes host-trust relay activation skipped because the Windows overlay relay is disabled", "provider", "docker-sandboxes", "instance", instance.Name)
		}
		return nil
	}
	if p.logger != nil {
		p.logger.Info(fmt.Sprintf("Docker Sandboxes host-trust relay activation started on controller port %d", p.hostTrustRelayPort), "provider", "docker-sandboxes", "instance", instance.Name)
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	relay, token, err := p.ensureRelayToken(instance.Name)
	if err != nil {
		return fmt.Errorf("prepare Docker Sandboxes host-trust relay: %w", err)
	}
	configured := false
	guestActivationAttempted := false
	probeStarted := time.Now().UTC()
	var addedPolicyRules []provider.NetworkPolicyRule
	defer func() {
		if !configured {
			if guestActivationAttempted {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
				rollbackErr := p.finalizeGuestRelay(rollbackCtx, instance, "--rollback")
				cancel()
				if rollbackErr != nil {
					activationErr = errors.Join(activationErr, fmt.Errorf("roll back Docker Sandboxes guest host-trust relay: %w", rollbackErr))
				}
			}
			if len(addedPolicyRules) != 0 {
				rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				rollbackErr := p.RemoveNetworkPolicy(rollbackCtx, instance, addedPolicyRules)
				cancel()
				if rollbackErr != nil {
					activationErr = errors.Join(activationErr, fmt.Errorf("roll back exact Docker Sandboxes host-trust relay policy: %w", rollbackErr))
				}
			}
			p.releaseRelayToken(instance.Name)
		}
	}()

	resource := net.JoinHostPort("host.docker.internal", strconv.Itoa(relay.port))
	addedPolicyRules, err = p.applyHostTrustRelayPolicy(ctx, instance, provider.NetworkPolicyRule{
		Name:      "epar-host-trust-relay",
		Decision:  provider.NetworkPolicyAllow,
		Resources: []string{resource},
	})
	if err != nil {
		return fmt.Errorf("allow exact Docker Sandboxes host-trust relay endpoint: %w", err)
	}
	if p.logger != nil {
		p.logger.Info("Docker Sandboxes host-trust relay policy is active", "provider", "docker-sandboxes", "instance", instance.Name)
	}

	configuration := guestRelayConfiguration{
		SchemaVersion: 1,
		RelayAddress:  resource,
		Token:         token,
	}
	payload, err := json.Marshal(configuration)
	if err != nil {
		return fmt.Errorf("encode Docker Sandboxes host-trust relay configuration: %w", err)
	}
	payload = append(payload, '\n')
	guestActivationAttempted = true
	if _, err := p.Exec(ctx, instance, provider.ShellCommand("sudo -n /opt/epar/configure-egress-relay.sh"), provider.ExecOptions{
		Stdin:              string(payload),
		SensitiveValues:    []string{token},
		SuppressTranscript: true,
	}); err != nil {
		return fmt.Errorf("activate Docker Sandboxes host-trust relay: %w", err)
	}
	if p.logger != nil {
		p.logger.Info("Docker Sandboxes guest host-trust relay is active", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	if err := p.verifyHostTrustRelayPolicy(ctx, instance, relay.port, probeStarted); err != nil {
		return err
	}
	if p.logger != nil {
		p.logger.Info("Docker Sandboxes host-trust relay route is verified", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	if err := p.finalizeGuestRelay(ctx, instance, "--commit"); err != nil {
		return fmt.Errorf("commit Docker Sandboxes guest host-trust relay: %w", err)
	}
	configured = true
	if p.logger != nil {
		p.logger.Info("Docker Sandboxes host-trust relay activation complete", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	return nil
}

func (p *Provider) finalizeGuestRelay(ctx context.Context, instance provider.Instance, operation string) error {
	if operation != "--commit" && operation != "--rollback" {
		return fmt.Errorf("unsupported guest relay transaction operation")
	}
	_, err := p.Exec(ctx, instance, provider.ShellCommand("sudo -n /opt/epar/configure-egress-relay.sh "+operation), provider.ExecOptions{SuppressTranscript: true})
	return err
}

func (p *Provider) applyHostTrustRelayPolicy(ctx context.Context, instance provider.Instance, rule provider.NetworkPolicyRule) ([]provider.NetworkPolicyRule, error) {
	before, err := p.ReadNetworkPolicy(ctx, instance)
	if err != nil {
		return nil, err
	}
	beforeIDs := make(map[string]struct{}, len(before))
	for _, existing := range before {
		beforeIDs[existing.ID] = struct{}{}
	}
	applyErr := p.ApplyNetworkPolicy(ctx, instance, []provider.NetworkPolicyRule{rule})
	readbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	after, readErr := p.ReadNetworkPolicy(readbackCtx, instance)
	cancel()
	if readErr != nil {
		return nil, errors.Join(applyErr, fmt.Errorf("read back relay policy delta: %w", readErr))
	}
	added := make([]provider.NetworkPolicyRule, 0, 1)
	for _, candidate := range after {
		if _, existed := beforeIDs[candidate.ID]; existed || candidate.Decision != provider.NetworkPolicyAllow || candidate.ResourceType != "network" || len(candidate.Resources) != 1 || candidate.Resources[0] != rule.Resources[0] || !isRemovableSandboxPolicyRule(candidate, instance.Name) {
			continue
		}
		added = append(added, candidate)
	}
	return added, applyErr
}

func (p *Provider) ensureRelayToken(instanceName string) (*egressRelay, string, error) {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if p.relayTokens == nil {
		p.relayTokens = make(map[string]string)
	}
	if p.relayConnections == nil {
		p.relayConnections = make(map[string]map[net.Conn]struct{})
	}
	if token := p.relayTokens[instanceName]; token != "" && p.relay != nil {
		return p.relay, token, nil
	}
	if p.relay == nil {
		listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(p.hostTrustRelayPort)))
		if err != nil {
			return nil, "", err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		p.relay = &egressRelay{
			listener: listener,
			port:     port,
			provider: p,
			closed:   make(chan struct{}),
			sem:      make(chan struct{}, relayMaxConnections),
		}
		go p.relay.serve()
	}
	token, err := p.newUniqueRelayTokenLocked()
	if err != nil {
		if len(p.relayTokens) == 0 {
			p.relay.close()
			p.relay = nil
		}
		return nil, "", err
	}
	p.relayTokens[instanceName] = token
	return p.relay, token, nil
}

func stableRelayPort(identity string) int {
	digest := sha256.Sum256([]byte(identity))
	return 30000 + int(binary.BigEndian.Uint32(digest[:4])%10000)
}

func (p *Provider) newUniqueRelayTokenLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return "", err
		}
		candidate := base64.RawURLEncoding.EncodeToString(tokenBytes)
		duplicate := false
		for _, existing := range p.relayTokens {
			if subtle.ConstantTimeCompare([]byte(candidate), []byte(existing)) == 1 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate a unique relay credential")
}

func (p *Provider) releaseRelayToken(instanceName string) {
	if instanceName == "" {
		return
	}
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	p.revokeRelayTokenLocked(instanceName)
	if len(p.relayTokens) == 0 && p.relay != nil {
		p.relay.close()
		p.relay = nil
	}
}

func (p *Provider) reconcileRelayTokens(items []provider.InventoryItem) {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if len(p.relayTokens) == 0 {
		return
	}
	present := make(map[string]struct{}, len(items))
	for _, item := range items {
		present[item.Instance.Name] = struct{}{}
	}
	for instanceName := range p.relayTokens {
		if _, exists := present[instanceName]; !exists {
			p.revokeRelayTokenLocked(instanceName)
		}
	}
	if len(p.relayTokens) == 0 && p.relay != nil {
		p.relay.close()
		p.relay = nil
	}
}

func (p *Provider) revokeRelayTokenLocked(instanceName string) {
	delete(p.relayTokens, instanceName)
	for connection := range p.relayConnections[instanceName] {
		_ = connection.Close()
	}
	delete(p.relayConnections, instanceName)
}

func (p *Provider) registerRelayConnection(instanceName string, connection net.Conn) bool {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if p.relayTokens[instanceName] == "" {
		return false
	}
	connections := p.relayConnections[instanceName]
	if connections == nil {
		connections = make(map[net.Conn]struct{})
		p.relayConnections[instanceName] = connections
	}
	connections[connection] = struct{}{}
	return true
}

func (p *Provider) unregisterRelayConnection(instanceName string, connection net.Conn) {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	connections := p.relayConnections[instanceName]
	delete(connections, connection)
	if len(connections) == 0 {
		delete(p.relayConnections, instanceName)
	}
}

func (relay *egressRelay) close() {
	relay.once.Do(func() {
		close(relay.closed)
		_ = relay.listener.Close()
	})
}

func (relay *egressRelay) serve() {
	for {
		connection, err := relay.listener.Accept()
		if err != nil {
			select {
			case <-relay.closed:
				return
			default:
				continue
			}
		}
		select {
		case relay.sem <- struct{}{}:
			go func() {
				defer func() { <-relay.sem }()
				relay.handle(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (relay *egressRelay) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(relayHeaderTimeout))
	reader := bufio.NewReaderSize(connection, relayHeaderLimit+1)
	header, err := readRelayHeader(reader, relayHeaderLimit)
	if err != nil || !strings.HasSuffix(header, "\n") || strings.ContainsRune(strings.TrimSuffix(header, "\n"), '\r') {
		return
	}
	parts := strings.Split(strings.TrimSuffix(header, "\n"), " ")
	if len(parts) != 3 || parts[0] != strings.TrimSpace(relayProtocolPrefix) {
		return
	}
	instanceName, authenticated := relay.authenticate(parts[1])
	if !authenticated {
		return
	}
	if !relay.provider.registerRelayConnection(instanceName, connection) {
		return
	}
	defer relay.provider.unregisterRelayConnection(instanceName, connection)
	if parts[2] == "PING" {
		_, _ = io.WriteString(connection, "PONG\n")
		return
	}
	resolveCtx, cancel := context.WithTimeout(context.Background(), relayDialTimeout)
	destination, err := resolvePublicTLSDestination(resolveCtx, parts[2])
	cancel()
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", destination, relayDialTimeout)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(connection, "OK\n"); err != nil {
		return
	}
	deadline := time.Now().Add(relayIdleTimeout)
	_ = connection.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(connection, upstream)
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	wait.Wait()
}

func readRelayHeader(reader *bufio.Reader, limit int) (string, error) {
	if reader == nil || limit <= 0 {
		return "", fmt.Errorf("invalid relay header reader")
	}
	header := make([]byte, 0, min(limit, 256))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > limit-len(header) {
			return "", fmt.Errorf("relay header exceeded limit")
		}
		header = append(header, fragment...)
		if err == nil {
			return string(header), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

func (relay *egressRelay) authenticate(candidate string) (string, bool) {
	relay.provider.relayMu.Lock()
	defer relay.provider.relayMu.Unlock()
	for instanceName, expected := range relay.provider.relayTokens {
		if len(candidate) == len(expected) && subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1 {
			return instanceName, true
		}
	}
	return "", false
}

func resolvePublicTLSDestination(ctx context.Context, target string) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port != "443" || len(host) > 253 || strings.ContainsAny(host, "\x00\r\n\t /") {
		return "", fmt.Errorf("relay destination must be a public host on port 443")
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return "", fmt.Errorf("resolve relay destination")
	}
	return selectPublicTLSDestination(addresses, port)
}

func selectPublicTLSDestination(addresses []netip.Addr, port string) (string, error) {
	var ipv6Fallback string
	for _, address := range addresses {
		address = address.Unmap()
		if !relayPublicAddress(address) {
			continue
		}
		destination := net.JoinHostPort(address.String(), port)
		if address.Is4() {
			return destination, nil
		}
		if ipv6Fallback == "" {
			ipv6Fallback = destination
		}
	}
	if ipv6Fallback != "" {
		return ipv6Fallback, nil
	}
	return "", fmt.Errorf("relay destination did not resolve to a permitted public address")
}

func relayPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalMulticast() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	blocked := []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("fec0::/10"),
	}
	for _, prefix := range blocked {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var _ provider.HostTrustRuntimeActivator = (*Provider)(nil)
