package prebuilt

import (
	"strings"
	"testing"

	structpb "google.golang.org/protobuf/types/known/structpb"
)

func TestParseSLSAClaimsAcceptsStandardResourceDescriptorDigestMap(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	predicate := slsaPredicate(digest)
	claims, err := parseSLSAClaims(predicate)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SourceIndexDigest != digest || len(claims.ResolvedDependencyDigests) < 1 {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestParseSLSAClaimsRejectsAmbiguousResourceDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	predicate := slsaPredicate("sha256:" + digest)
	root := predicate.AsMap()
	buildDefinition := root["buildDefinition"].(map[string]any)
	dependencies := buildDefinition["resolvedDependencies"].([]any)
	dependencies[0].(map[string]any)["digest"] = map[string]any{"sha256": digest, "sha512": digest}
	mutated, _ := structpb.NewStruct(root)
	if _, err := parseSLSAClaims(mutated); err == nil || !strings.Contains(err.Error(), "exactly one sha256") {
		t.Fatalf("ambiguous digest error = %v", err)
	}
	dependencies[0].(map[string]any)["digest"] = "sha256:" + digest
	mutated, _ = structpb.NewStruct(root)
	if _, err := parseSLSAClaims(mutated); err == nil || !strings.Contains(err.Error(), "exactly one sha256") {
		t.Fatalf("string digest error = %v", err)
	}
}

func TestParseSLSAClaimsAcceptsMatchingGitHubWorkflowMaterial(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	predicate := slsaPredicate(digest)
	root := predicate.AsMap()
	buildDefinition := root["buildDefinition"].(map[string]any)
	dependencies := buildDefinition["resolvedDependencies"].([]any)
	gitMaterial := map[string]any{"uri": "git+https://github.com/solutionforest/ephemeral-action-runner@refs/heads/main", "digest": map[string]any{"gitCommit": strings.Repeat("a", 40)}}
	buildDefinition["resolvedDependencies"] = append([]any{gitMaterial}, dependencies...)
	mutated, _ := structpb.NewStruct(root)
	if _, err := parseSLSAClaims(mutated); err != nil {
		t.Fatal(err)
	}
	gitMaterial["digest"] = map[string]any{"gitCommit": strings.Repeat("b", 40)}
	mutated, _ = structpb.NewStruct(root)
	if _, err := parseSLSAClaims(mutated); err == nil || !strings.Contains(err.Error(), "exactly match recipe revision") {
		t.Fatalf("mismatched git material error = %v", err)
	}
}

func TestParseSPDXClaimsRequiresSHA256PackageChecksums(t *testing.T) {
	predicate, err := structpb.NewStruct(map[string]any{"spdxVersion": "SPDX-2.3", "documentNamespace": "https://spdx.example/doc", "packages": []any{map[string]any{"name": "epar", "checksums": []any{map[string]any{"algorithm": "SHA1", "checksumValue": strings.Repeat("a", 40)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseSPDXClaims(predicate); err == nil || !strings.Contains(err.Error(), "no SHA256") {
		t.Fatalf("missing SHA256 error = %v", err)
	}
}

func TestMatchEvidenceClaimsRejectsSourcePlatformMismatch(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	claims := claimsForEntry(entry)
	claims.SourcePlatformDigests = map[string]string{"linux/amd64": entry.Source.PlatformDigests["linux/amd64"], "linux/arm64": "sha256:" + strings.Repeat("b", 64)}
	claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, "sha256:"+strings.Repeat("b", 64))
	if err := matchEvidenceClaims(entry, claims); err == nil || !strings.Contains(err.Error(), "source platform") {
		t.Fatalf("source mismatch error = %v", err)
	}
}

func TestMatchEvidenceClaimsRejectsWrongRecipeRevision(t *testing.T) {
	entry := validEntry(ProfileAct, "a", StatusCandidate)
	claims := claimsForEntry(entry)
	claims.RecipeRevision = strings.Repeat("b", 40)
	if err := matchEvidenceClaims(entry, claims); err == nil || !strings.Contains(err.Error(), "recipe/runtime") {
		t.Fatalf("recipe revision mismatch error = %v", err)
	}
}

func slsaPredicate(digest string) *structpb.Struct {
	hex := strings.TrimPrefix(digest, "sha256:")
	value := func(value string) map[string]any {
		return map[string]any{"sha256": strings.TrimPrefix(value, "sha256:")}
	}
	platformDigest := map[string]any{"linux/amd64": map[string]any{"packageManifestDigest": digest}, "linux/arm64": map[string]any{"packageManifestDigest": digest}}
	tools := map[string]any{"dockerfile-frontend": map[string]any{"digest": digest}}
	root, _ := structpb.NewStruct(map[string]any{
		"buildDefinition": map[string]any{
			"externalParameters": map[string]any{
				"source":    map[string]any{"indexDigest": digest, "platformDigests": map[string]any{"linux/amd64": digest, "linux/arm64": digest}},
				"recipe":    map[string]any{"digest": digest, "revision": strings.Repeat("a", 40), "runtimeContract": "docker-sandboxes-v1", "templateSchema": float64(2)},
				"runner":    map[string]any{"version": "2.336.0", "assetDigests": map[string]any{"linux/amd64": digest}},
				"tools":     tools,
				"platforms": platformDigest,
			},
			"resolvedDependencies": []any{map[string]any{"uri": "ghcr.io/catthehacker/ubuntu", "digest": value(digest)}, map[string]any{"uri": "runner", "digest": value(digest)}},
		},
	})
	_ = hex
	return root
}

func claimsForEntry(entry Entry) EvidenceClaims {
	claims := EvidenceClaims{SubjectDigest: entry.PackageIndexDigest, SourceIndexDigest: entry.Source.IndexDigest, SourcePlatformDigests: entry.Source.PlatformDigests, RecipeDigest: entry.Recipe.Digest, RecipeRevision: entry.Recipe.RecipeRevision, RuntimeContract: entry.Recipe.RuntimeContract, TemplateSchema: entry.Recipe.TemplateSchema, RunnerVersion: entry.Runner.Version, RunnerAssetDigests: entry.Runner.AssetDigests, ToolDigests: map[string]string{}, PlatformPackageDigests: map[string]string{}, ResolvedDependencyDigests: append([]string{entry.Source.IndexDigest}, mapValues(entry.Source.PlatformDigests)...), SPDXNamespace: "https://spdx.example/doc", SPDXPackageChecksums: map[string]string{}}
	for name, digest := range expectedSPDXChecksums(entry) {
		claims.SPDXPackageChecksums[name] = strings.TrimPrefix(digest, "sha256:")
	}
	for _, tool := range entry.Tools {
		claims.ToolDigests[tool.Name] = tool.Digest
		claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, tool.Digest)
	}
	for _, platform := range entry.Platforms {
		claims.PlatformPackageDigests[platform.Platform] = platform.PackageManifestDigest
	}
	for _, digest := range entry.Runner.AssetDigests {
		claims.ResolvedDependencyDigests = append(claims.ResolvedDependencyDigests, digest)
	}
	return claims
}
