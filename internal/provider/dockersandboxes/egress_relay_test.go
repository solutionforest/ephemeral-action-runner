package dockersandboxes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestReadRelayHeaderDoesNotLimitTunnelPayload(t *testing.T) {
	payload := bytes.Repeat([]byte("tls-payload-"), relayHeaderLimit)
	reader := bufio.NewReader(bytes.NewReader(append([]byte("EPAR1 token target:443\n"), payload...)))
	header, err := readRelayHeader(reader, relayHeaderLimit)
	if err != nil {
		t.Fatal(err)
	}
	if header != "EPAR1 token target:443\n" {
		t.Fatalf("header = %q", header)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remaining, payload) {
		t.Fatalf("remaining tunnel payload = %d bytes, want %d", len(remaining), len(payload))
	}
}

func TestEgressRelayAuthenticatesHealthWithoutExposingToken(t *testing.T) {
	p := NewWithDryRun("sbx", false)
	relay, token, err := p.ensureRelayToken("sandbox-one")
	if err != nil {
		t.Fatal(err)
	}
	defer p.releaseRelayToken("sandbox-one")
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43", len(token))
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", relay.port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "EPAR1 %s PING\n", token); err != nil {
		t.Fatal(err)
	}
	response, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if response != "PONG\n" {
		t.Fatalf("response = %q, want PONG", response)
	}
	if len(p.relayTokens) != 1 {
		t.Fatalf("relay token count = %d, want 1", len(p.relayTokens))
	}
}

func TestEgressRelayRejectsUnknownToken(t *testing.T) {
	p := NewWithDryRun("sbx", false)
	relay, _, err := p.ensureRelayToken("sandbox-one")
	if err != nil {
		t.Fatal(err)
	}
	defer p.releaseRelayToken("sandbox-one")
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", relay.port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprintf(connection, "EPAR1 %s PING\n", strings.Repeat("A", 43))
	response, _ := bufio.NewReader(connection).ReadString('\n')
	if response != "" {
		t.Fatalf("unauthenticated response = %q, want empty", response)
	}
}

func TestRelayTokenRevocationClosesAuthenticatedConnections(t *testing.T) {
	p := NewWithDryRun("sbx", false)
	if _, _, err := p.ensureRelayToken("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close()
	if !p.registerRelayConnection("sandbox-one", server) {
		t.Fatal("registerRelayConnection() = false")
	}
	p.releaseRelayToken("sandbox-one")
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("probe")); err == nil {
		t.Fatal("revoked relay connection remained writable")
	}
}

func TestStableRelayPortIsDeterministicAndScoped(t *testing.T) {
	first := stableRelayPort(`D:\repo\one` + "\x00" + "epar-one")
	if first < 30000 || first > 39999 {
		t.Fatalf("stable relay port = %d, want 30000-39999", first)
	}
	if repeated := stableRelayPort(`D:\repo\one` + "\x00" + "epar-one"); repeated != first {
		t.Fatalf("stable relay port changed: %d != %d", repeated, first)
	}
	if other := stableRelayPort(`D:\repo\two` + "\x00" + "epar-two"); other == first {
		t.Fatalf("test identities unexpectedly collided at port %d", first)
	}
}

func TestHostTrustRelayActivationFailureRollsBackExactAddedPolicy(t *testing.T) {
	const ruleID = "11111111-1111-1111-1111-111111111111"
	p := NewWithDryRun("sbx", false)
	p.ConfigureHostTrustRelay(true, "rollback-test")
	rulePresent := false
	removed := false
	rolledBack := false
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		args := strings.Join(request.args, " ")
		switch {
		case args == "ls --json":
			return provider.ExecResult{Stdout: readyListJSON}, nil
		case args == "policy ls "+testName+" --include-inactive --json":
			rules := "[]"
			if rulePresent {
				resource := net.JoinHostPort("host.docker.internal", fmt.Sprint(p.hostTrustRelayPort))
				rules = fmt.Sprintf(`[{"id":%q,"name":"host relay","policy_id":"local","scope":%q,"applies_to":%q,"resource_type":"network","decision":"allow","resources":[%q],"origin":"scoped","status":"active","editable":true,"sandbox_id":%q}]`, ruleID, "sandbox:"+testName, "sandbox:"+testName, resource, testName)
			}
			return provider.ExecResult{Stdout: policyFixture(rules)}, nil
		case strings.HasPrefix(args, "policy allow network --sandbox "+testName+" host.docker.internal:"):
			rulePresent = true
			return provider.ExecResult{}, nil
		case args == "policy rm network --sandbox "+testName+" --id "+ruleID:
			rulePresent = false
			removed = true
			return provider.ExecResult{}, nil
		case strings.HasPrefix(args, "exec -i "+testName+" -- bash -lc ") && strings.Contains(args, "/opt/epar/configure-egress-relay.sh --rollback"):
			rolledBack = true
			return provider.ExecResult{}, nil
		case strings.HasPrefix(args, "exec -i "+testName+" -- bash -lc ") && strings.Contains(args, "/opt/epar/configure-egress-relay.sh"):
			return provider.ExecResult{}, errors.New("guest activation failed")
		default:
			t.Fatalf("unexpected command: %v", request.args)
			return provider.ExecResult{}, nil
		}
	}

	err := p.ActivateHostTrustRuntime(context.Background(), testInstance)
	if err == nil || !strings.Contains(err.Error(), "guest activation failed") {
		t.Fatalf("activation error = %v, want guest failure", err)
	}
	if rulePresent || !removed || !rolledBack {
		t.Fatalf("rollback state = policy present %t policy removed %t guest rolled back %t", rulePresent, removed, rolledBack)
	}
	if len(p.relayTokens) != 0 || p.relay != nil {
		t.Fatalf("relay credentials survived failed activation: tokens=%d relay=%v", len(p.relayTokens), p.relay)
	}
}

func TestHostTrustRelayActivationCommitsOnlyAfterFreshPolicyProof(t *testing.T) {
	const ruleID = "22222222-2222-2222-2222-222222222222"
	p := NewWithDryRun("sbx", false)
	p.ConfigureHostTrustRelay(true, "commit-test")
	rulePresent := false
	committed := false
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		args := strings.Join(request.args, " ")
		resource := net.JoinHostPort("host.docker.internal", fmt.Sprint(p.hostTrustRelayPort))
		switch {
		case args == "ls --json":
			return provider.ExecResult{Stdout: readyListJSON}, nil
		case args == "policy ls "+testName+" --include-inactive --json":
			rules := "[]"
			if rulePresent {
				rules = fmt.Sprintf(`[{"id":%q,"name":"host relay","policy_id":"local","scope":%q,"applies_to":%q,"resource_type":"network","decision":"allow","resources":[%q],"origin":"scoped","status":"active","editable":true,"sandbox_id":%q}]`, ruleID, "sandbox:"+testName, "sandbox:"+testName, resource, testName)
			}
			return provider.ExecResult{Stdout: policyFixture(rules)}, nil
		case args == "policy allow network --sandbox "+testName+" "+resource:
			rulePresent = true
			return provider.ExecResult{}, nil
		case strings.HasPrefix(args, "exec -i "+testName+" -- bash -lc ") && strings.Contains(args, "/opt/epar/configure-egress-relay.sh --commit"):
			committed = true
			return provider.ExecResult{}, nil
		case strings.HasPrefix(args, "exec -i "+testName+" -- bash -lc ") && strings.Contains(args, "/opt/epar/configure-egress-relay.sh"):
			return provider.ExecResult{}, nil
		case args == "policy log "+testName+" --json":
			now := time.Now().UTC()
			return provider.ExecResult{Stdout: fmt.Sprintf(`{"blocked_hosts":[],"allowed_hosts":[%s]}`, policyLogEntry(net.JoinHostPort("localhost", fmt.Sprint(p.hostTrustRelayPort)), testName, "transparent", now))}, nil
		default:
			t.Fatalf("unexpected command: %v", request.args)
			return provider.ExecResult{}, nil
		}
	}

	if err := p.ActivateHostTrustRuntime(context.Background(), testInstance); err != nil {
		t.Fatal(err)
	}
	defer p.releaseRelayToken(testName)
	if !committed || !rulePresent || len(p.relayTokens) != 1 || p.relay == nil {
		t.Fatalf("committed activation state = commit %t policy %t tokens %d relay %v", committed, rulePresent, len(p.relayTokens), p.relay)
	}
}

func TestRelayPublicAddressRejectsNonPublicAndSpecialRanges(t *testing.T) {
	rejected := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.1.1",
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "::1", "fe80::1", "fec0::1", "2001:2::1", "2001:db8::1",
	}
	for _, value := range rejected {
		if relayPublicAddress(netip.MustParseAddr(value)) {
			t.Errorf("relayPublicAddress(%s) = true, want false", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !relayPublicAddress(netip.MustParseAddr(value)) {
			t.Errorf("relayPublicAddress(%s) = false, want true", value)
		}
	}
}

func TestSelectPublicTLSDestinationPrefersIPv4WithIPv6Fallback(t *testing.T) {
	destination, err := selectPublicTLSDestination([]netip.Addr{
		netip.MustParseAddr("2606:4700:4700::1111"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("1.1.1.1"),
	}, "443")
	if err != nil {
		t.Fatal(err)
	}
	if destination != "1.1.1.1:443" {
		t.Fatalf("destination = %q, want IPv4", destination)
	}
	destination, err = selectPublicTLSDestination([]netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, "443")
	if err != nil {
		t.Fatal(err)
	}
	if destination != "[2606:4700:4700::1111]:443" {
		t.Fatalf("destination = %q, want IPv6 fallback", destination)
	}
}

func TestVerifyHostTrustRelayPolicyRequiresFreshTransparentExactPort(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Microsecond)
	instance := provider.Instance{Name: "sandbox-one", ProviderID: "12345678-1234-1234-1234-123456789abc"}
	for _, test := range []struct {
		name    string
		allowed string
		blocked string
		wantErr string
	}{
		{name: "accepted", allowed: policyLogEntry("localhost:43123", "sandbox-one", "transparent", started), blocked: ""},
		{name: "stale", allowed: policyLogEntry("localhost:43123", "sandbox-one", "transparent", started.Add(-time.Minute)), wantErr: "did not confirm"},
		{name: "wrong route", allowed: policyLogEntry("localhost:43123", "sandbox-one", "forward", started), wantErr: "unexpected"},
		{name: "blocked", allowed: policyLogEntry("localhost:43123", "sandbox-one", "transparent", started), blocked: policyLogEntry("localhost:43123", "sandbox-one", "transparent", started), wantErr: "blocked"},
		{name: "credential forward", allowed: policyLogEntry("registry-1.docker.io:443", "sandbox-one", "forward", started) + "," + policyLogEntry("localhost:43123", "sandbox-one", "transparent", started), wantErr: "credential-bearing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewWithDryRun("sbx", false)
			p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
				if strings.Join(request.args, " ") != "policy log sandbox-one --json" {
					t.Fatalf("unexpected command: %v", request.args)
				}
				return provider.ExecResult{Stdout: fmt.Sprintf(`{"blocked_hosts":[%s],"allowed_hosts":[%s]}`, test.blocked, test.allowed)}, nil
			}
			err := p.verifyHostTrustRelayPolicy(context.Background(), instance, 43123, started)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func policyLogEntry(host, vmName, proxyType string, lastSeen time.Time) string {
	return fmt.Sprintf(`{"host":%q,"vm_name":%q,"proxy_type":%q,"rule":"","last_seen":%q,"since":%q,"count_since":1}`, host, vmName, proxyType, lastSeen.Format(time.RFC3339Nano), lastSeen.Format(time.RFC3339Nano))
}

func TestValidatePolicyCommandRejectsBroadPolicyAccess(t *testing.T) {
	accepted := [][]string{
		{"policy", "log", "sandbox-one", "--json"},
		{"policy", "ls", "--include-inactive", "--json"},
		{"policy", "ls", "sandbox-one", "--include-inactive", "--json"},
		{"policy", "allow", "network", "--sandbox", "sandbox-one", "host.docker.internal:43123"},
		{"policy", "rm", "network", "--sandbox", "sandbox-one", "--id", "12345678-1234-1234-1234-123456789abc"},
	}
	for _, args := range accepted {
		if err := validatePolicyCommand(args); err != nil {
			t.Errorf("validatePolicyCommand(%v) rejected: %v", args, err)
		}
	}
	for _, args := range [][]string{{"policy", "log"}, {"policy", "set", "global"}, {"policy", "log", "sandbox-one"}, {"policy", "allow", "filesystem", "--sandbox", "sandbox-one", "**"}} {
		if err := validatePolicyCommand(args); err == nil {
			t.Errorf("validatePolicyCommand(%v) accepted", args)
		}
	}
}
