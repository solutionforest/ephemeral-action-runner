package image

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

const (
	liveBuildxContextAEnvironment = "EPAR_TEST_BUILDX_CONTEXT_A"
	liveBuildxContextBEnvironment = "EPAR_TEST_BUILDX_CONTEXT_B"
)

func TestLiveBuildxRecoveryAcrossDisposableDockerContexts(t *testing.T) {
	contextA := strings.TrimSpace(os.Getenv(liveBuildxContextAEnvironment))
	contextB := strings.TrimSpace(os.Getenv(liveBuildxContextBEnvironment))
	if contextA == "" || contextB == "" {
		t.Skipf("live Buildx recovery requires two caller-supplied disposable Docker contexts; set both %s and %s", liveBuildxContextAEnvironment, liveBuildxContextBEnvironment)
	}
	if contextA == contextB {
		t.Fatalf("%s and %s must name different disposable Docker contexts", liveBuildxContextAEnvironment, liveBuildxContextBEnvironment)
	}

	testContext, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	backendA, err := liveDockerBackendID(testContext, contextA)
	if err != nil {
		t.Fatalf("inspect disposable Docker context %q: %v", contextA, err)
	}
	backendB, err := liveDockerBackendID(testContext, contextB)
	if err != nil {
		t.Fatalf("inspect disposable Docker context %q: %v", contextB, err)
	}
	if backendA == backendB {
		t.Fatalf("disposable Docker contexts %q and %q report the same daemon identity %q; two different daemons are required", contextA, contextB, backendA)
	}

	projectRoot := t.TempDir()
	configPath := filepath.Join(projectRoot, ".local", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  type: docker-container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EPAR_STATE_HOME", filepath.Join(projectRoot, "host-state"))
	scope, err := resolveBuildxScope(projectRoot, configPath)
	if err != nil {
		t.Fatal(err)
	}
	builder := "epar-" + scope.configID
	imageTag := "epar-buildx-live-cache:" + scope.configID
	defer cleanupLiveBuildxResources(t, []string{contextA, contextB}, builder, imageTag)

	trust, err := liveBuildxTrustSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	environment := &liveBuildxEnvironment{t: t, dockerContext: contextA, trust: trust}
	coordinator := Coordinator{Config: config.Default(), ProjectRoot: projectRoot, ConfigPath: configPath, environment: environment}
	registryReferences := []string{"docker.io/library/epar-buildx-live-trust-probe:latest"}

	builderA, err := coordinator.ensureBuildxBuilder(testContext, registryReferences)
	if err != nil {
		t.Fatalf("reconcile Buildx builder on disposable context %q: %v", contextA, err)
	}
	if builderA != builder {
		t.Fatalf("context A builder = %q, want exact config-scoped builder %q", builderA, builder)
	}
	metadataA := loadLiveBuildxMetadata(t, projectRoot, configPath)
	assertLiveBuildxBackend(t, metadataA, backendA)
	assertLiveBuildxTrustMetadata(t, metadataA, trust)
	if metadataA.CreatedAt.IsZero() {
		t.Fatal("schema-5 metadata omitted CreatedAt after context A reconciliation")
	}
	createdAt := metadataA.CreatedAt

	buildContext := filepath.Join(projectRoot, "cache-probe")
	if err := os.MkdirAll(buildContext, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildContext, "Dockerfile"), []byte("FROM scratch\nCOPY payload.txt /payload.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildContext, "payload.txt"), []byte("deterministic EPAR Buildx live cache probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runLiveBuildxCacheProbe(testContext, contextA, builder, imageTag, buildContext); err != nil {
		t.Fatalf("prime daemon-A BuildKit cache: %v", err)
	}

	environment.setDockerContext(contextB)
	builderB, err := coordinator.ensureBuildxBuilder(testContext, registryReferences)
	if err != nil {
		t.Fatalf("recover Buildx builder on disposable context %q: %v", contextB, err)
	}
	if builderB != builder {
		t.Fatalf("context B builder = %q, want exact config-scoped builder %q", builderB, builder)
	}
	metadataB := loadLiveBuildxMetadata(t, projectRoot, configPath)
	assertLiveBuildxBackend(t, metadataB, backendB)
	assertLiveBuildxTrustMetadata(t, metadataB, trust)
	if !metadataB.CreatedAt.Equal(createdAt) {
		t.Fatalf("context B reconciliation changed CreatedAt: got %s, want %s", metadataB.CreatedAt, createdAt)
	}

	environment.setDockerContext(contextA)
	builderAAgain, err := coordinator.ensureBuildxBuilder(testContext, registryReferences)
	if err != nil {
		t.Fatalf("recover Buildx builder after returning to disposable context %q: %v", contextA, err)
	}
	if builderAAgain != builder {
		t.Fatalf("return-to-A builder = %q, want exact config-scoped builder %q", builderAAgain, builder)
	}
	metadataAAgain := loadLiveBuildxMetadata(t, projectRoot, configPath)
	assertLiveBuildxBackend(t, metadataAAgain, backendA)
	assertLiveBuildxTrustMetadata(t, metadataAAgain, trust)
	if !metadataAAgain.CreatedAt.Equal(createdAt) {
		t.Fatalf("return-to-A reconciliation changed CreatedAt: got %s, want %s", metadataAAgain.CreatedAt, createdAt)
	}

	cacheOutput, err := runLiveBuildxCacheProbe(testContext, contextA, builder, imageTag, buildContext)
	if err != nil {
		t.Fatalf("repeat daemon-A BuildKit cache probe after A -> B -> A recovery: %v", err)
	}
	if !liveBuildxCopyStepWasCached(cacheOutput) {
		t.Fatalf("daemon-A cache was not reused after A -> B -> A recovery; expected the deterministic COPY step to be CACHED:\n%s", cacheOutput)
	}
}

type liveBuildxEnvironment struct {
	Environment
	t *testing.T

	mu            sync.RWMutex
	dockerContext string
	trust         hosttrust.Snapshot
}

func (environment *liveBuildxEnvironment) setDockerContext(value string) {
	environment.mu.Lock()
	defer environment.mu.Unlock()
	environment.dockerContext = value
}

func (environment *liveBuildxEnvironment) currentDockerContext() string {
	environment.mu.RLock()
	defer environment.mu.RUnlock()
	return environment.dockerContext
}

func (environment *liveBuildxEnvironment) Infof(format string, args ...any) {
	environment.t.Logf(strings.TrimSuffix(format, "\n"), args...)
}

func (environment *liveBuildxEnvironment) Warnf(format string, args ...any) {
	environment.t.Logf("warning: "+strings.TrimSuffix(format, "\n"), args...)
}

func (environment *liveBuildxEnvironment) RunHost(ctx context.Context, name string, args ...string) error {
	if name != "docker" {
		return fmt.Errorf("live Buildx test refuses implicit host command %q", name)
	}
	_, err := runLiveDockerCommand(ctx, environment.currentDockerContext(), args...)
	return err
}

func (environment *liveBuildxEnvironment) RunHostOutput(ctx context.Context, name string, args ...string) (string, error) {
	if name != "docker" {
		return "", fmt.Errorf("live Buildx test refuses implicit host command %q", name)
	}
	return runLiveDockerCommand(ctx, environment.currentDockerContext(), args...)
}

func (environment *liveBuildxEnvironment) ResolveBuildTrust(context.Context) (hosttrust.Snapshot, error) {
	return environment.trust, nil
}

func runLiveDockerCommand(ctx context.Context, dockerContext string, args ...string) (string, error) {
	explicitArgs := append([]string{"--context", dockerContext}, args...)
	command := exec.CommandContext(ctx, "docker", explicitArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker --context %q %s: %w: %s", dockerContext, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func liveDockerBackendID(ctx context.Context, dockerContext string) (string, error) {
	output, err := runLiveDockerCommand(ctx, dockerContext, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(output)
	if identity == "" {
		return "", fmt.Errorf("Docker context %q returned an empty daemon identity", dockerContext)
	}
	return "docker:" + identity, nil
}

func liveBuildxTrustSnapshot() (hosttrust.Snapshot, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "EPAR Buildx live test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certificates, err := hosttrust.CertificatesFromBytes(certificatePEM)
	if err != nil {
		return hosttrust.Snapshot{}, err
	}
	return hosttrust.Canonicalize(hosttrust.Snapshot{HostOS: "linux", Scopes: []string{hosttrust.ScopeSystem}, Certificates: certificates, CollectedAt: now})
}

func loadLiveBuildxMetadata(t *testing.T, projectRoot, configPath string) BuildxMetadata {
	t.Helper()
	metadata, err := LoadBuildxMetadataForConfig(projectRoot, configPath)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func assertLiveBuildxBackend(t *testing.T, metadata BuildxMetadata, backendID string) {
	t.Helper()
	if metadata.SchemaVersion != buildxMetadataSchemaVersion {
		t.Fatalf("Buildx metadata schema = %d, want %d", metadata.SchemaVersion, buildxMetadataSchemaVersion)
	}
	if metadata.BackendID != backendID {
		t.Fatalf("Buildx metadata backend = %q, want %q", metadata.BackendID, backendID)
	}
}

func assertLiveBuildxTrustMetadata(t *testing.T, metadata BuildxMetadata, trust hosttrust.Snapshot) {
	t.Helper()
	if metadata.TrustGeneration != trust.Generation {
		t.Fatalf("Buildx trust generation = %q, want %q", metadata.TrustGeneration, trust.Generation)
	}
	if metadata.ConfigSHA256 == "" || metadata.CertificateSHA256 == "" || metadata.BuildKitImageID == "" {
		t.Fatalf("Buildx metadata omitted verified configuration, certificate, or BuildKit image evidence: %#v", metadata)
	}
	if len(metadata.RegistryHosts) != 1 || metadata.RegistryHosts[0] != "docker.io" {
		t.Fatalf("Buildx metadata registry trust hosts = %v, want [docker.io]", metadata.RegistryHosts)
	}
}

func runLiveBuildxCacheProbe(ctx context.Context, dockerContext, builder, imageTag, buildContext string) (string, error) {
	return runLiveDockerCommand(ctx, dockerContext, "buildx", "build", "--builder", builder, "--progress", "plain", "--load", "--tag", imageTag, buildContext)
}

func liveBuildxCopyStepWasCached(output string) bool {
	stepID := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.Contains(line, "COPY payload.txt /payload.txt") {
			stepID = fields[0]
			continue
		}
		if stepID != "" && strings.HasPrefix(strings.TrimSpace(line), stepID+" ") && strings.Contains(strings.ToUpper(line), "CACHED") {
			return true
		}
	}
	return false
}

func cleanupLiveBuildxResources(t *testing.T, dockerContexts []string, builder, imageTag string) {
	t.Helper()
	cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	controlContainer := buildxControlContainer(builder)
	stateVolume := controlContainer + "_state"
	for _, dockerContext := range dockerContexts {
		commands := [][]string{
			{"buildx", "rm", "--force", builder},
			{"container", "rm", "--force", controlContainer},
			{"volume", "rm", "--force", stateVolume},
			{"image", "rm", "--force", imageTag},
		}
		for _, command := range commands {
			if _, err := runLiveDockerCommand(cleanupContext, dockerContext, command...); err != nil && !liveBuildxCleanupTargetWasAbsent(err) {
				t.Logf("exact live Buildx cleanup warning for Docker context %q (%s): %v", dockerContext, strings.Join(command, " "), err)
			}
		}
	}
}

func liveBuildxCleanupTargetWasAbsent(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such") || strings.Contains(message, "not found") || (strings.Contains(message, "no builder") && strings.Contains(message, "found"))
}
