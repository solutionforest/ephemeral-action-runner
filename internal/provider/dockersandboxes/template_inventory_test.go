package dockersandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func inventoryArray(t *testing.T, wrapper, key string) string {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(wrapper), &document); err != nil {
		t.Fatal(err)
	}
	return string(document[key])
}

func TestTemplateInventorySupportedEnvelopes(t *testing.T) {
	legacy, err := parseTemplateInventory([]byte(templateListJSON))
	if err != nil {
		t.Fatal(err)
	}
	modern := inventoryArray(t, templateListJSON, "images")
	images, err := parseTemplateInventory([]byte(modern))
	if err != nil || !reflect.DeepEqual(images, legacy) {
		t.Fatalf("array inventory = %v, %v; want %v", images, err, legacy)
	}
	p, done := scriptedProvider(t, commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: modern}})
	if err := p.verifyImportedTemplate(context.Background(), testTemplate, "39cf20eca861"); err != nil {
		t.Fatal(err)
	}
	done()
}

func TestTemplateInventoryFullImageIDsKeepExistingCacheIdentity(t *testing.T) {
	full := strings.Replace(templateListJSON, `"39cf20eca861"`, `"`+testDigest+`"`, 1)
	for name, inventory := range map[string]string{"wrapped": full, "array": inventoryArray(t, full, "images")} {
		t.Run(name, func(t *testing.T) {
			artifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-template:one", CacheID: "39cf20eca861", Digest: testDigest}
			inventory = strings.Replace(inventory, `"docker.io/docker/sandbox-templates"`, `"docker.io/library/epar-template"`, 1)
			inventory = strings.Replace(inventory, `"tag":"shell-docker"`, `"tag":"one"`, 1)
			read := commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: inventory}}
			p, done := scriptedProvider(t, read, read, read, read,
				commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}}, read,
				commandStep{args: []string{"template", "rm", "--force", artifact.CacheID}},
				commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
			)
			templates, err := p.CachedTemplates(context.Background())
			if err != nil || len(templates) != 1 || templates[0].CacheID != artifact.CacheID || templates[0].Reference != artifact.Reference {
				t.Fatalf("cached templates = %v, %v", templates, err)
			}
			if id, found, err := p.ResolveTemplateCacheID(context.Background(), artifact.Reference); err != nil || !found || id != artifact.CacheID {
				t.Fatalf("resolved cache identity = %q, %t, %v", id, found, err)
			}
			if err := p.VerifyImportedTemplate(context.Background(), artifact); err != nil {
				t.Fatal(err)
			}
			if exists, err := p.ObserveTemplate(context.Background(), artifact); err != nil || !exists {
				t.Fatalf("existing receipt observation = %t, %v", exists, err)
			}
			if err := p.RemoveTemplate(context.Background(), artifact); err != nil {
				t.Fatal(err)
			}
			done()
		})
	}
}

func TestTemplateInventoryV053WindowsCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/sbx-v0.53.0-template-ls.json")
	if err != nil {
		t.Fatal(err)
	}
	templates, err := parseTemplateInventory(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 3 || templates[0].ID != "d86a6cdc105a" || templates[1].ID != "9f6ff1ca4a25" || templates[2].ID != templates[1].ID {
		t.Fatalf("captured Windows template identities = %v", templates)
	}
	if templates[1].Repository+":"+templates[1].Tag != "docker.io/library/epar-docker-sandboxes-prebuilt-base:6bda3be539a7ad3a2a01c4a0d2a21b6743d8c408e24ea5a5bde1e5cc30c700a9" || templates[2].Repository+":"+templates[2].Tag != "docker.io/library/sandbox-kit-assembled:90d0c1e9696e60df" || templates[1].Size != 19850892400 {
		t.Fatalf("captured Windows template aliases = %v", templates)
	}
}

func TestTemplateInventoryRejectsMalformedFullImageIDs(t *testing.T) {
	for _, id := range []string{
		"sha256:39cf20eca861",
		testDigest[:len(testDigest)-1],
		testDigest + "0",
		testDigest[:len(testDigest)-1] + "g",
		strings.ToUpper(testDigest),
		" " + testDigest,
	} {
		inventory := strings.Replace(templateListJSON, `"39cf20eca861"`, `"`+id+`"`, 1)
		if images, err := parseTemplateInventory([]byte(inventory)); err == nil || images != nil || !strings.Contains(err.Error(), "category=invalid_image_schema") {
			t.Fatalf("malformed full ID %q = %v, %v", id, images, err)
		}
	}
}

func TestTemplateInventoryFullImageAliasesRejectAmbiguousShortIDs(t *testing.T) {
	for _, aliasFirst := range []bool{false, true} {
		inventory := strings.ReplaceAll(templateAliasesJSON(t, aliasFirst), `"39cf20eca861"`, `"`+testDigest+`"`)
		inventory = strings.Replace(inventory, `"docker.io/docker/sandbox-templates"`, `"docker.io/library/epar-template"`, 1)
		inventory = strings.Replace(inventory, `"tag":"shell-docker"`, `"tag":"one"`, 1)
		images, err := parseTemplateInventory([]byte(inventory))
		if err != nil || len(images) != 2 || images[0].ID != "39cf20eca861" || images[1].ID != "39cf20eca861" {
			t.Fatalf("full image aliases = %v, %v", images, err)
		}
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
			commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: inventory}},
		)
		if err := p.RemoveTemplate(context.Background(), provider.TemplateArtifact{Reference: "docker.io/library/epar-template:one", CacheID: "39cf20eca861"}); err == nil {
			t.Fatal("cache-ID deletion accepted an additional unowned full-ID alias")
		}
		done()
		for _, id := range []string{testDigest[:len(testDigest)-1] + "1", "39cf20eca861"} {
			ambiguous := strings.Replace(inventory, testDigest, id, 1)
			if images, err := parseTemplateInventory([]byte(ambiguous)); err == nil || images != nil || !strings.Contains(err.Error(), "category=ambiguous_image_id") {
				t.Fatalf("ambiguous full image aliases = %v, %v", images, err)
			}
		}
	}
}

func TestTemplateInventoryDiagnosticCategories(t *testing.T) {
	for _, fixture := range []string{`{"images":[]}`, `[]`} {
		images, err := parseTemplateInventory([]byte(fixture))
		if err != nil || images == nil || len(images) != 0 {
			t.Fatalf("explicit empty inventory = %v, %v", images, err)
		}
	}
	for _, test := range []struct{ input, category string }{
		{"secret-token", "invalid_json"},
		{`{"images":[]} secret-token`, "invalid_json"},
		{`null`, "invalid_root_type"},
		{`"secret-token"`, "invalid_root_type"},
		{`[] {}`, "invalid_json"},
		{`[null]`, "invalid_image_schema"},
		{`[{"id":"secret-token","id":"other"}]`, "duplicate_json_key"},
		{`{"secret-token":[]}`, "missing_images"},
		{`{"images":null}`, "invalid_images_type"},
		{`{"images":"secret-token"}`, "invalid_images_type"},
		{`{"images":{}}`, "invalid_images_type"},
		{`{"images":["secret-token"]}`, "invalid_image_schema"},
		{`{"images":[null]}`, "invalid_image_schema"},
		{`{"images":[],"images":[]}`, "duplicate_json_key"},
		{`{"images":[],"extra":{"secret-token":1,"secret-token":2}}`, "duplicate_json_key"},
		{strings.Replace(templateListJSON, "shell-docker", "secret-token!", 1), "invalid_image_identity"},
		{strings.Replace(templateListJSON, `}]}`, `},`+strings.TrimSuffix(strings.TrimPrefix(templateListJSON, `{"images":[`), `]}`)+`]}`, 1), "duplicate_image_id"},
	} {
		t.Run(test.category+"/"+test.input, func(t *testing.T) {
			images, err := parseTemplateInventory([]byte(test.input))
			want := fmt.Sprintf("docker sandbox template inventory parse failure: category=%s stdoutBytes=%d", test.category, len(test.input))
			if err == nil || err.Error() != want || images != nil {
				t.Fatalf("parse = %v, %v; want nil, %s", images, err, want)
			}
		})
	}
}

func TestTemplateReadsRetryMalformedOutput(t *testing.T) {
	readers := map[string]func(*Provider, context.Context) error{
		"cache": func(p *Provider, ctx context.Context) error {
			images, err := p.CachedTemplates(ctx)
			if err == nil && (len(images) != 1 || images[0].Reference != testTemplate) {
				t.Errorf("unexpected templates: %v", images)
			}
			if err != nil && images != nil {
				t.Error("uncertain inventory returned templates")
			}
			return err
		},
		"verify": func(p *Provider, ctx context.Context) error {
			return p.verifyImportedTemplate(ctx, testTemplate, "39cf20eca861")
		},
	}
	for name, read := range readers {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/persistent=%t", name, persistent), func(t *testing.T) {
				second := templateListJSON
				if persistent {
					second = `{"secret-token":[]}`
				}
				p, done := scriptedProvider(t,
					commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: "secret-token"}},
					commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: second}},
				)
				var logs bytes.Buffer
				p.SetLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
				err := read(p, context.Background())
				if persistent {
					if err == nil || !strings.Contains(err.Error(), "category=missing_images") || errors.Is(err, provider.ErrTemplateNotFound) {
						t.Fatalf("persistent parse failure = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(logs.String()+fmt.Sprint(err), "secret-token") || !strings.Contains(logs.String(), "category=invalid_json") {
					t.Fatalf("unsafe or missing diagnostic: %s / %v", logs.String(), err)
				}
				done()
			})
		}
		t.Run(name+"/command-error", func(t *testing.T) {
			p, done := scriptedProvider(t, commandStep{args: []string{"template", "ls", "--json"}, err: context.Canceled})
			if err := read(p, context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("command cancellation = %v", err)
			}
			done()
		})
		t.Run(name+"/already-canceled", func(t *testing.T) {
			p, done := scriptedProvider(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := read(p, ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation = %v", err)
			}
			done()
		})
	}
}
