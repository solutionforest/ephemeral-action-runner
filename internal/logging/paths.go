package logging

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ManagerPath returns the active manager log path.
func ManagerPath(root string) string { return filepath.Join(root, ManagerFilename) }

// LastErrorPath returns the stable latest-error report path.
func LastErrorPath(root string) string { return filepath.Join(root, LastErrorFilename) }

// CategoryDirectory returns a supported shallow category directory.
func CategoryDirectory(root string, category Category) (string, error) {
	if !category.validTranscriptCategory() {
		return "", fmt.Errorf("unsupported transcript category %q", category)
	}
	return filepath.Join(root, string(category)), nil
}

// InstancePath returns instances/<instance>.<component>.log. Component is a
// provider name or "guest".
func InstancePath(root, instance, component string) (string, error) {
	if err := validateComponent("instance", instance); err != nil {
		return "", err
	}
	if err := validateComponent("instance component", component); err != nil {
		return "", err
	}
	if !validInstanceComponent(component) {
		return "", fmt.Errorf("unsupported instance component %q", component)
	}
	directory, _ := CategoryDirectory(root, CategoryInstances)
	return filepath.Join(directory, instance+"."+component+".log"), nil
}

// BuildPath returns builds/<image-stem>.<component>.log. Component must be a
// recognized build transcript suffix such as "docker-build" or "refresh".
func BuildPath(root, imageStem, component string) (string, error) {
	if err := validateComponent("image stem", imageStem); err != nil {
		return "", err
	}
	if !validBuildComponent(component) {
		return "", fmt.Errorf("unsupported build component %q", component)
	}
	directory, _ := CategoryDirectory(root, CategoryBuilds)
	return filepath.Join(directory, imageStem+"."+component+".log"), nil
}

// ErrorPath returns errors/epar-YYYYMMDD-HHMMSS-error.log.
func ErrorPath(root string, timestamp time.Time) string {
	directory, _ := CategoryDirectory(root, CategoryErrors)
	return filepath.Join(directory, "epar-"+timestamp.UTC().Format("20060102-150405")+"-error.log")
}

// BenchmarkPath returns benchmarks/<UTC-nanos>-<provider>.jsonl.
func BenchmarkPath(root string, timestamp time.Time, provider string) (string, error) {
	if err := validateComponent("benchmark provider", provider); err != nil {
		return "", err
	}
	directory, _ := CategoryDirectory(root, CategoryBenchmarks)
	return filepath.Join(directory, timestamp.UTC().Format("20060102T150405.000000000Z")+"-"+provider+".jsonl"), nil
}

func validateComponent(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\\`) || !safeComponentPattern.MatchString(value) {
		return fmt.Errorf("%s %q is not a safe path component", label, value)
	}
	return nil
}

func validBuildComponent(component string) bool {
	switch component {
	case "docker-build", "wsl-build", "build", "source", "refresh", "wsl-refresh", "guest":
		return true
	default:
		return false
	}
}

func validInstanceComponent(component string) bool {
	switch component {
	case "guest", "docker-dind", "wsl", "tart":
		return true
	default:
		return false
	}
}

func canonicalPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	return canonicalCase(absolute), nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
