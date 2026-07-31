// Package staging manages the only host path exposed to a Docker Sandbox.
//
// A staging directory is always a fresh, empty, direct child of a dedicated
// root. Disposal first binds the exact filesystem object, atomically moves it
// to a deterministic quarantine child, and only then removes that private tree.
package staging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Staging struct {
	root string
}

type OwnedDirectory struct {
	Path     string
	Identity string
}

func Open(root string) (*Staging, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Docker Sandboxes staging root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker Sandboxes staging root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	if err := rejectAlternateDataStream(absRoot); err != nil {
		return nil, err
	}
	_, statErr := os.Lstat(absRoot)
	rootExisted := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect Docker Sandboxes staging root: %w", statErr)
	}
	if err := createPathWithoutRedirect(absRoot); err != nil {
		return nil, fmt.Errorf("create Docker Sandboxes staging root: %w", err)
	}
	if !rootExisted {
		if err := restrictPlatformPermissions(absRoot); err != nil {
			return nil, fmt.Errorf("restrict Docker Sandboxes staging root: %w", err)
		}
	}
	if err := validateDirectory(absRoot, false); err != nil {
		return nil, fmt.Errorf("validate Docker Sandboxes staging root: %w", err)
	}
	canonicalRoot, err := platformCanonicalPathSpelling(absRoot)
	if err != nil {
		return nil, fmt.Errorf("normalize Docker Sandboxes staging root: %w", err)
	}
	return &Staging{root: filepath.Clean(canonicalRoot)}, nil
}

func (s *Staging) Root() string {
	return s.root
}

// CreateOwned creates a fresh direct child and captures the stable filesystem
// object identity used to distinguish it from a later same-path replacement.
func (s *Staging) CreateOwned(name string) (OwnedDirectory, error) {
	if err := validateName(name); err != nil {
		return OwnedDirectory{}, err
	}
	path, err := s.exactPath(name)
	if err != nil {
		return OwnedDirectory{}, err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		if os.IsExist(err) {
			return OwnedDirectory{}, fmt.Errorf("Docker Sandboxes staging directory %q already exists", path)
		}
		return OwnedDirectory{}, fmt.Errorf("create Docker Sandboxes staging directory %q: %w", path, err)
	}
	if err := restrictPlatformPermissions(path); err != nil {
		_ = os.Remove(path)
		return OwnedDirectory{}, fmt.Errorf("restrict Docker Sandboxes staging directory %q: %w", path, err)
	}
	if err := validateDirectory(path, true); err != nil {
		_ = os.Remove(path)
		return OwnedDirectory{}, err
	}
	identity, err := platformDirectoryIdentity(path)
	if err != nil {
		_ = os.Remove(path)
		return OwnedDirectory{}, fmt.Errorf("read Docker Sandboxes staging directory identity %q: %w", path, err)
	}
	return OwnedDirectory{Path: path, Identity: identity}, nil
}

func (s *Staging) verifyEmpty(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	path, err := s.exactPath(name)
	if err != nil {
		return "", err
	}
	if err := validateDirectory(path, true); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Staging) VerifyOwnedEmpty(name, identity string) (string, error) {
	return s.verifyOwned(name, identity, true)
}

func (s *Staging) VerifyOwned(name, identity string) (string, error) {
	return s.verifyOwned(name, identity, false)
}

func (s *Staging) verifyOwned(name, identity string, requireEmpty bool) (string, error) {
	if strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("Docker Sandboxes staging directory identity is required")
	}
	if err := validateName(name); err != nil {
		return "", err
	}
	path, err := s.exactPath(name)
	if err != nil {
		return "", err
	}
	if err := validateDirectory(path, requireEmpty); err != nil {
		return "", err
	}
	actual, err := platformDirectoryIdentity(path)
	if err != nil {
		return "", fmt.Errorf("read Docker Sandboxes staging directory identity %q: %w", path, err)
	}
	if actual != identity {
		return "", fmt.Errorf("Docker Sandboxes staging directory %q was replaced by a different filesystem object", path)
	}
	return path, nil
}

func (s *Staging) RemoveEmptyOwned(name, identity string) error {
	path, err := s.VerifyOwnedEmpty(name, identity)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove exact owned Docker Sandboxes staging directory %q: %w", path, err)
	}
	return nil
}

// PurgeOwned removes a non-empty direct workspace only after the sandbox has been
// proven absent. The deterministic quarantine name makes a crash after rename
// recoverable without ever selecting a path by prefix or following a replaced
// top-level object.
func (s *Staging) PurgeOwned(name, identity string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(identity) == "" {
		return fmt.Errorf("Docker Sandboxes staging directory identity is required")
	}
	if err := validateDirectory(s.root, false); err != nil {
		return fmt.Errorf("validate Docker Sandboxes staging root before purge: %w", err)
	}
	original, err := s.exactPath(name)
	if err != nil {
		return err
	}
	quarantineName := name + ".deleting"
	quarantine, err := s.exactPath(quarantineName)
	if err != nil {
		return err
	}
	originalInfo, originalErr := os.Lstat(original)
	quarantineInfo, quarantineErr := os.Lstat(quarantine)
	if originalErr == nil && quarantineErr == nil {
		return fmt.Errorf("Docker Sandboxes staging source and quarantine both exist")
	}
	if originalErr != nil && !os.IsNotExist(originalErr) {
		return fmt.Errorf("inspect exact Docker Sandboxes staging source: %w", originalErr)
	}
	if quarantineErr != nil && !os.IsNotExist(quarantineErr) {
		return fmt.Errorf("inspect exact Docker Sandboxes staging quarantine: %w", quarantineErr)
	}
	if originalErr == nil {
		if !originalInfo.IsDir() {
			return fmt.Errorf("Docker Sandboxes staging source is not a real directory")
		}
		if _, err := s.VerifyOwned(name, identity); err != nil {
			return err
		}
		if err := os.Rename(original, quarantine); err != nil {
			return fmt.Errorf("quarantine exact Docker Sandboxes staging source: %w", err)
		}
	} else if quarantineErr != nil {
		return nil
	}
	if quarantineInfo != nil && !quarantineInfo.IsDir() {
		return fmt.Errorf("Docker Sandboxes staging quarantine is not a real directory")
	}
	actual, err := platformDirectoryIdentity(quarantine)
	if err != nil {
		return fmt.Errorf("read quarantined Docker Sandboxes staging identity: %w", err)
	}
	if actual != identity {
		return fmt.Errorf("Docker Sandboxes staging quarantine was replaced by a different filesystem object")
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("purge exact quarantined Docker Sandboxes staging tree: %w", err)
	}
	if _, err := os.Lstat(quarantine); !os.IsNotExist(err) {
		return fmt.Errorf("verify exact Docker Sandboxes staging quarantine absence: %w", err)
	}
	return nil
}

func (s *Staging) exactPath(name string) (string, error) {
	path := filepath.Join(s.root, name)
	relative, err := filepath.Rel(s.root, path)
	if err != nil || relative != name || filepath.IsAbs(relative) || strings.Contains(relative, string(filepath.Separator)) {
		return "", fmt.Errorf("Docker Sandboxes staging path %q escapes its configured root", path)
	}
	return path, nil
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid Docker Sandboxes staging name %q", name)
	}
	for _, value := range name {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '.' || value == '_' || value == '-' {
			continue
		}
		return fmt.Errorf("invalid Docker Sandboxes staging name %q", name)
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return fmt.Errorf("invalid Docker Sandboxes staging name %q", name)
	}
	return nil
}

func validateDirectory(path string, requireEmpty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateNoRedirectDirectory(path); err != nil {
		return err
	}
	if err := validatePlatformPermissions(path, info); err != nil {
		return fmt.Errorf("Docker Sandboxes staging path %q has weak permissions: %w", path, err)
	}
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve Docker Sandboxes staging path %q: %w", path, err)
	}
	absEvaluated, err := filepath.Abs(evaluated)
	if err != nil {
		return fmt.Errorf("resolve canonical Docker Sandboxes staging path %q: %w", path, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve Docker Sandboxes staging path %q: %w", path, err)
	}
	canonicalPath, err := platformCanonicalPathSpelling(filepath.Clean(absPath))
	if err != nil {
		return fmt.Errorf("normalize Docker Sandboxes staging path %q: %w", path, err)
	}
	if !samePath(filepath.Clean(canonicalPath), filepath.Clean(absEvaluated)) {
		return fmt.Errorf("Docker Sandboxes staging path %q contains a symlink, junction, or reparse redirection", path)
	}
	if requireEmpty {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("read Docker Sandboxes staging path %q: %w", path, err)
		}
		if len(entries) != 0 {
			return fmt.Errorf("Docker Sandboxes staging path %q is not empty", path)
		}
	}
	return nil
}

func createPathWithoutRedirect(path string) error {
	missing := make([]string, 0)
	cursor := path
	for {
		_, err := os.Lstat(cursor)
		if err == nil {
			if err := validateNoRedirectDirectory(cursor); err != nil {
				return err
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, cursor)
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return fmt.Errorf("no existing ancestor for staging path")
		}
		cursor = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		candidate := missing[index]
		if err := os.Mkdir(candidate, 0700); err != nil {
			return err
		}
		if err := validateNoRedirectDirectory(candidate); err != nil {
			return err
		}
	}
	return nil
}

func validateNoRedirectDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if isPlatformRedirect(info) || !info.IsDir() {
		return fmt.Errorf("Docker Sandboxes staging path %q is not a real directory", path)
	}
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve Docker Sandboxes staging path %q: %w", path, err)
	}
	absEvaluated, err := filepath.Abs(evaluated)
	if err != nil {
		return fmt.Errorf("resolve canonical Docker Sandboxes staging path %q: %w", path, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve Docker Sandboxes staging path %q: %w", path, err)
	}
	canonicalPath, err := platformCanonicalPathSpelling(filepath.Clean(absPath))
	if err != nil {
		return fmt.Errorf("normalize Docker Sandboxes staging path %q: %w", path, err)
	}
	if !samePath(filepath.Clean(canonicalPath), filepath.Clean(absEvaluated)) {
		return fmt.Errorf("Docker Sandboxes staging path %q contains a symlink, junction, or reparse redirection", path)
	}
	return nil
}

func rejectAlternateDataStream(path string) error {
	rest := strings.TrimPrefix(path, filepath.VolumeName(path))
	if strings.Contains(rest, ":") {
		return fmt.Errorf("Docker Sandboxes staging path %q contains an alternate-data-stream separator", path)
	}
	return nil
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
