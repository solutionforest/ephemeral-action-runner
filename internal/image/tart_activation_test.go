package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

type fakeTartActivationProvider struct {
	instances  map[string]provider.Instance
	nextID     int
	failSource string
	failTarget string
}

func (p *fakeTartActivationProvider) Clone(_ context.Context, source, name string) error {
	if source == p.failSource && name == p.failTarget {
		return errors.New("injected clone failure")
	}
	if _, exists := p.instances[source]; !exists {
		return fmt.Errorf("source %q does not exist", source)
	}
	p.nextID++
	p.instances[name] = provider.Instance{Name: name, Source: source, ProviderID: fmt.Sprintf("tart-mac:%012d", p.nextID), State: "stopped"}
	return nil
}

func (p *fakeTartActivationProvider) Start(context.Context, string, provider.StartOptions) (*provider.RunningProcess, error) {
	return nil, errors.New("unexpected start")
}

func (p *fakeTartActivationProvider) Exec(context.Context, string, []string, provider.ExecOptions) (provider.ExecResult, error) {
	return provider.ExecResult{}, errors.New("unexpected exec")
}

func (p *fakeTartActivationProvider) IP(context.Context, string, int) (string, error) {
	return "", errors.New("unexpected IP")
}

func (p *fakeTartActivationProvider) Stop(_ context.Context, name string) error {
	instance, exists := p.instances[name]
	if !exists {
		return fmt.Errorf("instance %q does not exist", name)
	}
	instance.State = "stopped"
	p.instances[name] = instance
	return nil
}

func (p *fakeTartActivationProvider) Delete(_ context.Context, name string) error {
	if _, exists := p.instances[name]; !exists {
		return fmt.Errorf("instance %q does not exist", name)
	}
	delete(p.instances, name)
	return nil
}

func (p *fakeTartActivationProvider) List(context.Context) ([]provider.Instance, error) {
	result := make([]provider.Instance, 0, len(p.instances))
	for _, instance := range p.instances {
		result = append(result, instance)
	}
	return result, nil
}

func TestTartActivationRetainsRollbackUntilReplacementReadback(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("TART_HOME", filepath.Join(t.TempDir(), "tart"))
	previous := provider.Instance{Name: "runner-image", ProviderID: "tart-mac:000000000001", State: "stopped"}
	fake := &fakeTartActivationProvider{instances: map[string]provider.Instance{
		previous.Name:       previous,
		"runner-build-hash": {Name: "runner-build-hash", ProviderID: "tart-mac:000000000002", State: "stopped"},
	}, nextID: 2}
	coordinator := Coordinator{Config: config.Default(), Provider: fake, ProjectRoot: t.TempDir()}
	if err := coordinator.activateTartImage(context.Background(), previous, true, "runner-build-hash", previous.Name); err != nil {
		t.Fatal(err)
	}
	if current, exists := fake.instances[previous.Name]; !exists || current.ProviderID == previous.ProviderID {
		t.Fatalf("replacement was not activated: %#v", fake.instances)
	}
	if _, exists := fake.instances["runner-build-hash"]; exists {
		t.Fatal("verified Tart build candidate was not retired after activation")
	}
	if _, exists := fake.instances[tartBackupName(previous.Name, previous.ProviderID)]; exists {
		t.Fatal("Tart rollback image was retired only after readback but still remains")
	}
}

func TestTartActivationFailureRestoresPreviousGeneration(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("TART_HOME", filepath.Join(t.TempDir(), "tart"))
	previous := provider.Instance{Name: "runner-image", ProviderID: "tart-mac:000000000001", State: "stopped"}
	fake := &fakeTartActivationProvider{instances: map[string]provider.Instance{
		previous.Name:       previous,
		"runner-build-hash": {Name: "runner-build-hash", ProviderID: "tart-mac:000000000002", State: "stopped"},
	}, nextID: 2, failSource: "runner-build-hash", failTarget: previous.Name}
	coordinator := Coordinator{Config: config.Default(), Provider: fake, ProjectRoot: t.TempDir()}
	err := coordinator.activateTartImage(context.Background(), previous, true, "runner-build-hash", previous.Name)
	if err == nil || !strings.Contains(err.Error(), "previous image was restored") {
		t.Fatalf("activation error = %v", err)
	}
	if current, exists := fake.instances[previous.Name]; !exists || current.ProviderID == "" {
		t.Fatalf("previous generation was not restored: %#v", fake.instances)
	}
}

func TestTartStartupReconciliationRestoresCatalogedRollbackBeforeCleanup(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("TART_HOME", filepath.Join(t.TempDir(), "tart"))
	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, "config.yml")
	if err := os.WriteFile(configPath, []byte("provider:\n  type: tart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupName := "runner-image-epar-previous-000000000001"
	fake := &fakeTartActivationProvider{instances: map[string]provider.Instance{
		backupName: {Name: backupName, ProviderID: "tart-mac:000000000002", State: "stopped"},
	}, nextID: 2}
	cfg := config.Default()
	cfg.Provider.Type = "tart"
	cfg.Image.OutputImage = "runner-image"
	coordinator := Coordinator{Config: cfg, Provider: fake, ProjectRoot: projectRoot, ConfigPath: configPath}
	if err := coordinator.recordTartStagingImage(context.Background(), backupName, "activation-rollback"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.reconcileInterruptedTartArtifacts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if current, exists := fake.instances[cfg.Image.OutputImage]; !exists || current.ProviderID == "" {
		t.Fatalf("startup did not restore the configured Tart output: %#v", fake.instances)
	}
	store, err := storagecatalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range value.Resources {
		if resource.Locator == backupName && (resource.State != storagecatalog.StateSuperseded || len(resource.References) != 0) {
			t.Fatalf("restored rollback staging reference was not released: %#v", resource)
		}
	}
}
