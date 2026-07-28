package policy

import (
	"testing"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func TestFingerprintIsOrderIndependentAndIncludesAttribution(t *testing.T) {
	rules := baselineRules()
	first, err := Fingerprint(rules)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []provider.NetworkPolicyRule{rules[1], rules[0]}
	second, err := Fingerprint(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("fingerprint changed with rule order: %s != %s", first, second)
	}
	changed := append([]provider.NetworkPolicyRule(nil), rules...)
	changed[0].Origin = "organization"
	third, err := Fingerprint(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("fingerprint ignored policy attribution")
	}
}

func TestVerifyBaselineRejectsUnexpectedOrInactiveRules(t *testing.T) {
	rules := baselineRules()
	fingerprint, err := Fingerprint(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBaseline(fingerprint, "epar-1", rules); err != nil {
		t.Fatalf("valid baseline rejected: %v", err)
	}
	withBuiltin := append(append([]provider.NetworkPolicyRule(nil), rules...), builtinRule("epar-1"))
	if err := VerifyBaseline(fingerprint, "epar-1", withBuiltin); err != nil {
		t.Fatalf("exact v0.35.0 shell-kit baseline rejected: %v", err)
	}
	unexpected := append(append([]provider.NetworkPolicyRule(nil), rules...), scopedRule("epar-1", "rule-1", provider.NetworkPolicyAllow, "example.test"))
	if err := VerifyBaseline(fingerprint, "epar-1", unexpected); err == nil {
		t.Fatal("pre-existing scoped rule accepted")
	}
	inactive := append([]provider.NetworkPolicyRule(nil), rules...)
	inactive[0].Active = false
	inactive[0].Status = "inactive"
	if err := VerifyBaseline(fingerprint, "epar-1", inactive); err == nil {
		t.Fatal("inactive baseline rule accepted")
	}
}

func TestVerifyBaselineRejectsOpenAndLockedDownGlobalModes(t *testing.T) {
	open := baselineRules()
	open[0].Resources = append(open[0].Resources, "**")
	openFingerprint, err := Fingerprint(open)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBaseline(openFingerprint, "epar-1", open); err == nil {
		t.Fatal("open global policy accepted as balanced")
	}

	lockedDown := baselineRules()[1:]
	lockedDownFingerprint, err := Fingerprint(lockedDown)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBaseline(lockedDownFingerprint, "epar-1", lockedDown); err == nil {
		t.Fatal("locked-down global policy accepted as balanced")
	}

	denyAll := baselineRules()
	denyAll = append(denyAll, provider.NetworkPolicyRule{ID: "deny-all", Name: "deny-all", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "network", Resources: []string{"**"}, Decision: provider.NetworkPolicyDeny, Origin: "local", Status: "active", Editable: true, Active: true})
	denyAllFingerprint, err := Fingerprint(denyAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBaseline(denyAllFingerprint, "epar-1", denyAll); err == nil {
		t.Fatal("bounded allow plus universal deny accepted as balanced")
	}
}

func TestVerifyEffectiveRequiresExactScopedResources(t *testing.T) {
	global := baselineRules()
	fingerprint, err := Fingerprint(global)
	if err != nil {
		t.Fatal(err)
	}
	effective := append(append([]provider.NetworkPolicyRule(nil), global...),
		builtinRule("epar-1"),
		scopedRule("epar-1", "allow-1", provider.NetworkPolicyAllow, "api.example.test"),
		scopedRule("epar-1", "deny-1", provider.NetworkPolicyDeny, "telemetry.example.test"),
	)
	if err := VerifyEffective(fingerprint, "epar-1", effective, []string{"api.example.test"}, []string{"telemetry.example.test"}); err != nil {
		t.Fatalf("valid effective policy rejected: %v", err)
	}
	extra := append(append([]provider.NetworkPolicyRule(nil), effective...), scopedRule("epar-1", "extra", provider.NetworkPolicyAllow, "unexpected.example.test"))
	if err := VerifyEffective(fingerprint, "epar-1", extra, []string{"api.example.test"}, []string{"telemetry.example.test"}); err == nil {
		t.Fatal("unexpected scoped rule accepted")
	}
	if err := VerifyEffective(fingerprint, "epar-1", effective[:len(effective)-1], []string{"api.example.test"}, []string{"telemetry.example.test"}); err == nil {
		t.Fatal("missing configured rule accepted")
	}
}

func baselineRules() []provider.NetworkPolicyRule {
	return []provider.NetworkPolicyRule{
		{ID: "network", Name: "network", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "network", Resources: []string{"github.com:443", "**.github.com:443"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Status: "active", Editable: true, Active: true},
		{ID: "filesystem", Name: "filesystem", PolicyID: "local-policy", Scope: "global", AppliesTo: "all", ResourceType: "filesystem:read", Resources: []string{"**"}, Decision: provider.NetworkPolicyAllow, Origin: "local", Status: "active", Editable: false, Active: true},
	}
}

func scopedRule(sandboxName, id string, decision provider.NetworkPolicyDecision, resource string) provider.NetworkPolicyRule {
	target := "sandbox:" + sandboxName
	return provider.NetworkPolicyRule{ID: id, Name: id, PolicyID: "local-policy", Scope: target, AppliesTo: target, ResourceType: "network", Resources: []string{resource}, Decision: decision, Origin: "scoped", Status: "active", Editable: true, Active: true}
}

func builtinRule(sandboxName string) provider.NetworkPolicyRule {
	target := "sandbox:" + sandboxName
	return provider.NetworkPolicyRule{ID: "kit-rule", Name: "kit:" + sandboxName, PolicyID: "kit-policy", Scope: target, AppliesTo: target, ResourceType: "network", Resources: []string{"openrouter.ai"}, Decision: provider.NetworkPolicyAllow, Origin: "scoped", Status: "active", Editable: false, Active: true}
}
