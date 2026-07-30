package dockersandboxes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

const (
	testName            = "epar-sandbox-1"
	testID              = "9b6dbdf3-2ef4-47cb-8f55-55b26a790c8b"
	testWorkspace       = "/var/lib/epar/staging/job-1"
	testTemplate        = "docker.io/docker/sandbox-templates:shell-docker"
	testDigest          = "sha256:39cf20eca8610000000000000000000000000000000000000000000000000000"
	readyListJSON       = `{"sandboxes":[{"id":"9b6dbdf3-2ef4-47cb-8f55-55b26a790c8b","name":"epar-sandbox-1","status":"running","workspaces":["/var/lib/epar/staging/job-1"],"agent":"shell","additive_field":true}]}`
	emptyPortsJSON      = `[]`
	templateListJSON    = `{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"2026-07-22T07:13:19Z","size":599103243}]}`
	inspectionJSON      = `{"name":"epar-sandbox-1","agent":"shell","kits":[],"state":"running","image":"docker.io/docker/sandbox-templates:shell-docker","image_digest":"sha256:39cf20eca8610000000000000000000000000000000000000000000000000000","workspace":"/var/lib/epar/staging/job-1","network":"epar-sandbox-1","network_policy":{"scope":"global"},"proxy":"172.17.0.1:3128","mcp_gateway":false,"sessions":0,"daemon_version":"fixture-current","daemon_uptime":"1h"}`
	healthyDiagnoseJSON = `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"healthy","detail":"","hint":""}],"summary":{"pass":1,"warn":0,"fail":0,"skip":0}}`
)

var testInstance = provider.Instance{Name: testName, ProviderID: testID, Source: "shell", State: "running"}

func TestCreateDryRunFailsBeforeProviderSideEffects(t *testing.T) {
	p := NewWithDryRun("sbx", true)
	called := false
	p.runCommand = func(context.Context, commandRequest) (provider.ExecResult, error) {
		called = true
		return provider.ExecResult{}, nil
	}
	_, err := p.Create(context.Background(), provider.CreateRequest{Name: testName})
	if err == nil || !strings.Contains(err.Error(), "does not support dry-run") {
		t.Fatalf("Create() error = %v", err)
	}
	if called {
		t.Fatal("dry-run Create invoked a provider command")
	}
}

func TestStartDaemonUsesExactDetachedCommand(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"daemon", "start", "--detach"}},
	)
	if err := p.StartDaemon(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestRemoveTemplateUsesExactCacheIDAndRefusesLiveSandboxes(t *testing.T) {
	artifact := provider.TemplateArtifact{
		Reference: "docker.io/library/epar-template:one",
		CacheID:   "aaaaaaaaaaaa",
		Digest:    "sha256:aaaaaaaaaaaa0000000000000000000000000000000000000000000000000000",
	}
	template := `{"images":[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"one","flavor":"","created_at":"2026-07-29T00:00:00Z","size":1024}]}`
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: template}},
		commandStep{args: []string{"template", "rm", "aaaaaaaaaaaa"}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[]}`}},
	)
	if err := p.RemoveTemplate(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	done()

	p, done = scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
	)
	if err := p.RemoveTemplate(context.Background(), artifact); err == nil || !strings.Contains(err.Error(), "while 1 Docker Sandbox") {
		t.Fatalf("RemoveTemplate() error = %v, want live Sandbox refusal", err)
	}
	done()
}

func TestObserveTemplateRequiresExactReferenceAndCacheID(t *testing.T) {
	artifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-template:one", CacheID: "aaaaaaaaaaaa"}
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"one","flavor":"","created_at":"2026-07-29T00:00:00Z","size":1024}]}`}},
	)
	exists, err := p.ObserveTemplate(context.Background(), artifact)
	if err != nil || !exists {
		t.Fatalf("ObserveTemplate() = %t, %v, want true", exists, err)
	}
	done()
}

type commandStep struct {
	args        []string
	result      provider.ExecResult
	err         error
	environment map[string]string
	stdin       string
	streamOut   string
	streamErr   string
}

type cancellationSignalWriter struct {
	once    sync.Once
	started chan struct{}
}

func (writer *cancellationSignalWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	return len(data), nil
}

func scriptedProvider(t *testing.T, steps ...commandStep) (*Provider, func()) {
	t.Helper()
	p := New("sbx-test-double")
	index := 0
	p.runCommand = func(_ context.Context, request commandRequest) (provider.ExecResult, error) {
		t.Helper()
		if index >= len(steps) {
			t.Fatalf("unexpected command: %#v", request.args)
		}
		step := steps[index]
		index++
		if !reflect.DeepEqual(request.args, step.args) {
			t.Fatalf("command %d args = %#v, want %#v", index, request.args, step.args)
		}
		if !reflect.DeepEqual(request.environment, step.environment) {
			t.Fatalf("command %d environment = %#v, want %#v", index, request.environment, step.environment)
		}
		if request.stdin != nil {
			data, err := io.ReadAll(request.stdin)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != step.stdin {
				t.Fatalf("command %d stdin = %q, want %q", index, data, step.stdin)
			}
		} else if step.stdin != "" {
			t.Fatalf("command %d did not receive stdin", index)
		}
		if request.stdout != nil {
			_, _ = io.WriteString(request.stdout, step.streamOut)
		}
		if request.stderr != nil {
			_, _ = io.WriteString(request.stderr, step.streamErr)
		}
		return step.result, step.err
	}
	return p, func() {
		t.Helper()
		if index != len(steps) {
			t.Fatalf("executed %d commands, want %d", index, len(steps))
		}
	}
}

func TestCreateUsesHealthyDiagnosticsAndExactArgv(t *testing.T) {
	wantCreate := []string{
		"create", "--name", testName,
		"--cpus", "4",
		"--memory", "8g",
		"--template", "docker.io/docker/sandbox-templates:shell-docker",
		"shell", testWorkspace,
	}
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: `No secrets found for scope "(global)".`}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{
			args: wantCreate,
			environment: map[string]string{
				"DOCKER_SANDBOXES_ROOT_SIZE":   "40g",
				"DOCKER_SANDBOXES_DOCKER_SIZE": "60g",
			},
		},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	instance, err := p.Create(context.Background(), provider.CreateRequest{
		Name:           testName,
		Template:       testTemplate,
		TemplateDigest: testDigest,
		StagingPath:    testWorkspace,
		CPUs:           4,
		Memory:         "8g",
		RootDisk:       "40g",
		DockerDisk:     "60g",
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != testName || instance.ProviderID != testID {
		t.Fatalf("instance = %#v", instance)
	}
	done()
}

func TestSplitTemplateReferenceCanonicalizesDockerHubNames(t *testing.T) {
	tests := map[string]string{
		"epar-template:version":                       "docker.io/library/epar-template",
		"docker/sandbox-templates:shell-docker":       "docker.io/docker/sandbox-templates",
		"docker.io/library/epar-template:version":     "docker.io/library/epar-template",
		"registry.example.test/team/template:version": "registry.example.test/team/template",
		"localhost:5000/team/template:version":        "localhost:5000/team/template",
	}
	for reference, expectedRepository := range tests {
		repository, _, err := splitTemplateReference(reference)
		if err != nil {
			t.Fatalf("splitTemplateReference(%q): %v", reference, err)
		}
		if repository != expectedRepository {
			t.Fatalf("splitTemplateReference(%q) repository = %q, want %q", reference, repository, expectedRepository)
		}
	}
}

func TestCreateFailsClosedOnCachedTemplateIdentityMismatch(t *testing.T) {
	mismatch := strings.Replace(templateListJSON, "39cf20eca861", "aaaaaaaaaaaa", 1)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: `No secrets found for scope "(global)".`}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: mismatch}},
	)
	if _, err := p.Create(context.Background(), validCreateRequest()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestCreateSucceedsWithImportedTemplateAndNoDockerStagingImage(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: `No secrets found for scope "(global)".`}},
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}},
		commandStep{args: []string{"create", "--name", testName, "--cpus", "4", "--memory", "8g", "--template", testTemplate, "shell", testWorkspace}, environment: map[string]string{}},
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}},
		commandStep{args: []string{"exec", "-i", testName, "--", "bash", "-lc", directWorkspaceVerificationScript}},
	)
	if _, err := p.Create(context.Background(), validCreateRequest()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestCreateFailsClosedWithoutEchoingGlobalSecretMetadata(t *testing.T) {
	const listedMetadata = "github registry masked-prefix masked-suffix"
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: listedMetadata}},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "global secrets are configured") || strings.Contains(err.Error(), listedMetadata) {
		t.Fatalf("global-secret preflight error = %v", err)
	}
	done()
}

func TestInstanceAdmissionUsesExactInspectionAndRejectsAttachedCapabilities(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: inspectionJSON}})
		if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("different daemon version", func(t *testing.T) {
		fixture := strings.Replace(inspectionJSON, `"daemon_version":"fixture-current"`, `"daemon_version":"fixture-next"`, 1)
		p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
		if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	for _, mutation := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "kit", old: `"kits":[]`, new: `"kits":[{"name":"docker-auth"}]`},
		{name: "mcp", old: `"mcp_gateway":false`, new: `"mcp_gateway":true`},
		{name: "secret", old: `"state":"running"`, new: `"state":"running","secrets":["registry"]`},
		{name: "port", old: `"state":"running"`, new: `"state":"running","published_ports":["8080:80"]`},
		{name: "auth", old: `"state":"running"`, new: `"state":"running","auth_mode":"docker-login"`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			fixture := strings.Replace(inspectionJSON, mutation.old, mutation.new, 1)
			p, done := identityAdmissionScript(t, commandStep{args: []string{"inspect", "--json", testName}, result: provider.ExecResult{Stdout: fixture}})
			if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err == nil {
				t.Fatal("attached capability was accepted")
			}
			done()
		})
	}
}

func TestInstanceAdmissionRejectsPublishedPortInventory(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture string
		message string
	}{
		{name: "published", fixture: `[{"host_ip":"127.0.0.1","host_port":60002,"sandbox_port":9418,"protocol":"tcp"}]`, message: "forbidden published port"},
		{name: "null", fixture: `null`, message: "unsupported JSON schema"},
		{name: "wrapper", fixture: `{"ports":[]}`, message: "unsupported JSON schema"},
		{name: "trailing-json", fixture: `[] {}`, message: "unsupported JSON schema"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: test.fixture}},
			)
			if err := p.VerifyInstanceAdmission(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("published port admission error = %v", err)
			}
			done()
		})
	}
}

func TestChildEnvironmentStripsHostSSHAgent(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/host/agent.sock")
	t.Setenv("SSH_AGENT_PID", "4242")
	environment := childEnvironment(nil)
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(key, "SSH_AUTH_SOCK") || strings.EqualFold(key, "SSH_AGENT_PID") {
			t.Fatalf("host SSH agent variable survived child environment filtering: %q", key)
		}
	}
}

func TestTemplateInventorySchemaFailsClosed(t *testing.T) {
	for _, fixture := range []string{
		`[]`,
		`{"templates":[]}`,
		`{"images":null}`,
		`{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"not-a-time","size":599103243}]}`,
		`{"images":[{"id":"39cf20eca861","repository":"docker.io/docker/sandbox-templates","tag":"shell-docker","flavor":"shell-docker","created_at":"2026-07-22T07:13:19Z","size":"599103243"}]}`,
	} {
		if _, err := parseTemplateInventory([]byte(fixture)); err == nil {
			t.Fatalf("template schema drift was accepted: %s", fixture)
		}
	}
}

func TestParseTemplateInventoryAcceptsCustomTemplateWithoutFlavor(t *testing.T) {
	images, err := parseTemplateInventory([]byte(`{"images":[{"id":"f40a31d1d676","repository":"docker.io/library/epar-docker-sandboxes-catthehacker-act-22.04","tag":"20260715-amd64","created_at":"2026-07-23T04:37:43Z","size":1019288120}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Flavor != "" || images[0].ID != "f40a31d1d676" {
		t.Fatalf("custom template inventory = %#v", images)
	}
}

func TestCachedTemplatesUsesMachineReadableInventoryWithoutVersionGate(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: templateListJSON}},
	)
	templates, err := p.CachedTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 {
		t.Fatalf("templates = %#v", templates)
	}
	template := templates[0]
	if template.Reference != testTemplate || template.CacheID != "39cf20eca861" || !template.CreatedAt.Equal(time.Date(2026, time.July, 22, 7, 13, 19, 0, time.UTC)) || template.SizeBytes != 599103243 {
		t.Fatalf("cached template = %#v", template)
	}
	done()
}

func TestCachedTemplatesFailsClosedOnMalformedInventory(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `{"images":[{"id":"not-a-cache-id"}]}`}},
	)
	if _, err := p.CachedTemplates(context.Background()); err == nil {
		t.Fatal("malformed template inventory was accepted")
	}
	done()
}

func TestReadGlobalNetworkPolicyUsesGlobalOnlyReadback(t *testing.T) {
	fixture := policyFixture(`[
		{"id":"global-1","name":"global","policy_id":"local","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"local","status":"active","editable":true},
		{"id":"sandbox-1","name":"sandbox","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"deny","resources":["**"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"}
	]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"policy", "ls", "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	rules, err := p.ReadGlobalNetworkPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != "global-1" || rules[0].Scope != "global" || rules[0].AppliesTo != "all" || !rules[0].Active {
		t.Fatalf("global policy rules = %#v", rules)
	}
	done()
}

func TestReadGlobalNetworkPolicyFailsClosedOnMalformedJSON(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"policy", "ls", "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: `{"rules":null}`}},
	)
	if _, err := p.ReadGlobalNetworkPolicy(context.Background()); err == nil {
		t.Fatal("malformed global policy was accepted")
	}
	done()
}

func TestDiagnosticsGateFailsClosedBeforeMutation(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":"restart the daemon"}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`}},
	)
	_, err := p.Create(context.Background(), validCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "1 failed check") || !strings.Contains(err.Error(), "sbx diagnose --output json") || !strings.Contains(err.Error(), "hints for each failed check") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestAdmissionUsesDiagnosticsWithoutReadingVersion(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: `No secrets found for scope "(global)".`}},
	)
	if err := p.VerifyAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestAdmissionRechecksDiagnostics(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: healthyDiagnoseJSON}},
		commandStep{args: []string{"secret", "ls", "-g"}, result: provider.ExecResult{Stdout: `No secrets found for scope "(global)".`}},
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":"restart the daemon"}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`}},
	)
	if err := p.VerifyAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyAdmission(context.Background()); err == nil || !strings.Contains(err.Error(), "1 failed check") || !strings.Contains(err.Error(), "sbx diagnose --output json") || !strings.Contains(err.Error(), "hints for each failed check") {
		t.Fatalf("failed diagnostics were accepted: %v", err)
	}
	done()
}

func TestInventoryParsesWrapperAndFailsClosedOnSchemaDrift(t *testing.T) {
	items, err := parseInventory([]byte(readyListJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance.Name != testInstance.Name || items[0].Instance.ProviderID != testInstance.ProviderID || !reflect.DeepEqual(items[0].Workspaces, []string{testWorkspace}) {
		t.Fatalf("items = %#v", items)
	}
	for _, fixture := range []string{
		`[]`,
		`{"items":[]}`,
		`{"sandboxes":null}`,
		`{"sandboxes":[{"name":"epar-sandbox-1","status":"running","workspaces":["/staging"]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","state":"running","workspaces":["/staging"]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running"}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running","workspaces":[]}]}`,
		`{"sandboxes":[{"id":"id-1","name":"epar-sandbox-1","status":"running","workspaces":["/staging","/staging"]}]}`,
	} {
		if _, err := parseInventory([]byte(fixture)); err == nil {
			t.Fatalf("schema drift was accepted: %s", fixture)
		}
	}
}

func TestLifecycleCommandsUseExactIdentityAndArgv(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"exec", "-i", testName, "--", "/bin/sleep", "infinity"}})
		if _, err := p.Start(context.Background(), testInstance, provider.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("verify", func(t *testing.T) {
		p, done := identityScript(t, commandStep{
			args:   []string{"exec", "-i", testName, "--", "bash", "-lc", runtimeVerificationScript},
			result: provider.ExecResult{Stdout: `"29.5.3"`},
		})
		info, err := p.VerifyRuntime(context.Background(), testInstance)
		if err != nil || !info.Ready || info.Runtime != "docker" || info.Version != "29.5.3" {
			t.Fatalf("info = %#v, err = %v", info, err)
		}
		done()
	})
	t.Run("stop_preserves_state", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"stop", testName}})
		if err := p.Stop(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("delete_is_exact_force_remove", func(t *testing.T) {
		p, done := identityScript(t, commandStep{args: []string{"rm", "--force", testName}})
		if err := p.Delete(context.Background(), testInstance); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("address_is_unavailable_without_command", func(t *testing.T) {
		p, done := scriptedProvider(t)
		address, available, err := p.Address(context.Background(), testInstance, 30)
		if err != nil || available || address != "" {
			t.Fatalf("address = %q, available = %v, err = %v", address, available, err)
		}
		done()
	})
}

func TestExecPreservesGuestArgvStdinAndRedactsAllSurfaces(t *testing.T) {
	const secret = "sentinel-sandbox-secret"
	var stdout, stderr bytes.Buffer
	p, done := identityScript(t, commandStep{
		args:      []string{"exec", "-i", testName, "--", "sh", "-c", "printf '%s' \"$1\"", "sh", "semi;colon", "--privileged"},
		stdin:     "payload\n",
		streamOut: "stream " + secret,
		streamErr: "TOKEN=" + secret,
		result: provider.ExecResult{
			Stdout: "result " + secret,
			Stderr: "SECRET=" + secret,
		},
		err: errors.New("exit status 17: " + secret),
	})
	result, err := p.Exec(context.Background(), testInstance,
		[]string{"sh", "-c", "printf '%s' \"$1\"", "sh", "semi;colon", "--privileged"},
		provider.ExecOptions{Stdin: "payload\n", SensitiveValues: []string{secret}, Stdout: &stdout, Stderr: &stderr},
	)
	if err == nil {
		t.Fatal("expected fake execution error")
	}
	combined := stdout.String() + stderr.String() + result.Stdout + result.Stderr + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("secret leaked: %q", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %q", combined)
	}
	done()
}

func TestExecCancellationPropagatesWithoutRealSbx(t *testing.T) {
	p := New("sbx-test-double")
	call := 0
	p.runCommand = func(ctx context.Context, request commandRequest) (provider.ExecResult, error) {
		call++
		switch call {
		case 1:
			if !reflect.DeepEqual(request.args, []string{"ls", "--json"}) {
				t.Fatalf("identity args = %#v", request.args)
			}
			return provider.ExecResult{Stdout: readyListJSON}, nil
		case 2:
			if !reflect.DeepEqual(request.args, []string{"exec", "-i", testName, "--", "sleep", "30"}) {
				t.Fatalf("exec args = %#v", request.args)
			}
			<-ctx.Done()
			return provider.ExecResult{}, ctx.Err()
		default:
			t.Fatalf("unexpected call %d", call)
			return provider.ExecResult{}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := p.Exec(ctx, testInstance, []string{"sleep", "30"}, provider.ExecOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRawNormalizesCommandContextKillToCancellation(t *testing.T) {
	if os.Getenv("EPAR_DOCKER_SANDBOXES_RUN_RAW_HELPER") == "1" {
		_, _ = os.Stdout.WriteString("ready\n")
		time.Sleep(30 * time.Second)
		return
	}
	t.Setenv("EPAR_DOCKER_SANDBOXES_RUN_RAW_HELPER", "1")

	p := New(os.Args[0])
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	type rawResult struct {
		result provider.ExecResult
		err    error
	}
	finished := make(chan rawResult, 1)
	go func() {
		result, err := p.runRaw(ctx, commandRequest{
			args:        []string{"-test.run=^TestRunRawNormalizesCommandContextKillToCancellation$"},
			operation:   "test helper",
			outputLimit: defaultOutputLimit,
			stdout:      &cancellationSignalWriter{started: started},
		})
		finished <- rawResult{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runRaw helper process did not start")
	}
	cancel()
	select {
	case outcome := <-finished:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("runRaw cancellation error = %v, want context.Canceled", outcome.err)
		}
		var exitErr *exec.ExitError
		if errors.As(outcome.err, &exitErr) {
			t.Fatalf("runRaw retained cancellation-induced process exit as a concrete error: %v", outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRaw did not return after cancellation")
	}
}

func TestStopAndDeleteAreIdempotentWhenInventorySaysMissing(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Provider) error
	}{
		{name: "stop", call: func(p *Provider) error { return p.Stop(context.Background(), testInstance) }},
		{name: "delete", call: func(p *Provider) error { return p.Delete(context.Background(), testInstance) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `{"sandboxes":[]}`}})
			if err := test.call(p); err != nil {
				t.Fatal(err)
			}
			done()
		})
	}
}

func TestIdentityMismatchFailsBeforeStateMutation(t *testing.T) {
	mismatch := strings.Replace(readyListJSON, testID, "different-id", 1)
	p, done := scriptedProvider(t, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: mismatch}})
	if err := p.Delete(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestDiagnosticsUsesBoundedMachineReadableCommands(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: `{"status":"running","socket":"\\\\.\\pipe\\docker_kaname_sandboxd","logs":"C:\\logs\\daemon.log"}`}},
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"ok","detail":"","hint":""},{"name":"optional update","status":"warn","message":"available","detail":"","hint":""},{"name":"optional integration","status":"skip","message":"not configured","detail":"","hint":""}],"summary":{"pass":1,"warn":1,"fail":0,"skip":1}}`}},
	)
	diagnostics, err := p.Diagnostics(context.Background(), testInstance)
	if err != nil || !diagnostics.Healthy || diagnostics.ChecksPassed != 1 || diagnostics.ChecksWarned != 1 || diagnostics.ChecksSkipped != 1 {
		t.Fatalf("diagnostics = %#v, err = %v", diagnostics, err)
	}
	done()
}

func TestVerifyHostReadinessAcceptsWarningsWithPassingChecksAndNoFailures(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"healthy","detail":"","hint":""},{"name":"optional update","status":"warn","message":"available","detail":"","hint":""}],"summary":{"pass":1,"warn":1,"fail":0,"skip":0}}`}},
	)
	readiness, err := p.VerifyHostReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if readiness.ChecksPassed != 1 || readiness.ChecksWarned != 1 || readiness.ChecksFailed != 0 || readiness.ChecksSkipped != 0 {
		t.Fatalf("readiness = %#v", readiness)
	}
	done()
}

func TestVerifyHostReadinessRejectsFailedOrEmptyDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "failed check",
			fixture: `{"version":"1.0","checks":[{"name":"daemon","status":"fail","message":"unhealthy","detail":"","hint":""}],"summary":{"pass":0,"warn":0,"fail":1,"skip":0}}`,
			want:    "1 failed check",
		},
		{
			name:    "no passing check",
			fixture: `{"version":"1.0","checks":[{"name":"optional","status":"skip","message":"not applicable","detail":"","hint":""}],"summary":{"pass":0,"warn":0,"fail":0,"skip":1}}`,
			want:    "no passing checks",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t,
				commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: test.fixture}},
			)
			if _, err := p.VerifyHostReadiness(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "sbx diagnose --output json") {
				t.Fatalf("VerifyHostReadiness() error = %v, want %q and diagnostic command", err, test.want)
			}
			done()
		})
	}
}

func TestVerifyHostReadinessRejectsCommandAndSchemaFailures(t *testing.T) {
	tests := []struct {
		name string
		step commandStep
		want string
	}{
		{
			name: "command failure",
			step: commandStep{args: []string{"diagnose", "--output", "json"}, err: errors.New("diagnose unavailable")},
			want: "diagnose unavailable",
		},
		{
			name: "malformed json",
			step: commandStep{args: []string{"diagnose", "--output", "json"}, result: provider.ExecResult{Stdout: `{"summary":`}},
			want: "unsupported json schema",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, done := scriptedProvider(t, test.step)
			if _, err := p.VerifyHostReadiness(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "sbx diagnose --output json") {
				t.Fatalf("VerifyHostReadiness() error = %v, want %q and diagnostic command", err, test.want)
			}
			done()
		})
	}
}

func TestDiagnosticsRequiresEverySummaryCount(t *testing.T) {
	fixture := `{"version":"1.0","checks":[{"name":"daemon","status":"pass","message":"ok","detail":"","hint":""}],"summary":{"pass":1,"warn":0,"fail":0}}`
	if _, _, _, _, err := parseDiagnose([]byte(fixture)); err == nil {
		t.Fatal("diagnostics accepted a missing summary count")
	}
}

func TestDiagnosticsRejectsOversizedOutput(t *testing.T) {
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"daemon", "status", "--json"}, result: provider.ExecResult{Stdout: strings.Repeat("x", diagnosticOutputLimit+1)}},
	)
	if _, err := p.Diagnostics(context.Background(), testInstance); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyCommandsAreSandboxScopedAndReadBackExactRules(t *testing.T) {
	globalRule := `{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true}`
	sandboxRule := `{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com","*.packages.example.com:443"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1","additive":42}`
	policyJSON := policyFixture(`[` + globalRule + `,` + sandboxRule + `]`)
	rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com", "*.packages.example.com:443"}}
	t.Run("apply", func(t *testing.T) {
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com,*.packages.example.com:443"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyJSON}},
		)
		if err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{rule}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("apply_open_public_egress", func(t *testing.T) {
		openRule := `{"id":"rule-open","name":"open public egress","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["**"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}`
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "allow", "network", "--sandbox", testName, "**"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyFixture(`[` + globalRule + `,` + openRule + `]`)}},
		)
		if err := p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{{
			Decision:  provider.NetworkPolicyAllow,
			Resources: []string{"**"},
		}}); err != nil {
			t.Fatal(err)
		}
		done()
	})
	t.Run("remove_by_stable_rule_id", func(t *testing.T) {
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyJSON}},
			commandStep{args: []string{"policy", "rm", "network", "--sandbox", testName, "--id", "rule-1"}},
			commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: policyFixture(`[` + globalRule + `]`)}},
		)
		remove := provider.NetworkPolicyRule{
			ID:           "rule-1",
			PolicyID:     "local",
			Scope:        "sandbox:" + testName,
			AppliesTo:    "sandbox:" + testName,
			ResourceType: "network",
			Resources:    append([]string(nil), rule.Resources...),
			Decision:     provider.NetworkPolicyAllow,
			Origin:       "scoped",
			Editable:     true,
		}
		if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{remove}); err != nil {
			t.Fatal(err)
		}
		done()
	})
}

func TestPolicyRemovalRefusesChangedStableIdentity(t *testing.T) {
	fixture := policyFixture(`[{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	changed := provider.NetworkPolicyRule{
		ID:           "rule-1",
		PolicyID:     "local",
		Scope:        "sandbox:" + testName,
		AppliesTo:    "sandbox:" + testName,
		ResourceType: "network",
		Resources:    []string{"different.example.com"},
		Decision:     provider.NetworkPolicyAllow,
		Origin:       "scoped",
		Editable:     true,
	}
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{changed}); err == nil || !strings.Contains(err.Error(), "stable identity changed") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyReadPreservesGlobalAndSandboxAttribution(t *testing.T) {
	fixture := policyFixture(`[
		{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true},
		{"id":"rule-1","name":"job allowlist","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"deny","resources":["**"],"origin":"scoped","status":"inactive","editable":true,"sandbox_id":"epar-sandbox-1"},
		{"id":"other-1","name":"another sandbox","policy_id":"local","scope":"sandbox:other-sandbox","applies_to":"sandbox:other-sandbox","resource_type":"network","decision":"allow","resources":["other.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"other-sandbox"},
		{"id":"fs-1","name":"filesystem baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"filesystem","decision":"allow","resources":["/workspace"],"origin":"local","status":"active","editable":false}
	]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %#v", rules)
	}
	global := rules[0]
	if global.ID != "global-1" || global.Name != "automatic baseline" || global.PolicyID != "local-policy" || global.Scope != "global" || global.AppliesTo != "all" || global.ResourceType != "network" || global.Origin != "local" || global.Status != "active" || !global.Editable || !global.Active {
		t.Fatalf("global attribution was not preserved: %#v", global)
	}
	local := rules[1]
	if local.Scope != "sandbox:"+testName || local.AppliesTo != "sandbox:"+testName || local.Origin != "scoped" || local.Status != "inactive" || local.Active {
		t.Fatalf("sandbox attribution was not preserved: %#v", local)
	}
	filesystem := rules[2]
	if filesystem.ID != "fs-1" || filesystem.ResourceType != "filesystem" || filesystem.Scope != "global" || filesystem.AppliesTo != "all" || filesystem.Editable {
		t.Fatalf("filesystem attribution was not preserved: %#v", filesystem)
	}
}

func TestPolicyReadPreservesExactKitAttribution(t *testing.T) {
	fixture := policyFixture(`[{"id":"kit-rule-1","name":"kit:epar-sandbox-1","policy_id":"kit-policy-1","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"scoped","status":"active","editable":false,"sandbox_id":"epar-sandbox-1"}]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Scope != "sandbox:"+testName || rules[0].AppliesTo != "sandbox:"+testName || rules[0].Origin != "scoped" || rules[0].Editable || !rules[0].Active {
		t.Fatalf("kit attribution was not preserved: %#v", rules)
	}
}

func TestPolicyReadRejectsMismatchedSandboxAttribution(t *testing.T) {
	for _, fixture := range []string{
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:other-sandbox","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"other-sandbox"}]`),
	} {
		if _, err := parseNetworkPolicy([]byte(fixture), testName); err == nil {
			t.Fatalf("mismatched sandbox attribution was accepted: %s", fixture)
		}
	}
}

func TestPolicyRemovalRefusesGlobalRule(t *testing.T) {
	global := provider.NetworkPolicyRule{ID: "global-1", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "network", Resources: []string{"openrouter.ai"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Editable: true}
	fixture := policyFixture(`[{"id":"global-1","name":"automatic baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["openrouter.ai"],"origin":"local","status":"active","editable":true}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{global}); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicyRemovalRefusesFilesystemRule(t *testing.T) {
	filesystem := provider.NetworkPolicyRule{ID: "fs-1", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "filesystem", Resources: []string{"/workspace"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Editable: false}
	fixture := policyFixture(`[{"id":"fs-1","name":"filesystem baseline","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"filesystem","decision":"allow","resources":["/workspace"],"origin":"local","status":"active","editable":false}]`)
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"policy", "ls", testName, "--include-inactive", "--json"}, result: provider.ExecResult{Stdout: fixture}},
	)
	if err := p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{filesystem}); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("err = %v", err)
	}
	done()
}

func TestPolicySchemaFailsClosed(t *testing.T) {
	for _, fixture := range []string{
		`[]`,
		`{"policies":[]}`,
		`{"rules":null}`,
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"permit","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
		policyFixture(`[{"id":"rule-1","name":"rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"unknown","editable":true,"sandbox_id":"epar-sandbox-1"}]`),
	} {
		if _, err := parseNetworkPolicy([]byte(fixture), testName); err == nil {
			t.Fatalf("policy schema drift was accepted: %s", fixture)
		}
	}
}

func TestPolicyReadbackAcceptsAttributedProviderBaselinePatterns(t *testing.T) {
	fixture := policyFixture(`[{"id":"default-cert-validation","name":"default-cert-validation","policy_id":"local-policy","scope":"global","applies_to":"all","resource_type":"network","decision":"allow","resources":["**.openai.com:443","crl*.digicert.com:80","**"],"origin":"local","status":"active","editable":true}]`)
	rules, err := parseNetworkPolicy([]byte(fixture), testName)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || !reflect.DeepEqual(rules[0].Resources, []string{"**.openai.com:443", "crl*.digicert.com:80", "**"}) {
		t.Fatalf("provider baseline resources = %#v", rules)
	}
}

func TestInjectionCorpusIsRejectedBeforeCommandExecution(t *testing.T) {
	for _, name := range []string{"-other", "../other", "other/name", "name;rm", "name\nnext", "name+other", ""} {
		request := validCreateRequest()
		request.Name = name
		if err := validateCreateRequest(request); err == nil {
			t.Fatalf("name injection accepted: %q", name)
		}
	}
	for _, resource := range []string{"-api.example.com", "api.example.com,evil.example.com", "api.example.com --sandbox other", "api.example.com;reset", "api.example.com\n**", "https://api.example.com", "127.0.0.1", "10.0.0.0/8", "[::1]:443", "*.localhost"} {
		rule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{resource}}
		if err := validateNetworkRule(rule, false); err == nil {
			t.Fatalf("resource injection accepted: %q", resource)
		}
	}
	openRule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyAllow, Resources: []string{"**"}}
	if err := validateNetworkRule(openRule, false); err != nil {
		t.Fatalf("owned sandbox-scoped open rule was rejected: %v", err)
	}
	denyAllRule := provider.NetworkPolicyRule{Decision: provider.NetworkPolicyDeny, Resources: []string{"**"}}
	if err := validateNetworkRule(denyAllRule, false); err == nil {
		t.Fatal("unbounded deny wildcard was accepted")
	}
}

func TestCommandBoundaryRejectsInteractiveAndDestructiveGlobalCommands(t *testing.T) {
	for _, command := range []string{"tui", "reset", "run", "kit", "secret", "login", "logout", "setup", "ssh", "cp", "completion", "help"} {
		err := validateCommandRequest(commandRequest{args: []string{command}, operation: "test forbidden command"})
		if err == nil {
			t.Fatalf("forbidden Docker Sandboxes command %q was accepted", command)
		}
	}
	for _, arguments := range [][]string{{"inspect"}, {"inspect", testName}, {"inspect", "--json", "../other"}, {"inspect", "--debug", testName}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden inspection"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes inspection was accepted: %q", arguments)
		}
	}
	for _, arguments := range [][]string{{"ports"}, {"ports", testName}, {"ports", "../other", "--json"}, {"ports", testName, "--json", "extra"}, {"ports", testName, "--unpublish"}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden port command"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes published-port inspection was accepted: %q", arguments)
		}
	}
	for _, arguments := range [][]string{{"daemon"}, {"daemon", "start"}, {"daemon", "start", "--foreground"}, {"daemon", "stop", "--detach"}, {"daemon", "status"}, {"daemon", "status", "--debug"}} {
		if err := validateCommandRequest(commandRequest{args: arguments, operation: "test forbidden daemon command"}); err == nil {
			t.Fatalf("non-exact Docker Sandboxes daemon command was accepted: %q", arguments)
		}
	}
}

func TestExecRejectsHostPathsAndEnvironmentPassthrough(t *testing.T) {
	p, done := scriptedProvider(t)
	if _, err := p.Exec(context.Background(), testInstance, []string{"true"}, provider.ExecOptions{LogPath: "/host/log"}); err == nil {
		t.Fatal("host log path was accepted")
	}
	if _, err := p.Exec(context.Background(), testInstance, []string{"true"}, provider.ExecOptions{Env: map[string]string{"TOKEN": "secret"}}); err == nil {
		t.Fatal("guest environment passthrough was accepted")
	}
	done()
}

func identityScript(t *testing.T, operation commandStep) (*Provider, func()) {
	t.Helper()
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		operation,
	)
	return p, done
}

func identityAdmissionScript(t *testing.T, inspection commandStep) (*Provider, func()) {
	t.Helper()
	p, done := scriptedProvider(t,
		commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
		commandStep{args: []string{"ports", testName, "--json"}, result: provider.ExecResult{Stdout: emptyPortsJSON}},
		inspection,
	)
	return p, done
}

func validCreateRequest() provider.CreateRequest {
	return provider.CreateRequest{
		Name:           testName,
		Template:       testTemplate,
		TemplateDigest: testDigest,
		StagingPath:    testWorkspace,
		CPUs:           4,
		Memory:         "8g",
	}
}

func policyFixture(rules string) string {
	return `{"rules":` + rules + `}`
}
