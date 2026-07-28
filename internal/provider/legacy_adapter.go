package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type legacyAdapter struct {
	provider Provider
	dryRun   bool
}

// AdaptLegacy preserves existing provider behavior behind the new lifecycle
// surface. Legacy runtime readiness is still performed by each provider's
// Start implementation, so VerifyRuntime intentionally has no additional side
// effect. The old one-second address wait is used only when a caller explicitly
// requests Address.
func AdaptLegacy(provider Provider, dryRun ...bool) Lifecycle {
	adapter := &legacyAdapter{provider: provider}
	if len(dryRun) != 0 {
		adapter.dryRun = dryRun[0]
	}
	return adapter
}

func (adapter *legacyAdapter) Create(ctx context.Context, request CreateRequest) (Instance, error) {
	if adapter == nil || adapter.provider == nil {
		return Instance{}, fmt.Errorf("legacy provider is nil")
	}
	if request.Name == "" {
		return Instance{}, fmt.Errorf("instance name is required")
	}
	if err := adapter.provider.Clone(ctx, request.Source, request.Name); err != nil {
		return Instance{}, err
	}
	readbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	instances, err := adapter.provider.List(readbackContext)
	if err != nil {
		return Instance{}, fmt.Errorf("read exact provider identity after create: %w", err)
	}
	for _, instance := range instances {
		if instance.Name != request.Name {
			continue
		}
		if instance.ProviderID == "" {
			return Instance{}, fmt.Errorf("provider inventory omitted immutable identity for %q", request.Name)
		}
		instance.ReceiptVersion = "v1"
		instance.Receipt, _ = json.Marshal(map[string]string{"providerId": instance.ProviderID, "source": request.Source})
		return instance, nil
	}
	if adapter.dryRun {
		receipt, _ := json.Marshal(map[string]string{"providerId": "dry-run:" + request.Name, "source": request.Source})
		return Instance{Name: request.Name, ProviderID: "dry-run:" + request.Name, Source: request.Source, ReceiptVersion: "v1", Receipt: receipt}, nil
	}
	return Instance{}, fmt.Errorf("provider inventory omitted newly created instance %q", request.Name)
}

func (adapter *legacyAdapter) Start(ctx context.Context, instance Instance, opts StartOptions) (*RunningProcess, error) {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return nil, err
	}
	return adapter.provider.Start(ctx, instance.Name, opts)
}

func (adapter *legacyAdapter) VerifyRuntime(ctx context.Context, instance Instance) (RuntimeInfo, error) {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return RuntimeInfo{}, err
	}
	if _, err := adapter.provider.Exec(ctx, instance.Name, []string{"sudo", "bash", "/opt/epar/validate-runtime.sh"}, ExecOptions{}); err != nil {
		return RuntimeInfo{}, err
	}
	return RuntimeInfo{Ready: true}, nil
}

func (adapter *legacyAdapter) Address(ctx context.Context, instance Instance, waitSeconds int) (string, bool, error) {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return "", false, err
	}
	address, err := adapter.provider.IP(ctx, instance.Name, waitSeconds)
	if err != nil {
		return "", false, err
	}
	if address == "" {
		return "", false, nil
	}
	return address, true, nil
}

func (adapter *legacyAdapter) Exec(ctx context.Context, instance Instance, command []string, opts ExecOptions) (ExecResult, error) {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return ExecResult{}, err
	}
	return adapter.provider.Exec(ctx, instance.Name, command, opts)
}

func (adapter *legacyAdapter) Diagnostics(ctx context.Context, instance Instance) (Diagnostics, error) {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return Diagnostics{}, err
	}
	return Diagnostics{}, nil
}

func (adapter *legacyAdapter) Stop(ctx context.Context, instance Instance) error {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return err
	}
	return adapter.provider.Stop(ctx, instance.Name)
}

func (adapter *legacyAdapter) Delete(ctx context.Context, instance Instance) error {
	if err := adapter.assertExactIdentity(ctx, instance); err != nil {
		return err
	}
	return adapter.provider.Delete(ctx, instance.Name)
}

func (adapter *legacyAdapter) Inventory(ctx context.Context) ([]InventoryItem, error) {
	identityContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	instances, err := adapter.provider.List(identityContext)
	if err != nil {
		return nil, err
	}
	items := make([]InventoryItem, 0, len(instances))
	for _, instance := range instances {
		items = append(items, InventoryItem{Instance: instance, State: instance.State, Source: instance.Source})
	}
	return items, nil
}

func (adapter *legacyAdapter) assertExactIdentity(ctx context.Context, expected Instance) error {
	if adapter == nil || adapter.provider == nil {
		return fmt.Errorf("legacy provider is nil")
	}
	if adapter.dryRun && expected.ProviderID == "dry-run:"+expected.Name {
		return nil
	}
	if expected.Name == "" || expected.ProviderID == "" {
		return fmt.Errorf("exact provider name and immutable id are required")
	}
	identityContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	instances, err := adapter.provider.List(identityContext)
	if err != nil {
		return err
	}
	for _, actual := range instances {
		if actual.Name != expected.Name {
			continue
		}
		if actual.ProviderID == "" || actual.ProviderID != expected.ProviderID {
			return fmt.Errorf("provider identity mismatch for %q: expected %q, observed %q", expected.Name, expected.ProviderID, actual.ProviderID)
		}
		return nil
	}
	return fmt.Errorf("provider instance %q is missing", expected.Name)
}
