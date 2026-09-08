package dockersandboxes

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestApplyNetworkPolicyProviderDeadlineIsRecoveryAuthorizing(t *testing.T) {
	rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com"}}
	for _, test := range []struct {
		name  string
		steps []commandStep
	}{
		{
			name: "identity inventory",
			steps: []commandStep{
				{args: []string{"ls", "--json"}, err: context.DeadlineExceeded},
			},
		},
		{
			name: "policy mutation",
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}, err: context.DeadlineExceeded},
			},
		},
		{
			name: "policy readback",
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}},
				{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, err: context.DeadlineExceeded},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, test.steps...)
			err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{rule})
			if err == nil || !errors.Is(err, provider.ErrControlPlaneAdmissionFailure) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("ApplyNetworkPolicy() error = %v, want provider-owned deadline admission failure", err)
			}
			done()
		})
	}
}

func TestApplyNetworkPolicyCallerCancellationIsNotRecoveryAuthorizing(t *testing.T) {
	rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com"}}
	ctx, cancel := context.WithCancel(context.Background())
	p := New("sbx-test-double")
	commands := 0
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		commands++
		switch commands {
		case 1:
			if !reflect.DeepEqual(request.args, []string{"ls", "--json"}) {
				t.Fatalf("identity command = %#v", request.args)
			}
			return provider.ExecResult{Stdout: readyListJSON}, nil
		case 2:
			if !reflect.DeepEqual(request.args, []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}) {
				t.Fatalf("policy command = %#v", request.args)
			}
			cancel()
			return provider.ExecResult{}, context.DeadlineExceeded
		default:
			t.Fatalf("unexpected command: %#v", request.args)
			return provider.ExecResult{}, nil
		}
	}

	err := p.ApplyNetworkPolicy(ctx, testInstance, []provider.NetworkPolicyRule{rule})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyNetworkPolicy() error = %v, want caller cancellation", err)
	}
	if errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("ApplyNetworkPolicy() error = %v, caller cancellation was recovery-authorizing", err)
	}
	if commands != 2 {
		t.Fatalf("commands = %d, want 2", commands)
	}
}

func TestApplyNetworkPolicyCallerDeadlineIsNotRecoveryAuthorizing(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	p := New("sbx-test-double")
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		t.Fatalf("caller-owned deadline invoked command: %#v", request.args)
		return provider.ExecResult{}, nil
	}

	err := p.ApplyNetworkPolicy(ctx, testInstance, []provider.NetworkPolicyRule{{
		Decision:  provider.NetworkPolicyAllow,
		Resources: []string{"api.example.com"},
	}})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ApplyNetworkPolicy() error = %v, want caller-owned context error", err)
	}
	if errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("ApplyNetworkPolicy() error = %v, caller-owned context was recovery-authorizing", err)
	}
}

func TestApplyNetworkPolicyNonTimeoutFailuresAreNotRecoveryAuthorizing(t *testing.T) {
	validRule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com"}}
	for _, test := range []struct {
		name    string
		rule    provider.NetworkPolicyRule
		steps   []commandStep
		wantErr string
	}{
		{
			name: "identity mismatch",
			rule: validRule,
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: strings.Replace(readyListJSON, testID, "8a403313-9a12-4a73-b8ad-a16980049d0c", 1)}},
			},
			wantErr: "identity changed",
		},
		{
			name: "rule validation",
			rule: provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"127.0.0.1"}},
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			},
			wantErr: "invalid docker sandbox network resource",
		},
		{
			name: "ordinary mutation failure",
			rule: validRule,
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}, err: errors.New("network policy service unavailable")},
			},
			wantErr: "network policy service unavailable",
		},
		{
			name: "malformed readback",
			rule: validRule,
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}},
				{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: `{"rules":`}},
			},
			wantErr: "unsupported json schema",
		},
		{
			name: "applied rule missing from readback",
			rule: validRule,
			steps: []commandStep{
				{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}},
				{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyFixture(`[]`)}},
			},
			wantErr: "did not contain an applied rule",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, test.steps...)
			err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{test.rule})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ApplyNetworkPolicy() error = %v, want %q", err, test.wantErr)
			}
			if errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
				t.Fatalf("ApplyNetworkPolicy() error = %v, non-timeout failure was recovery-authorizing", err)
			}
			done()
		})
	}
}
