package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const hashPrefix = "sha256:"

// ComputePlanHash returns the deterministic SHA-256 identity of a plan with its
// Hash field cleared.
func ComputePlanHash(plan Plan) (string, error) {
	plan.Hash = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode storage plan: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hashPrefix + hex.EncodeToString(sum[:]), nil
}

// ValidatePlanHash verifies both the embedded hash and an independently
// supplied approval hash.
func ValidatePlanHash(plan Plan, approvedHash string) error {
	if strings.TrimSpace(approvedHash) == "" {
		return errors.New("approved storage plan hash is required")
	}
	actual, err := ComputePlanHash(plan)
	if err != nil {
		return err
	}
	if plan.Hash != actual {
		return fmt.Errorf("storage plan content drifted: embedded hash %q does not match %q", plan.Hash, actual)
	}
	if approvedHash != actual {
		return fmt.Errorf("storage plan approval hash %q does not match %q", approvedHash, actual)
	}
	return nil
}

// ValidatePlanArtifacts verifies that every planned removal still has the exact
// artifact snapshot supplied by the caller. Missing, added, or changed
// non-removal artifacts do not broaden the approved target set.
func ValidatePlanArtifacts(plan Plan, current []Artifact) error {
	byID := make(map[string]Artifact, len(current))
	for _, artifact := range current {
		if _, exists := byID[artifact.ID]; exists {
			return fmt.Errorf("duplicate current storage artifact ID %q", artifact.ID)
		}
		byID[artifact.ID] = artifact
	}
	for _, decision := range plan.Decisions {
		if decision.Action != ActionRemove {
			continue
		}
		actual, exists := byID[decision.Artifact.ID]
		if !exists {
			return fmt.Errorf("planned storage artifact %q is missing", decision.Artifact.ID)
		}
		expectedJSON, err := json.Marshal(decision.Artifact)
		if err != nil {
			return err
		}
		actualJSON, err := json.Marshal(actual)
		if err != nil {
			return err
		}
		if string(expectedJSON) != string(actualJSON) {
			return fmt.Errorf("planned storage artifact %q drifted", decision.Artifact.ID)
		}
	}
	return nil
}
