package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	imageartifact "github.com/solutionforest/ephemeral-action-runner/internal/image"
	sandboxpromotion "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/promotion"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/tart"
)

func TestTartWizardWarnsBeforeCreatingNativeConfiguration(t *testing.T) {
	stubInitHostAndRandom(t, "Mobile Mac", []byte{0x12, 0x34, 0x56})
	stubNoWSL2(t)
	oldPrerequisites, oldSource := initTartPrerequisites, initResolveTartSource
	t.Cleanup(func() { initTartPrerequisites = oldPrerequisites; initResolveTartSource = oldSource })
	initTartPrerequisites = func(context.Context, sandboxpromotion.Platform) (bool, string) { return true, "READY — test Tart" }
	initResolveTartSource = func(_ context.Context, ref, platform string) (imageartifact.TartSource, error) {
		return imageartifact.TartSource{Reference: ref, Platform: platform, CompressedBytes: 1024, ExpandedBytes: 4096}, nil
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".local", "config.yml")
	var out bytes.Buffer
	if err := runInitWithOptions(initOptions{ProjectRoot: dir, ConfigPath: path, SkipDockerCheck: true, SkipHostTrustCheck: true, In: strings.NewReader("123456\nsolutionforest\n.local/github-app.pem\n1\nc\n4\n\n\n\n"), Out: &out}); err != nil {
		t.Fatalf("init failed: %v\n%s", err, out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.TartMacOS() || !cfg.Runner.NoDefaultLabels || cfg.Image.HostTrustMode != config.HostTrustModeDisabled || cfg.Pool.NamePrefix != "mobile-mac-123456" {
		t.Fatalf("unexpected Tart configuration: %+v", cfg)
	}
	if len(cfg.Image.CustomInstallScripts) != 0 || cfg.Image.UpdateFrequency != config.ImageUpdateFrequencyWeekly || cfg.Image.UpdateTime != "07:00" {
		t.Fatalf("shared artifact defaults were lost: %+v", cfg.Image)
	}
	selection := strings.Index(out.String(), "4. Tart macOS")
	review := strings.Index(out.String(), "Configuration review:")
	if selection < 0 || review <= selection {
		t.Fatalf("missing provider/review flow:\n%s", out.String())
	}
	if !strings.Contains(out.String()[selection:review], tart.MacOSDockerWarning) || !strings.Contains(out.String()[review:], tart.MacOSDockerWarning) {
		t.Fatalf("Docker limitation must be shown at selection and review:\n%s", out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestTartImageSetupBackAndUnavailableSelection(t *testing.T) {
	result, _, err := promptTartImageWizard(context.Background(), t.TempDir(), nil, &bytes.Buffer{}, bufio.NewReader(strings.NewReader("/back\n")))
	if err != nil || result.Action != initWizardBack {
		t.Fatalf("Back action=%+v err=%v", result, err)
	}
	_, err = promptProviderOptionsWizard(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("4\n")), initProviderPrerequisites{TartStatus: "UNAVAILABLE — no Tart"}, false, false, true, &initProviderMenuState{CompatibilityRevealed: true})
	if err == nil {
		t.Fatal("unavailable Tart provider was selected")
	}
}
