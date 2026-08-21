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
	relayProtocolPrefix    = "EPAR1 "
	relayHeaderLimit       = 1024
	relayMaxConnections    = 128
	relayDialTimeout       = 15 * time.Second
	relayHeaderTimeout     = 10 * time.Second
	relayIdleTimeout       = 5 * time.Minute
	guestRelayPort         = 3129
	guestRelayProbeTimeout = 8 * time.Second
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

type relayTokenBinding struct {
	ProviderID  string
	Token       string
	Epoch       uint64
	PolicyRules map[string]struct{}
}

type relayBindingSnapshot struct {
	Instance    provider.Instance
	Token       string
	Epoch       uint64
	Relay       *egressRelay
	Port        int
	PolicyRules map[string]struct{}
}

const hostTrustRelayVerificationScript = `set -euo pipefail
test -f /run/epar/egress-relay-active
test ! -L /run/epar/egress-relay-active
test "$(stat -c '%U:%G:%a' /run/epar/egress-relay-active)" = "root:root:444"
test "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --connect-timeout 1 --max-time 2 http://127.0.0.1:3129/health)" = "204"
registry_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --connect-timeout 2 --max-time 5 --proxy http://127.0.0.1:3129 --noproxy '' --cacert /usr/local/share/ca-certificates/epar/epar-egress-relay.crt https://registry-1.docker.io/v2/)"
test "${registry_status}" = "401"`

func (p *Provider) ActivateHostTrustRuntime(ctx context.Context, instance provider.Instance) (activationErr error) {
	if !p.hostTrustRelayEnabled {
		if p.logger != nil {
			p.logger.Debug("Docker Sandboxes host-trust relay activation skipped because the Windows overlay relay is disabled", "provider", "docker-sandboxes", "instance", instance.Name)
		}
		return nil
	}
	if err := validateInstance(instance, true); err != nil {
		return err
	}
	releaseInstanceOperation := p.lockInstanceOperation(instance.Name)
	defer releaseInstanceOperation()
	if p.logger != nil {
		p.logger.Debug(fmt.Sprintf("Docker Sandboxes host-trust relay activation started on controller port %d", p.hostTrustRelayPort), "provider", "docker-sandboxes", "instance", instance.Name)
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	binding, err := p.ensureRelayToken(instance)
	if err != nil {
		return fmt.Errorf("prepare Docker Sandboxes host-trust relay: %w", err)
	}
	relay := binding.Relay
	token := binding.Token
	configured := false
	guestActivationAttempted := false
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
			p.releaseRelayToken(binding)
		}
	}()

	resource := net.JoinHostPort("host.docker.internal", strconv.Itoa(relay.port))
	var policyRuleNames []string
	addedPolicyRules, policyRuleNames, err = p.applyHostTrustRelayPolicy(ctx, instance, provider.NetworkPolicyRule{
		Name:      "epar-host-trust-relay",
		Decision:  provider.NetworkPolicyAllow,
		Resources: []string{resource},
	})
	if err != nil {
		return fmt.Errorf("allow exact Docker Sandboxes host-trust relay endpoint: %w", err)
	}
	binding, err = p.bindRelayPolicyRules(binding, policyRuleNames)
	if err != nil {
		return err
	}
	if p.logger != nil {
		p.logger.Debug("Docker Sandboxes host-trust relay policy is active", "provider", "docker-sandboxes", "instance", instance.Name)
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
	probeStarted := hostTrustRelayPolicyProbeStart()
	if _, err := p.Exec(ctx, instance, provider.ShellCommand("sudo -n /opt/epar/configure-egress-relay.sh"), provider.ExecOptions{
		Stdin:              string(payload),
		SensitiveValues:    []string{token},
		SuppressTranscript: true,
	}); err != nil {
		return fmt.Errorf("activate Docker Sandboxes host-trust relay: %w", err)
	}
	if err := p.verifyRelayBinding(binding); err != nil {
		return err
	}
	if p.logger != nil {
		p.logger.Debug("Docker Sandboxes guest host-trust relay is active", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	if err := p.verifyBoundHostTrustRelayPolicy(ctx, binding, probeStarted); err != nil {
		return err
	}
	if err := p.verifyExactRelayInstance(ctx, binding); err != nil {
		return err
	}
	if p.logger != nil {
		p.logger.Debug("Docker Sandboxes host-trust relay route is verified", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	if err := p.finalizeGuestRelay(ctx, instance, "--commit"); err != nil {
		return fmt.Errorf("commit Docker Sandboxes guest host-trust relay: %w", err)
	}
	configured = true
	if p.logger != nil {
		p.logger.Debug("Docker Sandboxes host-trust relay activation complete", "provider", "docker-sandboxes", "instance", instance.Name)
	}
	return nil
}

// VerifyHostTrustRuntime performs only read-only checks for the controller
// relay and the exact guest identity. It deliberately does not recreate relay
// credentials or invoke the destructive guest configuration transaction.
func (p *Provider) VerifyHostTrustRuntime(ctx context.Context, instance provider.Instance) error {
	if !p.hostTrustRelayEnabled {
		return nil
	}
	if err := validateInstance(instance, true); err != nil {
		return err
	}
	releaseInstanceOperation := p.lockInstanceOperation(instance.Name)
	defer releaseInstanceOperation()
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	binding, err := p.currentRelayBinding(instance)
	if err != nil {
		return err
	}
	if len(binding.PolicyRules) == 0 {
		return fmt.Errorf("Docker Sandboxes host-trust relay policy proof is not bound to the exact instance")
	}

	probeStarted := hostTrustRelayPolicyProbeStart()
	probeCtx, cancelProbe := context.WithTimeout(ctx, guestRelayProbeTimeout)
	result, err := p.Exec(probeCtx, instance, provider.ShellCommand(hostTrustRelayVerificationScript), provider.ExecOptions{
		SensitiveValues:    []string{binding.Token},
		SuppressTranscript: true,
	})
	cancelProbe()
	if err != nil {
		return fmt.Errorf("verify Docker Sandboxes host-trust relay from the exact instance: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("Docker Sandboxes host-trust relay verification returned unexpected output")
	}
	if err := p.verifyRelayBinding(binding); err != nil {
		return err
	}
	if err := p.verifyBoundHostTrustRelayPolicy(ctx, binding, probeStarted); err != nil {
		return err
	}
	return p.verifyExactRelayInstance(ctx, binding)
}

func (p *Provider) finalizeGuestRelay(ctx context.Context, instance provider.Instance, operation string) error {
	if operation != "--commit" && operation != "--rollback" {
		return fmt.Errorf("unsupported guest relay transaction operation")
	}
	_, err := p.Exec(ctx, instance, provider.ShellCommand("sudo -n /opt/epar/configure-egress-relay.sh "+operation), provider.ExecOptions{SuppressTranscript: true})
	return err
}

func (p *Provider) applyHostTrustRelayPolicy(ctx context.Context, instance provider.Instance, rule provider.NetworkPolicyRule) ([]provider.NetworkPolicyRule, []string, error) {
	before, err := p.ReadNetworkPolicy(ctx, instance)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, errors.Join(applyErr, fmt.Errorf("read back relay policy delta: %w", readErr))
	}
	added := make([]provider.NetworkPolicyRule, 0, 1)
	policyRuleNames := make([]string, 0, 1)
	for _, candidate := range after {
		if candidate.Active && candidate.Name != "" && candidate.Decision == provider.NetworkPolicyAllow && candidate.ResourceType == "network" && len(candidate.Resources) == 1 && candidate.Resources[0] == rule.Resources[0] && isSandboxPolicyTarget(candidate.Scope, candidate.AppliesTo, instance.Name) {
			policyRuleNames = append(policyRuleNames, candidate.Name)
		}
		if _, existed := beforeIDs[candidate.ID]; existed || candidate.Decision != provider.NetworkPolicyAllow || candidate.ResourceType != "network" || len(candidate.Resources) != 1 || candidate.Resources[0] != rule.Resources[0] || !isRemovableSandboxPolicyRule(candidate, instance.Name) {
			continue
		}
		added = append(added, candidate)
	}
	if len(policyRuleNames) == 0 {
		return added, nil, errors.Join(applyErr, fmt.Errorf("Docker Sandboxes policy readback did not identify the exact active relay allow rule"))
	}
	return added, policyRuleNames, applyErr
}

func hostTrustRelayPolicyProbeStart() time.Time {
	// Docker Sandboxes policy logs may serialize timestamps at whole-second
	// precision. Truncation permits only the sub-second representation gap.
	return time.Now().UTC().Truncate(time.Second)
}

func (p *Provider) verifyBoundHostTrustRelayPolicy(ctx context.Context, binding relayBindingSnapshot, startedAt time.Time) error {
	if binding.Port <= 0 || len(binding.PolicyRules) == 0 {
		return fmt.Errorf("Docker Sandboxes host-trust relay policy proof is not bound to the exact instance")
	}
	result, err := p.run(ctx, commandRequest{
		args:        []string{"policy", "log", binding.Instance.Name, "--json"},
		operation:   "verify Docker Sandboxes host-trust relay route",
		outputLimit: diagnosticOutputLimit,
		timeout:     providerReadbackTimeout,
	})
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	var document policyLogDocument
	if err := decoder.Decode(&document); err != nil || requireJSONEOF(decoder) != nil {
		return fmt.Errorf("Docker Sandboxes policy log returned an unsupported json schema")
	}
	relayHosts := map[string]struct{}{
		net.JoinHostPort("localhost", strconv.Itoa(binding.Port)):            {},
		net.JoinHostPort("host.docker.internal", strconv.Itoa(binding.Port)): {},
	}
	for _, record := range document.Blocked {
		if record.VMName != binding.Instance.Name || record.LastSeen.Before(startedAt) {
			continue
		}
		if _, exactRelay := relayHosts[record.Host]; exactRelay {
			return fmt.Errorf("Docker Sandboxes blocked the exact EPAR host-trust relay endpoint")
		}
	}
	foundTransparentRelay := false
	for _, record := range document.Allowed {
		if record.VMName != binding.Instance.Name || record.LastSeen.Before(startedAt) {
			continue
		}
		if record.Host == "registry-1.docker.io:443" && record.ProxyType == "forward" {
			return fmt.Errorf("Docker Sandboxes routed the relay registry proof through credential-bearing forward egress")
		}
		if _, exactRelay := relayHosts[record.Host]; !exactRelay {
			continue
		}
		if record.ProxyType != "transparent" {
			return fmt.Errorf("Docker Sandboxes host-trust relay used unexpected %q routing", record.ProxyType)
		}
		if _, expectedRule := binding.PolicyRules[record.Rule]; !expectedRule {
			return fmt.Errorf("Docker Sandboxes host-trust relay matched unexpected policy rule %q", record.Rule)
		}
		foundTransparentRelay = true
	}
	if !foundTransparentRelay {
		return fmt.Errorf("Docker Sandboxes policy log did not confirm fresh transparent routing for the exact EPAR host-trust relay endpoint and bound policy rule")
	}
	return nil
}

func (p *Provider) ensureRelayToken(instance provider.Instance) (relayBindingSnapshot, error) {
	if err := validateInstance(instance, true); err != nil {
		return relayBindingSnapshot{}, err
	}
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if p.relayTokens == nil {
		p.relayTokens = make(map[string]relayTokenBinding)
	}
	if p.relayConnections == nil {
		p.relayConnections = make(map[string]map[net.Conn]struct{})
	}
	if binding, exists := p.relayTokens[instance.Name]; exists && binding.ProviderID == instance.ProviderID && binding.Token != "" && binding.Epoch != 0 && p.relay != nil {
		return relayBindingSnapshotLocked(instance, binding, p.relay), nil
	}
	if _, exists := p.relayTokens[instance.Name]; exists {
		p.revokeRelayTokenLocked(instance.Name)
	}
	if p.relay == nil {
		listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(p.hostTrustRelayPort)))
		if err != nil {
			return relayBindingSnapshot{}, err
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
		return relayBindingSnapshot{}, err
	}
	p.relayEpoch++
	if p.relayEpoch == 0 {
		p.relayEpoch++
	}
	binding := relayTokenBinding{ProviderID: instance.ProviderID, Token: token, Epoch: p.relayEpoch}
	p.relayTokens[instance.Name] = binding
	return relayBindingSnapshotLocked(instance, binding, p.relay), nil
}

func relayBindingSnapshotLocked(instance provider.Instance, binding relayTokenBinding, relay *egressRelay) relayBindingSnapshot {
	policyRules := make(map[string]struct{}, len(binding.PolicyRules))
	for rule := range binding.PolicyRules {
		policyRules[rule] = struct{}{}
	}
	port := 0
	if relay != nil {
		port = relay.port
	}
	return relayBindingSnapshot{Instance: instance, Token: binding.Token, Epoch: binding.Epoch, Relay: relay, Port: port, PolicyRules: policyRules}
}

func (p *Provider) currentRelayBinding(instance provider.Instance) (relayBindingSnapshot, error) {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[instance.Name]
	if !exists || binding.ProviderID != instance.ProviderID || binding.Token == "" || binding.Epoch == 0 || p.relay == nil || p.relay.port <= 0 {
		return relayBindingSnapshot{}, fmt.Errorf("Docker Sandboxes host-trust relay is not bound to the exact instance")
	}
	return relayBindingSnapshotLocked(instance, binding, p.relay), nil
}

func (p *Provider) bindRelayPolicyRules(snapshot relayBindingSnapshot, ruleNames []string) (relayBindingSnapshot, error) {
	policyRules := make(map[string]struct{}, len(ruleNames))
	for _, ruleName := range ruleNames {
		if ruleName == "" {
			return relayBindingSnapshot{}, fmt.Errorf("Docker Sandboxes host-trust relay policy readback omitted the matched rule identity")
		}
		policyRules[ruleName] = struct{}{}
	}
	if len(policyRules) == 0 {
		return relayBindingSnapshot{}, fmt.Errorf("Docker Sandboxes host-trust relay policy proof is unavailable")
	}
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[snapshot.Instance.Name]
	if !exists || !relayBindingMatchesSnapshot(binding, p.relay, snapshot) {
		return relayBindingSnapshot{}, fmt.Errorf("Docker Sandboxes host-trust relay binding changed during activation")
	}
	binding.PolicyRules = policyRules
	p.relayTokens[snapshot.Instance.Name] = binding
	return relayBindingSnapshotLocked(snapshot.Instance, binding, p.relay), nil
}

func (p *Provider) verifyRelayBinding(snapshot relayBindingSnapshot) error {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[snapshot.Instance.Name]
	if !exists || !relayBindingMatchesSnapshot(binding, p.relay, snapshot) {
		return fmt.Errorf("Docker Sandboxes host-trust relay binding changed during exact-instance verification")
	}
	return nil
}

func relayBindingMatchesSnapshot(binding relayTokenBinding, relay *egressRelay, snapshot relayBindingSnapshot) bool {
	return relay != nil && relay == snapshot.Relay && relay.port == snapshot.Port && binding.ProviderID == snapshot.Instance.ProviderID && binding.Token == snapshot.Token && binding.Epoch == snapshot.Epoch
}

func (p *Provider) verifyExactRelayInstance(ctx context.Context, snapshot relayBindingSnapshot) error {
	present, err := p.assertIdentity(ctx, snapshot.Instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	return p.verifyRelayBinding(snapshot)
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
			if subtle.ConstantTimeCompare([]byte(candidate), []byte(existing.Token)) == 1 {
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

func (p *Provider) releaseRelayToken(snapshot relayBindingSnapshot) {
	if snapshot.Instance.Name == "" || snapshot.Instance.ProviderID == "" || snapshot.Epoch == 0 || snapshot.Token == "" {
		return
	}
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[snapshot.Instance.Name]
	if !exists || !relayBindingMatchesSnapshot(binding, p.relay, snapshot) {
		return
	}
	p.revokeRelayTokenLocked(snapshot.Instance.Name)
	if len(p.relayTokens) == 0 && p.relay != nil {
		p.relay.close()
		p.relay = nil
	}
}

func (p *Provider) releaseRelayTokenForInstance(instance provider.Instance) {
	if instance.Name == "" || instance.ProviderID == "" {
		return
	}
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[instance.Name]
	if !exists || binding.ProviderID != instance.ProviderID {
		return
	}
	p.revokeRelayTokenLocked(instance.Name)
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
	present := make(map[string]string, len(items))
	for _, item := range items {
		present[item.Instance.Name] = item.Instance.ProviderID
	}
	for instanceName, binding := range p.relayTokens {
		providerID, exists := present[instanceName]
		if !exists || providerID != binding.ProviderID {
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

func (p *Provider) registerRelayConnection(instanceName string, epoch uint64, connection net.Conn) bool {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	binding, exists := p.relayTokens[instanceName]
	if !exists || binding.Epoch != epoch {
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
	instanceName, bindingEpoch, authenticated := relay.authenticate(parts[1])
	if !authenticated {
		return
	}
	if !relay.provider.registerRelayConnection(instanceName, bindingEpoch, connection) {
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

func (relay *egressRelay) authenticate(candidate string) (string, uint64, bool) {
	relay.provider.relayMu.Lock()
	defer relay.provider.relayMu.Unlock()
	for instanceName, expected := range relay.provider.relayTokens {
		if len(candidate) == len(expected.Token) && subtle.ConstantTimeCompare([]byte(candidate), []byte(expected.Token)) == 1 {
			return instanceName, expected.Epoch, true
		}
	}
	return "", 0, false
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
var _ provider.HostTrustRuntimeVerifier = (*Provider)(nil)
