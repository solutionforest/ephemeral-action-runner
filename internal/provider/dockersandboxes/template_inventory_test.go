package dockersandboxes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestTemplateInventoryDiagnosticCategories(t *testing.T) {
	images, err := parseTemplateInventory([]byte(`{"images":[]}`))
	if err != nil || images == nil || len(images) != 0 {
		t.Fatalf("explicit empty inventory = %v, %v", images, err)
	}
	for _, test := range []struct{ input, category string }{
		{"secret-token", "invalid_json"},
		{`{"images":[]} secret-token`, "invalid_json"},
		{`null`, "invalid_root_type"},
		{`[]`, "invalid_root_type"},
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
