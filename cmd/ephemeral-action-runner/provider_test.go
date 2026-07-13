package main

import (
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	dockerdindprovider "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockerdind"
)

func TestNewProviderWiresDockerDaemonProxy(t *testing.T) {
	cfg := config.Default()
	cfg.Provider.Type = "docker-dind"
	cfg.Provider.Platform = "linux/amd64"
	cfg.Docker.HTTPProxy = "http://host.docker.internal:3128"
	cfg.Docker.HTTPSProxy = "http://host.docker.internal:3128"
	cfg.Docker.NoProxy = "localhost,127.0.0.1"

	created, err := newProvider(cfg, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	dind, ok := created.(*dockerdindprovider.Provider)
	if !ok {
		t.Fatalf("newProvider() type = %T, want Docker-DinD provider", created)
	}
	if !dind.HostGateway {
		t.Fatal("host.docker.internal proxy did not enable host gateway")
	}
	for key, want := range map[string]string{
		"HTTP_PROXY":  cfg.Docker.HTTPProxy,
		"HTTPS_PROXY": cfg.Docker.HTTPSProxy,
		"NO_PROXY":    cfg.Docker.NoProxy,
	} {
		if got := dind.Environment[key]; got != want {
			t.Errorf("provider environment %s = %q, want %q", key, got, want)
		}
	}
}
