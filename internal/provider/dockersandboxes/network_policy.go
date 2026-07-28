package dockersandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func (p *Provider) ApplyNetworkPolicy(ctx context.Context, instance provider.Instance, rules []provider.NetworkPolicyRule) error {
	if len(rules) == 0 {
		return nil
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("docker sandbox is missing")
	}
	for _, rule := range rules {
		if err := validateNetworkRule(rule, false); err != nil {
			return err
		}
		args := []string{"policy", string(rule.Decision), "network", "--sandbox", instance.Name, strings.Join(rule.Resources, ",")}
		result, runErr := p.run(ctx, commandRequest{args: args, operation: "apply docker sandbox network policy"})
		if runErr != nil && !strings.Contains(strings.ToLower(result.Stdout+"\n"+result.Stderr), "already covered") {
			return runErr
		}
	}
	actual, err := p.readNetworkPolicyVerified(ctx, instance)
	if err != nil {
		return err
	}
	for _, expected := range rules {
		if !containsSandboxPolicyRule(actual, expected, instance.Name) {
			return fmt.Errorf("docker sandbox network policy readback did not contain an applied rule")
		}
	}
	return nil
}

func (p *Provider) ReadNetworkPolicy(ctx context.Context, instance provider.Instance) ([]provider.NetworkPolicyRule, error) {
	present, err := p.assertIdentity(ctx, instance)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("docker sandbox is missing")
	}
	return p.readNetworkPolicyVerified(ctx, instance)
}

// ReadGlobalNetworkPolicy returns only the provider-wide policy baseline. It
// never changes policy and preserves the attribution supplied by Docker
// Sandboxes for every returned rule.
func (p *Provider) ReadGlobalNetworkPolicy(ctx context.Context) ([]provider.NetworkPolicyRule, error) {
	if err := p.ensureVersion(ctx); err != nil {
		return nil, err
	}
	result, err := p.run(ctx, commandRequest{
		args:        []string{"policy", "ls", "--include-inactive", "--json"},
		operation:   "read docker sandboxes global network policy",
		outputLimit: diagnosticOutputLimit,
	})
	if err != nil {
		return nil, err
	}
	return parseGlobalNetworkPolicy([]byte(result.Stdout))
}

func (p *Provider) readNetworkPolicyVerified(ctx context.Context, instance provider.Instance) ([]provider.NetworkPolicyRule, error) {
	result, err := p.run(ctx, commandRequest{
		args:      []string{"policy", "ls", instance.Name, "--include-inactive", "--json"},
		operation: "read docker sandbox network policy",
	})
	if err != nil {
		return nil, err
	}
	return parseNetworkPolicy([]byte(result.Stdout), instance.Name)
}

func (p *Provider) RemoveNetworkPolicy(ctx context.Context, instance provider.Instance, rules []provider.NetworkPolicyRule) error {
	if len(rules) == 0 {
		return nil
	}
	present, err := p.assertIdentity(ctx, instance)
	if err != nil || !present {
		return err
	}
	actual, err := p.readNetworkPolicyVerified(ctx, instance)
	if err != nil {
		return err
	}
	seenIDs := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if err := validateNetworkRule(rule, true); err != nil {
			return err
		}
		if _, duplicate := seenIDs[rule.ID]; duplicate {
			continue
		}
		seenIDs[rule.ID] = struct{}{}
		matched, found := findPolicyRuleID(actual, rule.ID)
		if !found {
			continue
		}
		if !isRemovableSandboxPolicyRule(matched, instance.Name) {
			return fmt.Errorf("refusing to remove a network policy rule that is not an editable local rule scoped to this sandbox")
		}
		if !sameStablePolicyRuleIdentity(matched, rule) {
			return fmt.Errorf("refusing to remove a network policy rule whose stable identity changed")
		}
		result, runErr := p.run(ctx, commandRequest{
			args:      []string{"policy", "rm", "network", "--sandbox", instance.Name, "--id", rule.ID},
			operation: "remove docker sandbox network policy",
		})
		if runErr != nil && !isMissingPolicyRule(result.Stdout+"\n"+result.Stderr+"\n"+runErr.Error()) {
			return runErr
		}
	}
	remaining, err := p.readNetworkPolicyVerified(ctx, instance)
	if err != nil {
		return err
	}
	for _, removed := range rules {
		if containsPolicyRuleID(remaining, removed.ID) {
			return fmt.Errorf("docker sandbox network policy rule remained after exact removal")
		}
	}
	return nil
}

func parseGlobalNetworkPolicy(data []byte) ([]provider.NetworkPolicyRule, error) {
	rules, err := parseNetworkPolicy(data, "")
	if err != nil {
		return nil, err
	}
	global := make([]provider.NetworkPolicyRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Scope == "global" && rule.AppliesTo == "all" {
			global = append(global, rule)
		}
	}
	return global, nil
}

func parseNetworkPolicy(data []byte, sandboxName string) ([]provider.NetworkPolicyRule, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || requireJSONEOF(decoder) != nil {
		return nil, fmt.Errorf("docker sandbox network policy returned an unsupported json schema")
	}
	var records []map[string]json.RawMessage
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("docker sandbox network policy returned an unsupported json schema")
	}
	rulesJSON, ok := wrapper["rules"]
	if !ok || bytes.Equal(bytes.TrimSpace(rulesJSON), []byte("null")) || json.Unmarshal(rulesJSON, &records) != nil {
		return nil, fmt.Errorf("docker sandbox network policy returned an unsupported json schema")
	}

	out := make([]provider.NetworkPolicyRule, 0, len(records))
	seenIDs := make(map[string]struct{}, len(records))
	for _, record := range records {
		ruleType, err := requiredJSONString(record, "resource_type")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its type")
		}
		id, err := requiredJSONString(record, "id")
		if err != nil || !providerIDPattern.MatchString(id) {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted a valid id")
		}
		decisionText, err := requiredJSONString(record, "decision")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its decision")
		}
		decision := provider.NetworkPolicyDecision(strings.ToLower(decisionText))
		if decision != provider.NetworkPolicyAllow && decision != provider.NetworkPolicyDeny {
			return nil, fmt.Errorf("docker sandbox network policy rule used an unknown decision")
		}
		name, err := requiredJSONString(record, "name")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its name")
		}
		policyID, err := requiredJSONString(record, "policy_id")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its policy id")
		}
		scope, err := requiredJSONString(record, "scope")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its scope")
		}
		appliesTo, err := requiredJSONString(record, "applies_to")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its target")
		}
		resources, err := policyRuleResources(record)
		if err != nil {
			return nil, err
		}
		status, active, err := policyRuleStatus(record)
		if err != nil {
			return nil, err
		}
		origin, err := requiredJSONString(record, "origin")
		if err != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted its origin")
		}
		var editable bool
		if rawEditable, ok := record["editable"]; !ok || json.Unmarshal(rawEditable, &editable) != nil {
			return nil, fmt.Errorf("docker sandbox network policy rule omitted editable state")
		}
		if ruleType == "network" {
			for _, resource := range resources {
				if err := validateReadbackPolicyResource(resource); err != nil {
					return nil, err
				}
			}
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("docker sandbox network policy returned a duplicate rule id")
		}
		seenIDs[id] = struct{}{}
		if scope != "global" {
			sandboxID, sandboxIDErr := requiredJSONString(record, "sandbox_id")
			if sandboxIDErr != nil || scope != "sandbox:"+sandboxID || appliesTo != scope {
				return nil, fmt.Errorf("docker sandbox policy returned an unsupported sandbox attribution")
			}
		}
		relevant, targetErr := isRelevantPolicyTarget(scope, appliesTo, sandboxName)
		if targetErr != nil {
			return nil, targetErr
		}
		if !relevant {
			continue
		}
		out = append(out, provider.NetworkPolicyRule{
			ID:           id,
			Name:         name,
			PolicyID:     policyID,
			Scope:        scope,
			AppliesTo:    appliesTo,
			ResourceType: ruleType,
			Resources:    append([]string(nil), resources...),
			Decision:     decision,
			Origin:       origin,
			Status:       status,
			Editable:     editable,
			Active:       active,
		})
	}
	return out, nil
}

func validateReadbackPolicyResource(resource string) error {
	if resource == "" || resource != strings.TrimSpace(resource) || len(resource) > 2048 || strings.ContainsAny(resource, "\x00\r\n\t") {
		return fmt.Errorf("docker sandbox network policy returned an invalid resource")
	}
	return nil
}

func policyRuleResources(record map[string]json.RawMessage) ([]string, error) {
	raw := record["resources"]
	if len(raw) == 0 {
		return nil, fmt.Errorf("docker sandbox network policy rule omitted its resource")
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return nil, fmt.Errorf("docker sandbox network policy rule returned an invalid resource")
	}
	return many, nil
}

func isSandboxPolicyTarget(scope, appliesTo, sandboxName string) bool {
	target := "sandbox:" + sandboxName
	return scope == target && appliesTo == target
}

func isRelevantPolicyTarget(scope, appliesTo, sandboxName string) (bool, error) {
	switch scope {
	case "global":
		if appliesTo != "all" {
			return false, fmt.Errorf("docker sandbox policy returned an unsupported global target")
		}
		return true, nil
	default:
		if !strings.HasPrefix(scope, "sandbox:") || appliesTo != scope {
			return false, fmt.Errorf("docker sandbox policy returned an unsupported sandbox target")
		}
		targetName := strings.TrimPrefix(scope, "sandbox:")
		if !sandboxNamePattern.MatchString(targetName) {
			return false, fmt.Errorf("docker sandbox policy returned an unsupported sandbox target")
		}
		return targetName == sandboxName, nil
	}
}

func policyRuleStatus(record map[string]json.RawMessage) (string, bool, error) {
	status, err := requiredJSONString(record, "status")
	if err != nil {
		return "", false, fmt.Errorf("docker sandbox network policy rule omitted status")
	}
	switch strings.ToLower(status) {
	case "active":
		return status, true, nil
	case "inactive":
		return status, false, nil
	default:
		return "", false, fmt.Errorf("docker sandbox network policy rule returned unknown status")
	}
}

func containsSandboxPolicyRule(rules []provider.NetworkPolicyRule, expected provider.NetworkPolicyRule, sandboxName string) bool {
	for _, expectedResource := range expected.Resources {
		found := false
		for _, rule := range rules {
			if rule.Decision != expected.Decision || !isSandboxPolicyTarget(rule.Scope, rule.AppliesTo, sandboxName) {
				continue
			}
			for _, actualResource := range rule.Resources {
				if actualResource == expectedResource {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsPolicyRuleID(rules []provider.NetworkPolicyRule, id string) bool {
	_, found := findPolicyRuleID(rules, id)
	return found
}

func findPolicyRuleID(rules []provider.NetworkPolicyRule, id string) (provider.NetworkPolicyRule, bool) {
	for _, rule := range rules {
		if rule.ID == id {
			return rule, true
		}
	}
	return provider.NetworkPolicyRule{}, false
}

func isRemovableSandboxPolicyRule(rule provider.NetworkPolicyRule, sandboxName string) bool {
	return rule.ResourceType == "network" && isSandboxPolicyTarget(rule.Scope, rule.AppliesTo, sandboxName) && strings.EqualFold(rule.Origin, "scoped") && rule.Editable
}

func sameStablePolicyRuleIdentity(actual, expected provider.NetworkPolicyRule) bool {
	if actual.ID != expected.ID || actual.PolicyID != expected.PolicyID || actual.Scope != expected.Scope || actual.AppliesTo != expected.AppliesTo || actual.ResourceType != expected.ResourceType || actual.Decision != expected.Decision || !strings.EqualFold(actual.Origin, expected.Origin) || actual.Editable != expected.Editable {
		return false
	}
	actualResources := append([]string(nil), actual.Resources...)
	expectedResources := append([]string(nil), expected.Resources...)
	sort.Strings(actualResources)
	sort.Strings(expectedResources)
	return slices.Equal(actualResources, expectedResources)
}

func isMissingPolicyRule(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "rule not found") || strings.Contains(text, "no matching rule") || strings.Contains(text, "status 404")
}
