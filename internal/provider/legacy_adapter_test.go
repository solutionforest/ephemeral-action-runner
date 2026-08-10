package provider

import (
	"context"
	"testing"
)

type legacyProviderFake struct {
	calls []string
	ip    string
}

func (fake *legacyProviderFake) Clone(_ context.Context, source, name string) error {
	fake.calls = append(fake.calls, "clone:"+source+":"+name)
	return nil
}

func (fake *legacyProviderFake) Start(_ context.Context, name string, _ StartOptions) (*RunningProcess, error) {
	fake.calls = append(fake.calls, "start:"+name)
	return &RunningProcess{Name: name}, nil
}

func (fake *legacyProviderFake) Exec(_ context.Context, name string, command []string, _ ExecOptions) (ExecResult, error) {
	fake.calls = append(fake.calls, "exec:"+name)
	return ExecResult{Stdout: command[0]}, nil
}

func (fake *legacyProviderFake) IP(_ context.Context, name string, waitSeconds int) (string, error) {
	fake.calls = append(fake.calls, "ip:"+name)
	if waitSeconds != 30 {
		panic("unexpected address wait")
	}
	return fake.ip, nil
}

func (fake *legacyProviderFake) Stop(_ context.Context, name string) error {
	fake.calls = append(fake.calls, "stop:"+name)
	return nil
}

func (fake *legacyProviderFake) Delete(_ context.Context, name string) error {
	fake.calls = append(fake.calls, "delete:"+name)
	return nil
}

func (fake *legacyProviderFake) List(context.Context) ([]Instance, error) {
	fake.calls = append(fake.calls, "list")
	return []Instance{{Name: "runner", ProviderID: "fake:runner-id", Source: "image", State: "running"}}, nil
}

func TestAdaptLegacyMapsLifecycleWithoutAdditionalRuntimeProbe(t *testing.T) {
	legacy := &legacyProviderFake{ip: "192.0.2.10"}
	lifecycle := AdaptLegacy(legacy)
	instance, err := lifecycle.Create(context.Background(), CreateRequest{Name: "runner", Source: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Start(context.Background(), instance, StartOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime, err := lifecycle.VerifyRuntime(context.Background(), instance)
	if err != nil || !runtime.Ready {
		t.Fatalf("runtime = %#v, err = %v", runtime, err)
	}
	address, available, err := lifecycle.Address(context.Background(), instance, 30)
	if err != nil || !available || address != "192.0.2.10" {
		t.Fatalf("address = %q, available = %v, err = %v", address, available, err)
	}
	result, err := lifecycle.Exec(context.Background(), instance, []string{"true"}, ExecOptions{})
	if err != nil || result.Stdout != "true" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if err := lifecycle.Stop(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Delete(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	items, err := lifecycle.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance.Name != "runner" || items[0].State != "running" {
		t.Fatalf("inventory = %#v", items)
	}
	for _, want := range []string{"clone:image:runner", "start:runner", "ip:runner", "exec:runner", "stop:runner", "delete:runner"} {
		if !containsCall(legacy.calls, want) {
			t.Fatalf("calls = %#v, missing %q", legacy.calls, want)
		}
	}
}

func TestAdaptLegacyAddressCanBeUnavailable(t *testing.T) {
	legacy := &legacyProviderFake{}
	lifecycle := AdaptLegacy(legacy)
	address, available, err := lifecycle.Address(context.Background(), Instance{Name: "runner", ProviderID: "fake:runner-id"}, 30)
	if err != nil || available || address != "" {
		t.Fatalf("address = %q, available = %v, err = %v", address, available, err)
	}
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}
