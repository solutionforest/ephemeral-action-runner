package dockersandboxes

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	sandboxpolicy "github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/policy"
)

func templateAliasesJSON(t *testing.T, aliasFirst bool) string {
	t.Helper()
	var wrapper struct {
		Images []map[string]any `json:"images"`
	}
	if err := json.Unmarshal([]byte(templateListJSON), &wrapper); err != nil {
		t.Fatal(err)
	}
	alias := map[string]any{}
	for key, value := range wrapper.Images[0] {
		alias[key] = value
	}
	alias["repository"] = "docker.io/library/sandbox-kit-assembled"
	alias["tag"] = "f48e59b12e899635"
	delete(alias, "flavor")
	images := append(wrapper.Images, alias)
	if aliasFirst {
		images[0], images[1] = images[1], images[0]
	}
	data, err := json.Marshal(images)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestV051ImplicitPolicyFallbackPreservesBalancedBaseline(t *testing.T) {
	fallback, err := os.ReadFile("testdata/sbx-v0.51.0-policy-fallback.json")
	if err != nil {
		t.Fatal(err)
	}
	const baseline = `{"id":"baseline","name":"baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["github.com:443"],"origin":"local","status":"active","editable":true}`
	legacy, err := parseGlobalNetworkPolicy([]byte(policyFixture("[" + baseline + "]")))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := sandboxpolicy.Fingerprint(legacy)
	if err != nil {
		t.Fatal(err)
	}
	modern, err := parseGlobalNetworkPolicy([]byte(policyFixture("[" + baseline + "," + string(fallback) + "]")))
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxpolicy.VerifyBaseline(fingerprint, testName, modern); err != nil {
		t.Fatalf("implicit no-match fallback changed the balanced baseline: %v", err)
	}
	// An actual global deny-all policy remains a lockdown, even with the same name.
	explicit := strings.Replace(string(fallback), `"policy_name":`, `"policy_id":"local-policy","policy_name":`, 1)
	locked, err := parseGlobalNetworkPolicy([]byte(policyFixture("[" + baseline + "," + explicit + "]")))
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxpolicy.VerifyBaseline(fingerprint, testName, locked); err == nil {
		t.Fatal("explicit deny-all policy was ignored")
	}
	for _, mutation := range []struct{ old, replacement string }{
		{`"decision": "deny"`, `"decision": "allow"`},
		{`"editable": false`, `"editable": true`},
		{`"editable": false`, `"editable": true,"editable": false`},
		{`"scope": "global"`, `"scope": "sandbox:other"`},
		{`"status": "active"`, `"status": "inactive"`},
		{`"**"`, `"github.com"`},
		{`"net:connect:udp"`, `"fs:mount:read"`},
		{`Implicit fallback when no applicable policy decides the request.`, `explicit rule`},
	} {
		changed := strings.Replace(string(fallback), mutation.old, mutation.replacement, 1)
		if _, err := parseGlobalNetworkPolicy([]byte(policyFixture("[" + baseline + "," + changed + "]"))); err == nil {
			t.Fatalf("malformed fallback was ignored: %s", mutation.old)
		}
	}
	if _, err := parseGlobalNetworkPolicy([]byte(policyFixture("[" + baseline + "," + string(fallback) + "," + string(fallback) + "]"))); err == nil {
		t.Fatal("duplicate fallback rule was ignored")
	}
}

func TestTemplateAliasesPreserveExactObservationAndCleanup(t *testing.T) {
	artifact := provider.TemplateArtifact{Reference: testTemplate, CacheID: "39cf20eca861"}
	// Use a removable EPAR reference instead of the protected shell base.
	artifact.Reference = "docker.io/library/epar-template:one"
	for _, aliasFirst := range []bool{false, true} {
		inventory := strings.Replace(templateAliasesJSON(t, aliasFirst), `"docker.io/docker/sandbox-templates"`, `"docker.io/library/epar-template"`, 1)
		inventory = strings.Replace(inventory, `"tag":"shell-docker"`, `"tag":"one"`, 1)
		p, done := scriptedProvider(t, commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: inventory}})
		if exists, err := p.ObserveTemplate(context.Background(), artifact); err != nil || !exists {
			t.Fatalf("aliased template observation = %t, %v", exists, err)
		}
		done()
		p, done = scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
			commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: inventory}},
		)
		if err := p.RemoveTemplate(context.Background(), artifact); err == nil {
			t.Fatal("cache-ID deletion accepted an additional unowned alias")
		}
		done()
	}
}

func TestTemplateAliasesRejectConflictingImageMetadata(t *testing.T) {
	for _, mutation := range []struct{ old, replacement string }{
		{`"size":599103243`, `"size":599103244`},
		{`2026-07-22T07:13:19Z`, `2026-07-23T07:13:19Z`},
	} {
		inventory := strings.Replace(templateAliasesJSON(t, false), mutation.old, mutation.replacement, 1)
		if _, err := parseTemplateInventory([]byte(inventory)); err == nil || !strings.Contains(err.Error(), "conflicting_image_metadata") {
			t.Fatalf("conflicting aliases accepted: %v", err)
		}
	}
}

func TestCreatePinsPublishedShellKit(t *testing.T) {
	fixture, err := os.ReadFile("testdata/sbx-v0.51.0-shell-inspect.json")
	if err != nil {
		t.Fatal(err)
	}
	inspection := strings.Replace(string(fixture), `"/var/lib/epar/staging/job-1"`, strconv.Quote(testWorkspace), 1)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--format", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
		commandStep{args: []string{"version"}, result: provider.ExecResult{Stdout: "sbx version v0.51.0 7d55286de0c775148b985ed349f760f1eba06203\n"}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, verifiedShellKit, testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspection}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateAliasesJSON(t, false)}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	instance, err := p.Create(context.Background(), validCreateRequest())
	if err != nil || instance.ProviderID != testID || instance.ReceiptVersion != "v1" {
		t.Fatalf("Create() = %+v, %v, want exact receipted sandbox", instance, err)
	}
	done()
}

func TestCreateRejectsUnknownShellModeBeforeAllocation(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--format", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
		commandStep{args: []string{"version"}, result: provider.ExecResult{Stdout: "sbx version unknown"}},
	)
	if _, err := p.Create(context.Background(), validCreateRequest()); err == nil || !strings.Contains(err.Error(), "cannot select Docker Sandboxes shell") {
		t.Fatalf("Create() error = %v, want version error before allocation", err)
	}
	done()
}

func TestShellCreateSelectorReadsLegacyClientVersion(t *testing.T) {
	const output = "Client Version:  v0.31.3 8f15ed5dfabbdc512da2bbec59ff723da9390f64\r\n" +
		"Server Version:  v0.31.3 8f15ed5dfabbdc512da2bbec59ff723da9390f64\r\n"
	p, done := scriptedProvider(t, commandStep{args: []string{"version"}, result: provider.ExecResult{Stdout: output}})
	if selector, err := p.shellCreateSelector(context.Background()); err != nil || selector != "shell" {
		t.Fatalf("shellCreateSelector() = %q, %v, want legacy shell", selector, err)
	}
	done()
}

func TestV051ShellInspectionRequiresVerifiedKitAndExactTemplateAlias(t *testing.T) {
	fixture, err := os.ReadFile("testdata/sbx-v0.51.0-shell-inspect.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		mutate    func(map[string]any)
		inventory string
		wantErr   bool
	}{
		{name: "captured shell kit", inventory: templateAliasesJSON(t, false)},
		{name: "updated shell shortcut", mutate: func(v map[string]any) {
			v["kits"] = []string{"docker.io/sbx/shell@sha256:13d250dc2795739484b6e99e1fab3d482b8d2dcf32268181b86e97bd44750ef5"}
			v["image"] = "sandbox-kit-assembled:b6443c8812fc2959"
		}, wantErr: true},
		{name: "credential mixin", mutate: func(v map[string]any) {
			v["kits"] = []string{verifiedShellKit, "docker.io/sbx/shell-credentials:latest"}
		}, wantErr: true},
		{name: "empty kits with alias", mutate: func(v map[string]any) { v["kits"] = []string{} }, wantErr: true},
		{name: "empty kits with exact image", mutate: func(v map[string]any) {
			v["kits"] = []string{}
			v["image"] = testTemplate
		}, wantErr: true},
		{name: "unexpected image reference", mutate: func(v map[string]any) { v["image"] = "unrelated:latest" }, wantErr: true},
		{name: "different image digest", mutate: func(v map[string]any) { v["image_digest"] = "sha256:" + strings.Repeat("a", 64) }, wantErr: true},
		{name: "different workspace", mutate: func(v map[string]any) { v["workspace"] = "/unowned" }, wantErr: true},
		{name: "missing alias", inventory: templateListJSON, wantErr: true},
		{name: "alias changed identity", inventory: strings.ReplaceAll(templateAliasesJSON(t, false), "39cf20eca861", "aaaaaaaaaaaa"), wantErr: true},
		{name: "attached auth", mutate: func(v map[string]any) { v["auth"] = "forbidden" }, inventory: templateAliasesJSON(t, false), wantErr: true},
		{name: "published port", mutate: func(v map[string]any) { v["published_ports"] = []string{"8080:80"} }, inventory: templateAliasesJSON(t, false), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var inspection map[string]any
			if err := json.Unmarshal(fixture, &inspection); err != nil {
				t.Fatal(err)
			}
			inspection["workspace"] = testWorkspace
			if test.mutate != nil {
				test.mutate(inspection)
			}
			data, err := json.Marshal(inspection)
			if err != nil {
				t.Fatal(err)
			}
			steps := []commandStep{{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: string(data)}}}
			if test.inventory != "" {
				steps = append(steps, commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: test.inventory}})
			}
			p, done := scriptedProvider(t, steps...)
			request := validCreateRequest()
			if err := p.verifyInspection(context.Background(), testInstance, &request, "39cf20eca861", true); (err != nil) != test.wantErr {
				t.Fatalf("inspection = %v, wantErr=%t", err, test.wantErr)
			}
			done()
		})
	}
}
