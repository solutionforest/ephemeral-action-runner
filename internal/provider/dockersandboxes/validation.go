package dockersandboxes

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

var (
	sandboxNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	providerIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sizePattern           = regexp.MustCompile(`^[1-9][0-9]*(?:[kKmMgGtT](?:i?[bB])?)?$`)
	templatePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,511}$`)
	templateDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	profilePattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hostLabelPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

func validateCreateRequest(request provider.CreateRequest) error {
	if !sandboxNamePattern.MatchString(request.Name) {
		return fmt.Errorf("invalid docker sandbox name")
	}
	if request.Source != "" {
		return fmt.Errorf("docker sandboxes does not accept a legacy source image")
	}
	if request.CPUs <= 0 || request.CPUs > 256 {
		return fmt.Errorf("docker sandbox cpu count must be between 1 and 256")
	}
	if !sizePattern.MatchString(request.Memory) {
		return fmt.Errorf("invalid docker sandbox memory limit")
	}
	if !templatePattern.MatchString(request.Template) || strings.HasPrefix(request.Template, "-") {
		return fmt.Errorf("invalid docker sandbox template")
	}
	if _, _, err := splitTemplateReference(request.Template); err != nil {
		return err
	}
	if !templateDigestPattern.MatchString(request.TemplateDigest) {
		return fmt.Errorf("docker sandbox template digest must be a full lowercase sha256 identity")
	}
	if err := validateCanonicalStagingPath(request.StagingPath); err != nil {
		return err
	}
	if request.RootDisk != "" && !sizePattern.MatchString(request.RootDisk) {
		return fmt.Errorf("invalid docker sandbox root disk size")
	}
	if request.DockerDisk != "" && !sizePattern.MatchString(request.DockerDisk) {
		return fmt.Errorf("invalid docker sandbox docker disk size")
	}
	return nil
}

func splitTemplateReference(reference string) (repository, tag string, err error) {
	separator := strings.LastIndex(reference, ":")
	if separator <= strings.LastIndex(reference, "/") || separator == len(reference)-1 || strings.Contains(reference, "@") {
		return "", "", fmt.Errorf("docker sandbox template must be an exact repository:tag reference")
	}
	repository, tag = reference[:separator], reference[separator+1:]
	if repository == "" || tag == "" {
		return "", "", fmt.Errorf("docker sandbox template must be an exact repository:tag reference")
	}
	if !strings.Contains(repository, "/") {
		repository = "docker.io/library/" + repository
	} else {
		first := strings.SplitN(repository, "/", 2)[0]
		if first != "localhost" && !strings.ContainsAny(first, ".:") {
			repository = "docker.io/" + repository
		}
	}
	return repository, tag, nil
}

func validateLocalTemplateReference(reference string) error {
	if !templatePattern.MatchString(reference) || strings.HasPrefix(reference, "-") {
		return fmt.Errorf("docker sandbox template must be an exact repository:tag reference")
	}
	_, tag, err := splitTemplateReference(reference)
	if err != nil || !profilePattern.MatchString(tag) {
		return fmt.Errorf("docker sandbox template must be an exact repository:tag reference")
	}
	return nil
}

func validateCanonicalStagingPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("invalid docker sandbox staging path")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("docker sandbox staging path must be an already-validated canonical absolute path")
	}
	return nil
}

func validateInstance(instance provider.Instance, requireID bool) error {
	if !sandboxNamePattern.MatchString(instance.Name) {
		return fmt.Errorf("invalid docker sandbox identity")
	}
	if requireID && !providerIDPattern.MatchString(instance.ProviderID) {
		return fmt.Errorf("docker sandbox stable provider id is required")
	}
	if instance.ProviderID != "" && !providerIDPattern.MatchString(instance.ProviderID) {
		return fmt.Errorf("invalid docker sandbox provider id")
	}
	return nil
}

func validateGuestCommand(command []string, opts provider.ExecOptions) error {
	if len(command) == 0 {
		return fmt.Errorf("docker sandbox guest command is required")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("docker sandbox guest command contains a null byte")
		}
	}
	if len(opts.Env) != 0 {
		return fmt.Errorf("docker sandbox guest environment passthrough is not permitted")
	}
	if opts.LogPath != "" {
		return fmt.Errorf("docker sandbox host log paths are not permitted")
	}
	return nil
}

func validateNetworkRule(rule provider.NetworkPolicyRule, forRemoval bool) error {
	if forRemoval {
		if !providerIDPattern.MatchString(rule.ID) {
			return fmt.Errorf("docker sandbox network rule id is required for exact removal")
		}
		if !providerIDPattern.MatchString(rule.PolicyID) || rule.Scope == "" || rule.AppliesTo == "" || rule.ResourceType == "" || rule.Origin == "" {
			return fmt.Errorf("docker sandbox network rule requires its complete stable identity for exact removal")
		}
		if rule.Decision != provider.NetworkPolicyAllow && rule.Decision != provider.NetworkPolicyDeny {
			return fmt.Errorf("invalid docker sandbox network decision")
		}
		if len(rule.Resources) == 0 {
			return fmt.Errorf("docker sandbox network policy requires at least one resource")
		}
		seen := make(map[string]struct{}, len(rule.Resources))
		for _, resource := range rule.Resources {
			if strings.TrimSpace(resource) == "" {
				return fmt.Errorf("docker sandbox network policy contains an empty resource")
			}
			if _, duplicate := seen[resource]; duplicate {
				return fmt.Errorf("docker sandbox network policy contains a duplicate resource")
			}
			seen[resource] = struct{}{}
		}
		return nil
	}
	if !forRemoval && rule.ID != "" {
		return fmt.Errorf("docker sandbox network rule id must be empty when applying")
	}
	if !forRemoval && rule.ResourceType != "" && rule.ResourceType != "network" {
		return fmt.Errorf("docker sandbox policy mutation supports only network rules")
	}
	if rule.Decision != provider.NetworkPolicyAllow && rule.Decision != provider.NetworkPolicyDeny {
		return fmt.Errorf("invalid docker sandbox network decision")
	}
	if len(rule.Resources) == 0 {
		return fmt.Errorf("docker sandbox network policy requires at least one resource")
	}
	seen := make(map[string]struct{}, len(rule.Resources))
	for _, resource := range rule.Resources {
		if resource != "**" || rule.Decision != provider.NetworkPolicyAllow {
			if err := validateNetworkResource(resource); err != nil {
				return err
			}
		}
		if _, duplicate := seen[resource]; duplicate {
			return fmt.Errorf("docker sandbox network policy contains a duplicate resource")
		}
		seen[resource] = struct{}{}
	}
	return nil
}

func validateNetworkResource(resource string) error {
	if resource == "" || len(resource) > 512 || strings.ContainsAny(resource, "\x00\r\n\t ,\\/@?#[]") || strings.HasPrefix(resource, "-") {
		return fmt.Errorf("invalid docker sandbox network resource")
	}
	host := resource
	if candidateHost, port, ok := strings.Cut(host, ":"); ok {
		if strings.Contains(port, ":") || !validPort(port) {
			return fmt.Errorf("invalid docker sandbox network resource")
		}
		host = candidateHost
	}
	wildcard := strings.HasPrefix(host, "*.")
	if wildcard {
		host = strings.TrimPrefix(host, "*.")
	}
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil || wildcard && !strings.Contains(host, ".") {
		return fmt.Errorf("invalid docker sandbox network resource")
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabelPattern.MatchString(label) {
			return fmt.Errorf("invalid docker sandbox network resource")
		}
	}
	return nil
}

func validPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port >= 1 && port <= 65535
}
