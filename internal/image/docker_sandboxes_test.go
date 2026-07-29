package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

func TestNormalizeCatthehackerSourceProfilesAndCustomTag(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"", "ghcr.io/catthehacker/ubuntu:full-latest"},
		{"full", "ghcr.io/catthehacker/ubuntu:full-latest"},
		{"act", "ghcr.io/catthehacker/ubuntu:act-latest"},
		{"dotnet", "ghcr.io/catthehacker/ubuntu:dotnet-latest"},
		{"js", "ghcr.io/catthehacker/ubuntu:js-latest"},
		{"go-24.04", "ghcr.io/catthehacker/ubuntu:go-24.04"},
		{"ghcr.io/catthehacker/ubuntu:go-24.04", "ghcr.io/catthehacker/ubuntu:go-24.04"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeCatthehackerSource(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("NormalizeCatthehackerSource(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestDockerSandboxesDockerfileUsesVerifiedLocalDownloadsAndInstallsTrustBeforeCustomization(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "templates", "docker-sandboxes", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "ADD --") || strings.Contains(text, "ACTIONS_RUNNER_URL") || strings.Contains(text, "TINI_URL") {
		t.Fatalf("Docker Sandboxes Dockerfile still delegates remote HTTPS downloads to BuildKit:\n%s", text)
	}
	for _, want := range []string{
		"COPY --chmod=0755 inputs/tini /usr/local/bin/tini",
		"COPY inputs/actions-runner.tar.gz /tmp/actions-runner.tar.gz",
		`echo "${TINI_SHA256#sha256:}  /usr/local/bin/tini" | sha256sum --check -`,
		`echo "${ACTIONS_RUNNER_SHA256#sha256:}  /tmp/actions-runner.tar.gz" | sha256sum --check -`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Docker Sandboxes Dockerfile omitted %q", want)
		}
	}
	trustInstall := strings.Index(text, "/opt/epar/install-trusted-ca-certificates.sh")
	customInstall := strings.Index(text, "/opt/epar/custom-install/run.sh")
	if trustInstall < 0 || customInstall < 0 || trustInstall >= customInstall {
		t.Fatalf("runner trust must be installed before custom scripts:\n%s", text)
	}
}

func TestDockerSandboxesDisabledTrustPolicyIsExplicit(t *testing.T) {
	root := t.TempDir()
	coordinator := &Coordinator{ProjectRoot: root}
	if err := coordinator.prepareDockerSandboxesTrustPolicy(root, hosttrust.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "host-trust-metadata", "host-trust-generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		SchemaVersion    int      `json:"schemaVersion"`
		Generation       string   `json:"generation"`
		HostOS           string   `json:"hostOS"`
		Mode             string   `json:"mode"`
		Scopes           []string `json:"scopes"`
		CertificateCount int      `json:"certificateCount"`
	}
	if err := json.Unmarshal(content, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.SchemaVersion != 1 || marker.Generation != "disabled" || marker.HostOS != "" || marker.Mode != hosttrust.ModeDisabled || len(marker.Scopes) != 0 || marker.CertificateCount != 0 {
		t.Fatalf("disabled policy marker = %+v", marker)
	}
}

func TestSelectBuildxSBOMAttachmentUsesExactAttestationChain(t *testing.T) {
	attestationDigest := "sha256:" + strings.Repeat("a", 64)
	sbomDigest := "sha256:" + strings.Repeat("b", 64)
	index := []byte(`{
		"manifests": [
			{"digest": "sha256:` + strings.Repeat("c", 64) + `", "annotations": {}},
			{"digest": "` + attestationDigest + `", "annotations": {"vnd.docker.reference.type": "attestation-manifest"}}
		]
	}`)
	gotAttestation, err := selectBuildxAttestationDigest(index)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttestation != attestationDigest {
		t.Fatalf("attestation digest = %q, want %q", gotAttestation, attestationDigest)
	}
	manifest := []byte(`{
		"layers": [
			{"mediaType": "application/vnd.in-toto+json", "digest": "` + sbomDigest + `", "size": 216579021, "annotations": {"in-toto.io/predicate-type": "https://spdx.dev/Document"}},
			{"mediaType": "application/vnd.in-toto+json", "digest": "sha256:` + strings.Repeat("d", 64) + `", "size": 123, "annotations": {"in-toto.io/predicate-type": "https://slsa.dev/provenance/v1"}}
		]
	}`)
	gotSBOM, gotSize, err := selectBuildxSBOMDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if gotSBOM != sbomDigest || gotSize != 216579021 {
		t.Fatalf("SBOM attachment = %q/%d, want %q/%d", gotSBOM, gotSize, sbomDigest, 216579021)
	}
}

func TestDockerContentBlobCandidatesAreExactAndContentAddressed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	candidates := dockerContentBlobCandidates(digest)
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, "/var/lib/") || !strings.HasSuffix(candidate, "/blobs/sha256/"+strings.Repeat("a", 64)) {
			t.Fatalf("unsafe Docker content candidate %q", candidate)
		}
	}
	if candidates[0] == candidates[1] {
		t.Fatal("Docker Desktop and native Engine content candidates must differ")
	}
}

func TestValidateInTotoSPDXStreamsAndRejectsMalformedPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sbom.intoto.json")
	valid := `{"_type":"https://in-toto.io/Statement/v1","subject":[],"predicateType":"https://spdx.dev/Document","predicate":{"SPDXID":"SPDXRef-DOCUMENT","packages":[{"name":"example"}]}}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInTotoSPDX(path); err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(valid, "https://spdx.dev/Document", "https://example.invalid/Unknown", 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInTotoSPDX(path); err == nil {
		t.Fatal("unknown predicate type was accepted")
	}
}

func TestNormalizeCatthehackerSourceRejectsOtherRepositoriesAndInvalidTags(t *testing.T) {
	for _, input := range []string{
		"ubuntu:latest",
		"ghcr.io/other/ubuntu:full-latest",
		"ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("a", 64),
		"tag with spaces",
		"-leading-dash",
	} {
		if _, err := NormalizeCatthehackerSource(input); err == nil {
			t.Fatalf("NormalizeCatthehackerSource(%q) succeeded", input)
		}
	}
}

func TestParseResolvedDockerSourceSelectsExactNativeManifestAndSize(t *testing.T) {
	index := dockerManifestDocument{MediaType: "application/vnd.oci.image.index.v1+json"}
	amd64 := dockerManifestDescriptor{Digest: "sha256:" + strings.Repeat("a", 64)}
	amd64.Platform.OS = "linux"
	amd64.Platform.Architecture = "amd64"
	arm64 := dockerManifestDescriptor{Digest: "sha256:" + strings.Repeat("b", 64)}
	arm64.Platform.OS = "linux"
	arm64.Platform.Architecture = "arm64"
	index.Manifests = []dockerManifestDescriptor{amd64, arm64}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw := []byte(`{"layers":[{"size":100},{"size":23}]}`)
	resolved, err := parseResolvedDockerSource("ghcr.io/catthehacker/ubuntu:full-latest", "linux/arm64", indexRaw, manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PlatformDigest != arm64.Digest || resolved.CompressedLayerBytes != 123 || !strings.HasPrefix(resolved.ImmutableReference, "ghcr.io/catthehacker/ubuntu@sha256:") {
		t.Fatalf("resolved source = %+v", resolved)
	}
	withCommandNewline, err := parseResolvedDockerSource("ghcr.io/catthehacker/ubuntu:full-latest", "linux/arm64", append(append([]byte(nil), indexRaw...), '\n'), append(append([]byte(nil), manifestRaw...), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if withCommandNewline.IndexDigest != resolved.IndexDigest {
		t.Fatalf("index digest changed because the CLI appended a newline: %s != %s", withCommandNewline.IndexDigest, resolved.IndexDigest)
	}
}

func TestDockerSandboxesArtifactIdentityChangesWithEveryFreshnessInput(t *testing.T) {
	base := Manifest{
		SchemaVersion:        ManifestSchemaVersion,
		ProviderType:         "docker-sandboxes",
		ProviderPlatform:     "linux/arm64",
		SourceType:           "docker-image",
		SourceImage:          "ghcr.io/catthehacker/ubuntu:full-latest",
		SourceDigest:         "sha256:" + strings.Repeat("a", 64),
		SourcePlatformDigest: "sha256:" + strings.Repeat("b", 64),
		RunnerVersion:        "2.332.0",
		TemplateInputs:       []FileDigest{{Path: "Dockerfile", SHA256: strings.Repeat("c", 64)}},
		CustomInstallScripts: []FileDigest{{Path: "custom.sh", SHA256: strings.Repeat("d", 64)}},
	}
	baseHash, err := ManifestHash(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Manifest){
		func(value *Manifest) { value.SourceDigest = "sha256:" + strings.Repeat("e", 64) },
		func(value *Manifest) { value.SourcePlatformDigest = "sha256:" + strings.Repeat("f", 64) },
		func(value *Manifest) { value.RunnerVersion = "2.333.0" },
		func(value *Manifest) { value.TemplateInputs[0].SHA256 = strings.Repeat("1", 64) },
		func(value *Manifest) { value.CustomInstallScripts[0].SHA256 = strings.Repeat("2", 64) },
		func(value *Manifest) {
			value.HostTrust = &HostTrustMetadata{Generation: "sha256:" + strings.Repeat("3", 64)}
		},
	}
	for index, mutate := range mutations {
		changed := base
		changed.TemplateInputs = append([]FileDigest(nil), base.TemplateInputs...)
		changed.CustomInstallScripts = append([]FileDigest(nil), base.CustomInstallScripts...)
		mutate(&changed)
		hash, err := ManifestHash(changed)
		if err != nil {
			t.Fatal(err)
		}
		if hash == baseHash {
			t.Fatalf("freshness mutation %d did not change artifact identity", index)
		}
	}
}

func TestVerifiedDockerSandboxesBuildArtifactAcceptsOnlyCompleteExactEvidence(t *testing.T) {
	root := t.TempDir()
	manifestHash := strings.Repeat("a", 64)
	source := ResolvedDockerSource{
		Reference:            "ghcr.io/catthehacker/ubuntu:full-latest",
		ImmutableReference:   "ghcr.io/catthehacker/ubuntu@sha256:" + strings.Repeat("b", 64),
		IndexDigest:          "sha256:" + strings.Repeat("b", 64),
		PlatformDigest:       "sha256:" + strings.Repeat("c", 64),
		Platform:             "linux/arm64",
		CompressedLayerBytes: 123,
	}
	archivePath := filepath.Join(root, "runner-template.tar")
	if err := os.WriteFile(archivePath, []byte("verified archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveSHA, archiveBytes, err := hashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := dockerSandboxesTemplateMetadata{
		SchemaVersion: dockerSandboxesMetadataSchema,
		Profile:       "full-latest",
		Platform:      source.Platform,
		ManifestHash:  manifestHash,
		Source:        source,
		Artifacts:     make(map[string]artifactEvidence),
	}
	templateDigest := "sha256:" + strings.Repeat("d", 64)
	metadata.Template.Tag = "docker.io/library/epar-docker-sandboxes-catthehacker-full-latest:test-arm64"
	metadata.Template.Digest = templateDigest
	metadata.Template.CacheID = strings.Repeat("d", 12)
	metadata.Template.RootDisk = "90GiB"
	metadata.Template.Archive = filepath.Base(archivePath)
	metadata.Template.ArchiveSHA256 = archiveSHA
	metadata.Template.ArchiveBytes = archiveBytes
	metadata.Compatibility.TemplateSchemaVersion = 1
	metadata.Compatibility.RunnerExecution = "direct-actions-listener"
	metadata.Compatibility.DockerDaemonOwner = "docker-sandboxes-runtime"
	metadata.Compatibility.ExpectedDockerDaemonCount = 1
	for _, name := range []string{"buildMetadata", "provenance", "sbom", "softwareInventory", "compatibility"} {
		path := filepath.Join(root, name+".json")
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, _, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Artifacts[name] = artifactEvidence{Path: filepath.Base(path), SHA256: digest}
	}
	metadataPath := filepath.Join(root, "template-metadata.json")
	if err := writeJSONFile(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}
	_, artifact, _, _, valid, err := verifiedDockerSandboxesBuildArtifact(root, metadataPath, archivePath, manifestHash, source)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || artifact.Digest != templateDigest || artifact.Platform != "linux/arm64" || artifact.RootDisk != "90GiB" {
		t.Fatalf("verified artifact = %+v, valid=%t", artifact, valid)
	}

	if err := os.WriteFile(filepath.Join(root, "sbom.json"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, valid, err = verifiedDockerSandboxesBuildArtifact(root, metadataPath, archivePath, manifestHash, source)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("corrupted evidence was accepted for interrupted-build resume")
	}
}
