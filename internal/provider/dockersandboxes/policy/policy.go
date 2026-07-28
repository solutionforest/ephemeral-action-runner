// Package policy canonicalizes and verifies the complete effective
// Docker Sandboxes policy before a runner registration token is requested.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

type canonicalRule struct {
	ID           string                         `json:"id"`
	Name         string                         `json:"name"`
	PolicyID     string                         `json:"policyId"`
	Scope        string                         `json:"scope"`
	AppliesTo    string                         `json:"appliesTo"`
	ResourceType string                         `json:"resourceType"`
	Resources    []string                       `json:"resources"`
	Decision     provider.NetworkPolicyDecision `json:"decision"`
	Origin       string                         `json:"origin"`
	Status       string                         `json:"status"`
	Editable     bool                           `json:"editable"`
	Active       bool                           `json:"active"`
}

// Fingerprint hashes all supplied rules and attribution fields in a stable
// order. It intentionally includes automatic and non-editable rules.
func Fingerprint(rules []provider.NetworkPolicyRule) (string, error) {
	canonical := make([]canonicalRule, len(rules))
	for index, rule := range rules {
		if err := validateAttributedRule(rule); err != nil {
			return "", err
		}
		resources := append([]string(nil), rule.Resources...)
		sort.Strings(resources)
		canonical[index] = canonicalRule{
			ID:           rule.ID,
			Name:         rule.Name,
			PolicyID:     rule.PolicyID,
			Scope:        rule.Scope,
			AppliesTo:    rule.AppliesTo,
			ResourceType: rule.ResourceType,
			Resources:    resources,
			Decision:     rule.Decision,
			Origin:       rule.Origin,
			Status:       rule.Status,
			Editable:     rule.Editable,
			Active:       rule.Active,
		}
	}
	sort.Slice(canonical, func(left, right int) bool {
		leftJSON, _ := json.Marshal(canonical[left])
		rightJSON, _ := json.Marshal(canonical[right])
		return string(leftJSON) < string(rightJSON)
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical Docker Sandboxes policy: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// VerifyBaseline requires a complete active global baseline plus only the
// exact built-in rule that Docker Sandboxes v0.35.0 may attach to a shell
// sandbox. The global content fingerprint remains stable across sandbox names
// and provider-generated rule IDs; the built-in rule is verified structurally.
func VerifyBaseline(expectedFingerprint, sandboxName string, rules []provider.NetworkPolicyRule) error {
	global, scoped, err := partition(sandboxName, rules)
	if err != nil {
		return err
	}
	if err := verifyBalancedGlobal(global); err != nil {
		return err
	}
	if err := verifyExpectedBuiltins(sandboxName, scoped); err != nil {
		return err
	}
	actual, err := Fingerprint(global)
	if err != nil {
		return err
	}
	if actual != expectedFingerprint {
		return fmt.Errorf("Docker Sandboxes balanced policy fingerprint mismatch: got %s, want %s", actual, expectedFingerprint)
	}
	return nil
}

// VerifyEffective proves that the global baseline is unchanged and that every
// exact sandbox-scoped policy resource is active, scoped, editable, network-only,
// and represented by the configured allow/deny sets with no extras.
func VerifyEffective(expectedBaseline, sandboxName string, rules []provider.NetworkPolicyRule, allow, deny []string) error {
	global, scoped, err := partition(sandboxName, rules)
	if err != nil {
		return err
	}
	if err := verifyBalancedGlobal(global); err != nil {
		return err
	}
	actualBaseline, err := Fingerprint(global)
	if err != nil {
		return err
	}
	if actualBaseline != expectedBaseline {
		return fmt.Errorf("Docker Sandboxes global policy changed after scoped policy application: got %s, want %s", actualBaseline, expectedBaseline)
	}
	want := make(map[string]provider.NetworkPolicyDecision, len(allow)+len(deny))
	for _, resource := range allow {
		want[resource] = provider.NetworkPolicyAllow
	}
	for _, resource := range deny {
		want[resource] = provider.NetworkPolicyDeny
	}
	seen := make(map[string]provider.NetworkPolicyDecision, len(want))
	builtinCount := 0
	for _, rule := range scoped {
		if IsExpectedBuiltinRule(sandboxName, rule) {
			builtinCount++
			continue
		}
		if rule.ResourceType != "network" || !strings.EqualFold(rule.Origin, "scoped") || !rule.Editable {
			return fmt.Errorf("Docker Sandboxes policy contained an unmanaged sandbox-scoped rule %q", rule.ID)
		}
		for _, resource := range rule.Resources {
			expectedDecision, exists := want[resource]
			if !exists || expectedDecision != rule.Decision {
				return fmt.Errorf("Docker Sandboxes policy contained unexpected sandbox-scoped resource %q", resource)
			}
			if previous, duplicate := seen[resource]; duplicate && previous != rule.Decision {
				return fmt.Errorf("Docker Sandboxes policy contained conflicting decisions for %q", resource)
			}
			seen[resource] = rule.Decision
		}
	}
	if builtinCount > 1 {
		return fmt.Errorf("Docker Sandboxes policy contained %d duplicate built-in shell rules", builtinCount)
	}
	for resource, decision := range want {
		if actual, exists := seen[resource]; !exists || actual != decision {
			return fmt.Errorf("Docker Sandboxes policy did not activate configured %s resource %q", decision, resource)
		}
	}
	return nil
}

func verifyBalancedGlobal(rules []provider.NetworkPolicyRule) error {
	networkAllowFound := false
	for _, rule := range rules {
		if rule.ResourceType != "network" {
			continue
		}
		for _, resource := range rule.Resources {
			host := resource
			if separator := strings.LastIndex(resource, ":"); separator >= 0 {
				host = resource[:separator]
			}
			if host == "*" || host == "**" {
				if rule.Decision == provider.NetworkPolicyDeny {
					return fmt.Errorf("Docker Sandboxes global policy is locked down rather than balanced")
				}
				if rule.Decision == provider.NetworkPolicyAllow {
					return fmt.Errorf("Docker Sandboxes global policy is open rather than balanced")
				}
			}
			if rule.Decision == provider.NetworkPolicyAllow {
				networkAllowFound = true
			}
		}
	}
	if !networkAllowFound {
		return fmt.Errorf("Docker Sandboxes global policy is locked down rather than balanced")
	}
	return nil
}

func partition(sandboxName string, rules []provider.NetworkPolicyRule) (global, scoped []provider.NetworkPolicyRule, err error) {
	if strings.TrimSpace(sandboxName) == "" {
		return nil, nil, fmt.Errorf("Docker Sandboxes policy sandbox name is required")
	}
	seenIDs := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if err := validateAttributedRule(rule); err != nil {
			return nil, nil, err
		}
		if _, duplicate := seenIDs[rule.ID]; duplicate {
			return nil, nil, fmt.Errorf("Docker Sandboxes policy contained duplicate rule id %q", rule.ID)
		}
		seenIDs[rule.ID] = struct{}{}
		if !rule.Active || !strings.EqualFold(rule.Status, "active") {
			return nil, nil, fmt.Errorf("Docker Sandboxes policy rule %q is inactive", rule.ID)
		}
		switch {
		case rule.Scope == "global" && rule.AppliesTo == "all":
			global = append(global, rule)
		case isExactSandboxScope(rule, sandboxName):
			scoped = append(scoped, rule)
		default:
			return nil, nil, fmt.Errorf("Docker Sandboxes policy contained rule %q with unexpected scope %q and target %q", rule.ID, rule.Scope, rule.AppliesTo)
		}
	}
	if len(global) == 0 {
		return nil, nil, fmt.Errorf("Docker Sandboxes policy omitted its global baseline")
	}
	return global, scoped, nil
}

func validateAttributedRule(rule provider.NetworkPolicyRule) error {
	for key, value := range map[string]string{
		"id": rule.ID, "name": rule.Name, "policy id": rule.PolicyID, "scope": rule.Scope, "target": rule.AppliesTo, "resource type": rule.ResourceType, "origin": rule.Origin, "status": rule.Status,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Docker Sandboxes policy rule omitted %s", key)
		}
	}
	if rule.Decision != provider.NetworkPolicyAllow && rule.Decision != provider.NetworkPolicyDeny {
		return fmt.Errorf("Docker Sandboxes policy rule %q used unsupported decision %q", rule.ID, rule.Decision)
	}
	if len(rule.Resources) == 0 {
		return fmt.Errorf("Docker Sandboxes policy rule %q omitted resources", rule.ID)
	}
	for _, resource := range rule.Resources {
		if strings.TrimSpace(resource) == "" {
			return fmt.Errorf("Docker Sandboxes policy rule %q contained an empty resource", rule.ID)
		}
	}
	return nil
}

func isExactSandboxScope(rule provider.NetworkPolicyRule, sandboxName string) bool {
	target := "sandbox:" + sandboxName
	return rule.Scope == target && rule.AppliesTo == target
}

// IsExpectedBuiltinRule identifies the exact non-editable shell-kit egress
// rule observed in Docker Sandboxes v0.35.0. It deliberately checks every
// stable semantic field while allowing provider-generated IDs to vary.
func IsExpectedBuiltinRule(sandboxName string, rule provider.NetworkPolicyRule) bool {
	return isExactSandboxScope(rule, sandboxName) &&
		rule.Name == "kit:"+sandboxName &&
		rule.ResourceType == "network" &&
		rule.Decision == provider.NetworkPolicyAllow &&
		len(rule.Resources) == 1 && rule.Resources[0] == "openrouter.ai" &&
		strings.EqualFold(rule.Origin, "scoped") &&
		strings.EqualFold(rule.Status, "active") && rule.Active && !rule.Editable
}

func verifyExpectedBuiltins(sandboxName string, rules []provider.NetworkPolicyRule) error {
	if len(rules) > 1 {
		return fmt.Errorf("Docker Sandboxes policy contained %d unexpected pre-existing sandbox-scoped rules", len(rules))
	}
	for _, rule := range rules {
		if !IsExpectedBuiltinRule(sandboxName, rule) {
			return fmt.Errorf("Docker Sandboxes policy contained unexpected pre-existing sandbox-scoped rule %q", rule.ID)
		}
	}
	return nil
}
