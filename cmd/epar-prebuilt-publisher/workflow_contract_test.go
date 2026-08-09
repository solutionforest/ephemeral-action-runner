package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowRepairsCatalogFirstPromotionBeforeNoop(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	immutableCatalogVerification := strings.Index(workflow, `verify-catalog --repository "$PACKAGE_REPOSITORY" --profile act --reference "${PACKAGE_REPOSITORY}@${catalog_digest}"`)
	reconcile := strings.Index(workflow, "- name: Reconcile an interrupted catalog-first alias promotion")
	noop := strings.Index(workflow, "- name: Determine whether the immutable tuple is already active")
	if immutableCatalogVerification < 0 || reconcile < 0 || noop < 0 || immutableCatalogVerification >= reconcile || reconcile >= noop {
		t.Fatalf("immutable catalog verification and alias reconciliation must run before no-op evaluation: verify=%d reconcile=%d noop=%d", immutableCatalogVerification, reconcile, noop)
	}
	manualReconcile := strings.Index(workflow, "manual-alias-reconciliation.json")
	manualCandidate := strings.Index(workflow, "'.entries[] | select(.packageIndexDigest == $digest and .status == \"candidate\")'")
	if manualReconcile < 0 || manualCandidate < 0 || manualReconcile >= manualCandidate {
		t.Fatalf("manual alias reconciliation must run before candidate promotion checks: reconcile=%d candidate=%d", manualReconcile, manualCandidate)
	}
}

func TestWorkflowForceCandidatePreservesVerifiedEvidence(t *testing.T) {
	workflow := readPublisherWorkflow(t)
	verificationGate := `if [[ "$ALLOW_ALIAS" != true || "$SOURCE_RECHECKED" != true || "$runtime_validated" != true || "$import_readback" != true ]]; then`
	if !strings.Contains(workflow, verificationGate) {
		t.Fatal("attestation verification gate unexpectedly changed")
	}
	candidateGate := `if [[ "$FORCE_CANDIDATE" == true || "$ALLOW_ALIAS" != true || "$SOURCE_RECHECKED" != true || "$runtime_validated" != true || "$import_readback" != true ]]; then`
	if !strings.Contains(workflow, candidateGate) {
		t.Fatal("force_candidate no longer suppresses automatic alias advancement")
	}
}

func readPublisherWorkflow(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "docker-sandboxes-images.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
