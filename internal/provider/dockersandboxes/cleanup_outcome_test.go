package dockersandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider/dockersandboxes/staging"
)

func TestCleanupUsesObservedOutcomeInsteadOfCommandProse(t *testing.T) {
	exitOne := sandboxCLIExitFailure(t, "1")
	for _, operation := range []string{"stop", "delete"} {
		t.Run(operation, func(t *testing.T) {
			args := []string{"stop", testName}
			completed := strings.Replace(readyListJSON, `"running"`, `"stopped"`, 1)
			if operation == "delete" {
				args, completed = []string{"rm", "--force", testName}, `[]`
			}
			for _, test := range []struct {
				name        string
				mutationErr error
				readback    string
				readbackErr error
				wantErr     bool
			}{
				{name: "observed completion", readback: completed},
				{name: "new failure wording but completed", mutationErr: exitOne, readback: completed},
				{name: "new failure wording but absent", mutationErr: exitOne, readback: `[]`},
				{name: "success still pending", readback: readyListJSON, wantErr: true},
				{name: "failure still pending", mutationErr: exitOne, readback: readyListJSON, wantErr: true},
				{name: "same name different id", readback: strings.Replace(readyListJSON, testID, "different-id", 1), wantErr: true},
				{name: "same id different name", readback: strings.Replace(readyListJSON, testName, "renamed-sandbox", 1), wantErr: true},
				{name: "unavailable readback", mutationErr: exitOne, readbackErr: errors.New("inventory unavailable"), wantErr: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					p, done := scriptedProvider(t,
						commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
						commandStep{args: args, result: provider.ExecResult{Stderr: "target result wording changed"}, err: test.mutationErr},
						commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: test.readback}, err: test.readbackErr},
					)
					var err error
					if operation == "stop" {
						err = p.Stop(context.Background(), testInstance)
					} else {
						err = p.Delete(context.Background(), testInstance)
					}
					if (err != nil) != test.wantErr {
						t.Fatalf("cleanup error = %v, want error %t", err, test.wantErr)
					}
					done()
				})
			}
		})
	}
}

func TestCleanupPreservesInterruptedAndNonExitFailures(t *testing.T) {
	exitOne := sandboxCLIExitFailure(t, "1")
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, errors.New("cannot launch CLI"), errors.Join(exitOne, errors.New("process containment failed"))} {
		for _, deleted := range []bool{false, true} {
			args := []string{"stop", testName}
			if deleted {
				args = []string{"rm", "--force", testName}
			}
			p, done := scriptedProvider(t,
				commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}},
				commandStep{args: args, err: failure},
			)
			var err error
			if deleted {
				err = p.Delete(context.Background(), testInstance)
			} else {
				err = p.Stop(context.Background(), testInstance)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("cleanup lost process failure: %v, want %v", err, failure)
			}
			done()
		}
	}
}

func TestDeletePurgesOwnedStagingOnlyAfterObservedAbsence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX CLI process fixture")
	}
	for _, mode := range []string{"immediate", "retry-delete", "retry-finalize"} {
		t.Run(mode, func(t *testing.T) {
			absent := mode == "immediate"
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			root, err := staging.Open(filepath.Join(t.TempDir(), "staging"))
			if err != nil {
				t.Fatal(err)
			}
			owned, err := root.CreateOwned(testName)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(owned.Path, "keep-until-absent")
			if err := os.WriteFile(marker, []byte("owned staging"), 0600); err != nil {
				t.Fatal(err)
			}
			fixtureDir := t.TempDir()
			before := strings.Replace(readyListJSON, strconv.Quote(testWorkspace), strconv.Quote(owned.Path), 1)
			after := before
			if absent {
				after = `[]`
			}
			for name, data := range map[string]string{"before": before, "after": after} {
				path := filepath.Join(fixtureDir, name)
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("EPAR_TEST_CLEANUP_"+strings.ToUpper(name), path)
			}
			t.Setenv("EPAR_TEST_CLEANUP_MARKER", filepath.Join(fixtureDir, "removed"))
			cli := filepath.Join(fixtureDir, "sbx")
			if err := os.WriteFile(cli, []byte(`#!/bin/sh
case "$1" in
ls) if test -f "$EPAR_TEST_CLEANUP_MARKER"; then cat "$EPAR_TEST_CLEANUP_AFTER"; else cat "$EPAR_TEST_CLEANUP_BEFORE"; fi ;;
rm) : > "$EPAR_TEST_CLEANUP_MARKER" ;;
*) exit 2 ;;
esac
`), 0700); err != nil {
				t.Fatal(err)
			}
			instance := testInstance
			instance.ReceiptVersion = "v1"
			instance.Receipt, err = json.Marshal(instanceReceipt{SchemaVersion: 1, StagingPath: owned.Path, StagingIdentity: owned.Identity})
			if err != nil {
				t.Fatal(err)
			}
			p := New(cli)
			err = p.Delete(context.Background(), instance)
			if (err == nil) != absent {
				t.Fatalf("Delete error = %v, absence %t", err, absent)
			}
			_, statErr := os.Stat(marker)
			if absent && !errors.Is(statErr, os.ErrNotExist) || !absent && statErr != nil {
				t.Fatalf("staging evidence = %v, absence %t", statErr, absent)
			}
			if !absent {
				// The CLI finishes deleting between controller attempts. A fresh
				// controller must finish the receipt-owned local cleanup too.
				if err := os.WriteFile(filepath.Join(fixtureDir, "after"), []byte(`[]`), 0600); err != nil {
					t.Fatal(err)
				}
				p = New(cli)
				if mode == "retry-finalize" {
					err = p.FinalizeAbsentInstance(context.Background(), instance)
				} else {
					err = p.Delete(context.Background(), instance)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging retained after observed absence: %v", err)
				}
			}
		})
	}
}

func TestAbsentInstanceFinalizerRefusesReappearanceAndIdentityDrift(t *testing.T) {
	for _, inventory := range []string{
		readyListJSON,
		strings.Replace(readyListJSON, testID, "different-id", 1),
		strings.Replace(readyListJSON, testName, "renamed-sandbox", 1),
	} {
		p, done := scriptedProvider(t, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: inventory}})
		if err := p.FinalizeAbsentInstance(context.Background(), testInstance); err == nil {
			t.Fatal("finalization accepted a present or identity-drifted sandbox")
		}
		done()
	}
}

func TestPolicyMutationErrorsRequireExactObservedOutcome(t *testing.T) {
	exitOne := sandboxCLIExitFailure(t, "1")
	rule := provider.NetworkPolicyRule{
		ID: "rule-one", PolicyID: "local", Scope: "sandbox:" + testName, AppliesTo: "sandbox:" + testName,
		ResourceType: "network", Decision: provider.NetworkPolicyAllow, Resources: []string{"api.example.com"},
		Origin: "scoped", Editable: true, Active: true, Status: "active",
	}
	present := policyFixture(`[{"id":"rule-one","name":"job rule","policy_id":"local","scope":"sandbox:epar-sandbox-1","applies_to":"sandbox:epar-sandbox-1","resource_type":"network","decision":"allow","resources":["api.example.com"],"origin":"scoped","status":"active","editable":true,"sandbox_id":"epar-sandbox-1"}]`)
	for _, remove := range []bool{false, true} {
		for _, outcome := range []string{"complete", "pending", "rebound"} {
			t.Run(strconv.FormatBool(remove)+"/"+outcome, func(t *testing.T) {
				steps := []commandStep{{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: readyListJSON}}}
				args := []string{"policy", "allow", "network", "--sandbox", testName, "api.example.com"}
				readArgs := []string{"policy", "ls", testName, "--include-inactive", "--json"}
				if remove {
					args = []string{"policy", "rm", "network", "--sandbox", testName, "--id", rule.ID}
					steps = append(steps, commandStep{args: readArgs, result: provider.ExecResult{Stdout: present}})
				}
				steps = append(steps, commandStep{args: args, result: provider.ExecResult{Stderr: "new CLI result wording"}, err: exitOne})
				identity := readyListJSON
				if outcome == "rebound" {
					identity = strings.Replace(identity, testID, "new-id", 1)
				}
				steps = append(steps, commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: identity}})
				if outcome != "rebound" {
					readback := present
					if remove == (outcome == "complete") {
						readback = policyFixture(`[]`)
					}
					steps = append(steps, commandStep{args: readArgs, result: provider.ExecResult{Stdout: readback}})
					if outcome == "complete" {
						steps = append(steps, commandStep{args: readArgs, result: provider.ExecResult{Stdout: readback}})
					}
				}
				p, done := scriptedProvider(t, steps...)
				var err error
				if remove {
					err = p.RemoveNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{rule})
				} else {
					err = p.ApplyNetworkPolicy(context.Background(), testInstance, []provider.NetworkPolicyRule{{Decision: rule.Decision, Resources: rule.Resources}})
				}
				if (err == nil) != (outcome == "complete") {
					t.Fatalf("policy outcome %s: %v", outcome, err)
				}
				done()
			})
		}
	}
}

func TestTemplateRemovalErrorRequiresObservedAbsence(t *testing.T) {
	exitOne := sandboxCLIExitFailure(t, "1")
	artifact := provider.TemplateArtifact{Reference: "docker.io/library/epar-template:one", CacheID: "aaaaaaaaaaaa"}
	present := `[{"id":"aaaaaaaaaaaa","repository":"docker.io/library/epar-template","tag":"one","flavor":"","created_at":"2026-07-29T00:00:00Z","size":1024}]`
	for _, absent := range []bool{false, true} {
		readback := present
		if absent {
			readback = `[]`
		}
		p, done := scriptedProvider(t,
			commandStep{args: []string{"ls", "--json"}, result: provider.ExecResult{Stdout: `[]`}},
			commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: present}},
			commandStep{args: []string{"template", "rm", "--force", artifact.CacheID}, result: provider.ExecResult{Stderr: "new CLI result wording"}, err: exitOne},
			commandStep{args: []string{"template", "ls", "--json"}, result: provider.ExecResult{Stdout: readback}},
		)
		if err := p.RemoveTemplate(context.Background(), artifact); (err == nil) != absent {
			t.Fatalf("template removal error = %v, observed absence %t", err, absent)
		}
		done()
	}
}
