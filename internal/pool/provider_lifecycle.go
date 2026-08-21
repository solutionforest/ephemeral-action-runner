package pool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func (m *Manager) copyTextGuest(ctx context.Context, name, path, mode, content string, atomic bool) error {
	staging := "/tmp/epar-copy"
	script := fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s; else install -m %s %s %s; fi && rm -f %s", shellQuote(staging), shellQuote(mode), shellQuote(staging), shellQuote(path), shellQuote(mode), shellQuote(staging), shellQuote(path), shellQuote(staging))
	if atomic {
		temporary := path + ".tmp"
		script = fmt.Sprintf("cat > %s && if command -v sudo >/dev/null 2>&1; then sudo install -m %s %s %s && sudo mv -f %s %s; else install -m %s %s %s && mv -f %s %s; fi && rm -f %s", shellQuote(staging), shellQuote(mode), shellQuote(staging), shellQuote(temporary), shellQuote(temporary), shellQuote(path), shellQuote(mode), shellQuote(staging), shellQuote(temporary), shellQuote(temporary), shellQuote(path), shellQuote(staging))
	}
	_, err := m.execGuest(ctx, name, provider.ShellCommand(script), provider.ExecOptions{Stdin: content})
	return err
}

func (m *Manager) createProviderInstance(ctx context.Context, name string) (provider.Instance, error) {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return provider.Instance{}, fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Create(ctx, provider.CreateRequest{
		Name:        name,
		Source:      m.Config.Provider.SourceImage,
		StagingPath: filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.DockerSandboxes.StagingRoot), name),
		CPUs:        m.Config.DockerSandboxes.CPUs,
		Memory:      m.Config.DockerSandboxes.Memory,
		RootDisk:    m.Config.DockerSandboxes.RootDisk,
		DockerDisk:  m.Config.DockerSandboxes.DockerDisk,
	})
}

func (m *Manager) startProviderInstance(ctx context.Context, instance provider.Instance, opts provider.StartOptions) (*provider.RunningProcess, error) {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return nil, fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Start(ctx, instance, opts)
}

func (m *Manager) providerAddress(ctx context.Context, instance provider.Instance, waitSeconds int) (string, bool, error) {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return "", false, fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Address(ctx, instance, waitSeconds)
}

func (m *Manager) verifyProviderRuntime(ctx context.Context, instance provider.Instance) error {
	if m.Lifecycle == nil {
		if m.Provider == nil {
			return fmt.Errorf("provider lifecycle is required")
		}
		return m.validateRuntime(ctx, instance.Name)
	}
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return fmt.Errorf("provider lifecycle is required")
	}
	info, err := lifecycle.VerifyRuntime(ctx, instance)
	if err != nil {
		return err
	}
	if !info.Ready {
		return fmt.Errorf("provider runtime for %q is not ready", instance.Name)
	}
	return nil
}

// verifyProviderHostTrustRuntime preserves the common runtime check and adds
// an optional provider-specific read-only trust-transport check. Providers
// that only implement HostTrustRuntimeActivator therefore retain the common
// VerifyRuntime fallback, while providers with a transport that needs a
// stronger proof can implement HostTrustRuntimeVerifier.
func (m *Manager) verifyProviderHostTrustRuntime(ctx context.Context, instance provider.Instance) error {
	if err := m.verifyProviderRuntime(ctx, instance); err != nil {
		return err
	}
	verifier, ok := m.providerLifecycle().(provider.HostTrustRuntimeVerifier)
	if !ok {
		return nil
	}
	if err := verifier.VerifyHostTrustRuntime(ctx, instance); err != nil {
		return fmt.Errorf("verify provider host-trust runtime: %w", err)
	}
	return nil
}

func (m *Manager) verifyProviderAdmission(ctx context.Context, instance provider.Instance) error {
	if verifier, ok := m.Lifecycle.(provider.AdmissionVerifier); ok {
		if err := verifier.VerifyAdmission(ctx); err != nil {
			return fmt.Errorf("provider-wide admission failed: %w", err)
		}
	}
	if verifier, ok := m.Lifecycle.(provider.InstanceAdmissionVerifier); ok {
		if err := verifier.VerifyInstanceAdmission(ctx, instance); err != nil {
			return fmt.Errorf("instance admission failed: %w", err)
		}
	}
	return nil
}

func (m *Manager) activateProviderHostTrustRuntime(ctx context.Context, instance provider.Instance) error {
	activator, ok := m.providerLifecycle().(provider.HostTrustRuntimeActivator)
	if !ok {
		return nil
	}
	if err := activator.ActivateHostTrustRuntime(ctx, instance); err != nil {
		return fmt.Errorf("activate provider host-trust runtime: %w", err)
	}
	return nil
}

// prepareExistingHostTrustRuntimes fences reconciled runners, restores their
// current generation, and establishes controller-owned transport state before
// the pool is advertised as running. Docker Sandboxes relay credentials live
// only in the controller process, so a restart must rebind every kept runtime
// even when the host trust generation did not change.
func (m *Manager) prepareExistingHostTrustRuntimes(ctx context.Context, active map[string]ProvisionedInstance, register bool) (string, error) {
	if !m.hostTrustEnabled() {
		return "", nil
	}
	names := make([]string, 0, len(active))
	for name, instance := range active {
		if !instance.ProviderOwned || (instance.Phase != LifecycleReady && instance.Phase != LifecycleDraining) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := m.revokeHostTrustLease(ctx, name); err != nil {
			return "", fmt.Errorf("fence existing runner %q before host-trust preparation: %w", name, err)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	current, err := m.resolveHostTrust(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve host trust before preparing existing runtimes: %w", err)
	}
	for _, name := range names {
		instance, err := m.providerInstance(ctx, name)
		if err != nil {
			return "", fmt.Errorf("resolve existing provider instance %q for host-trust preparation: %w", name, err)
		}
		marker, err := m.readInstanceHostTrustMarker(ctx, name)
		if err != nil || validateHostTrustMarkerAgainstSnapshot(marker, current) != nil {
			if err := m.installHostTrustRuntime(ctx, name, current); err != nil {
				return "", fmt.Errorf("refresh host trust in existing runtime %q: %w", name, err)
			}
		}
		if err := m.activateProviderHostTrustRuntime(ctx, instance); err != nil {
			return "", fmt.Errorf("reactivate existing host-trust transport for %q: %w", name, err)
		}
		if err := m.verifyProviderRuntimeWithRetry(ctx, instance, active[name].GuestLogPath); err != nil {
			return "", fmt.Errorf("verify existing runtime %q after host-trust preparation: %w", name, err)
		}
		marker, err = m.readInstanceHostTrustMarker(ctx, name)
		if err != nil {
			return "", fmt.Errorf("read existing runtime %q host-trust marker: %w", name, err)
		}
		if err := validateHostTrustMarkerAgainstSnapshot(marker, current); err != nil {
			return "", fmt.Errorf("verify existing runtime %q host-trust marker: %w", name, err)
		}
		if err := m.verifyProviderAdmission(ctx, instance); err != nil {
			return "", fmt.Errorf("verify existing runtime %q admission before lease: %w", name, err)
		}
		prepared := active[name]
		prepared.HostTrustGeneration = marker.Generation
		active[name] = prepared
	}
	latest, err := m.resolveHostTrust(ctx)
	if err != nil {
		return "", fmt.Errorf("recheck host trust after preparing existing runtimes: %w", err)
	}
	if latest.Generation != current.Generation {
		return "", fmt.Errorf("host trust changed while existing runtimes were being prepared (%s -> %s)", current.Generation, latest.Generation)
	}
	if !register {
		return current.Generation, nil
	}
	if m.GitHub == nil {
		return "", fmt.Errorf("github client is required to lease existing registered runtimes")
	}
	for _, name := range names {
		runner, found, err := m.GitHub.RunnerByName(ctx, name)
		if err != nil {
			return "", fmt.Errorf("verify existing GitHub runner %q before host-trust lease: %w", name, err)
		}
		if !found {
			return "", fmt.Errorf("existing GitHub runner %q disappeared before host-trust lease", name)
		}
		lifetime := hostTrustLeaseLifetime
		if runner.Busy {
			lifetime = hostTrustHandoffLease
		}
		if err := m.issueHostTrustLeaseWithLifetime(ctx, name, current, lifetime); err != nil {
			return "", fmt.Errorf("lease existing runtime %q after host-trust preparation: %w", name, err)
		}
	}
	return current.Generation, nil
}

func (m *Manager) applyProviderNetworkPolicy(ctx context.Context, instance provider.Instance) error {
	if m.PolicyManager == nil {
		return nil
	}
	var rules []provider.NetworkPolicyRule
	if m.Config.DockerSandboxes.NetworkBaseline == config.DockerSandboxesNetworkBaselineOpen {
		rules = append(rules,
			provider.NetworkPolicyRule{Name: "epar-public-egress", Decision: provider.NetworkPolicyAllow, Resources: []string{"**"}},
			provider.NetworkPolicyRule{Name: "epar-host-alias-guardrails", Decision: provider.NetworkPolicyDeny, Resources: config.DockerSandboxesOpenDefaultDenyResources()},
		)
	}
	if len(m.Config.DockerSandboxes.AdditionalAllow) != 0 {
		rules = append(rules, provider.NetworkPolicyRule{Name: "epar-additional-allow", Decision: provider.NetworkPolicyAllow, Resources: append([]string(nil), m.Config.DockerSandboxes.AdditionalAllow...)})
	}
	if len(m.Config.DockerSandboxes.AdditionalDeny) != 0 {
		rules = append(rules, provider.NetworkPolicyRule{Name: "epar-additional-deny", Decision: provider.NetworkPolicyDeny, Resources: append([]string(nil), m.Config.DockerSandboxes.AdditionalDeny...)})
	}
	if err := m.PolicyManager.ApplyNetworkPolicy(ctx, instance, rules); err != nil {
		return err
	}
	_, err := m.PolicyManager.ReadNetworkPolicy(ctx, instance)
	return err
}

func (m *Manager) inventoryProvider(ctx context.Context) ([]provider.InventoryItem, error) {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return nil, fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Inventory(ctx)
}

func (m *Manager) providerInstance(ctx context.Context, name string) (provider.Instance, error) {
	if m.LifecycleState != nil {
		record, err := m.LifecycleState.Read(ctx, name)
		if err == nil {
			if record.ProviderType != m.Config.Provider.Type || record.ProviderID == "" {
				return provider.Instance{}, fmt.Errorf("lifecycle record for %q has no exact provider identity", name)
			}
			return provider.Instance{
				Name:           name,
				ProviderID:     record.ProviderID,
				ReceiptVersion: record.Receipt.Version,
				Receipt:        append([]byte(nil), record.Receipt.Payload...),
			}, nil
		}
		if !errors.Is(err, poolstate.ErrNotFound) {
			return provider.Instance{}, err
		}
	}
	items, err := m.inventoryProvider(ctx)
	if err != nil {
		return provider.Instance{}, err
	}
	for _, item := range items {
		if item.Instance.Name == name {
			return item.Instance, nil
		}
	}
	return provider.Instance{}, fmt.Errorf("provider instance %q is missing", name)
}

func (m *Manager) stopProviderInstance(ctx context.Context, instance provider.Instance) error {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Stop(ctx, instance)
}

func (m *Manager) deleteProviderInstance(ctx context.Context, instance provider.Instance) error {
	lifecycle := m.providerLifecycle()
	if lifecycle == nil {
		return fmt.Errorf("provider lifecycle is required")
	}
	return lifecycle.Delete(ctx, instance)
}

func (m *Manager) providerLifecycle() provider.Lifecycle {
	if m.Lifecycle != nil {
		return m.Lifecycle
	}
	if m.Provider == nil {
		return nil
	}
	return provider.AdaptLegacy(m.Provider, m.DryRun)
}
