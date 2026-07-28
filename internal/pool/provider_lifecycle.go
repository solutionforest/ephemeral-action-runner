package pool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

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
		Name:           name,
		Source:         m.Config.Provider.SourceImage,
		Template:       m.Config.DockerSandboxes.Template,
		TemplateDigest: m.Config.DockerSandboxes.TemplateDigest,
		StagingPath:    filepath.Join(config.ProjectPath(m.ProjectRoot, m.Config.DockerSandboxes.StagingRoot), name),
		CPUs:           m.Config.DockerSandboxes.CPUs,
		Memory:         m.Config.DockerSandboxes.Memory,
		RootDisk:       m.Config.DockerSandboxes.RootDisk,
		DockerDisk:     m.Config.DockerSandboxes.DockerDisk,
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
