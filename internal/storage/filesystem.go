package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ProbeFilesystemCapacity returns an OS capacity observation for an existing
// filesystem path. Read-only measurement follows symlinks and junctions to the
// physical filesystem domain; cleanup identity remains redirect-strict.
func ProbeFilesystemCapacity(path string, now time.Time) (Capacity, error) {
	domain, err := ProbeFilesystemCapacityDomain(path, now)
	if err != nil {
		return Capacity{}, err
	}
	return domain.Capacity, nil
}

// ProbeFilesystemCapacityDomain resolves an existing path for read-only
// measurement and returns its stable Windows volume or Unix filesystem domain.
func ProbeFilesystemCapacityDomain(path string, now time.Time) (CapacityDomain, error) {
	canonical, err := inspectFilesystemCapacityPath(path)
	if err != nil {
		return CapacityDomain{}, err
	}
	identity, domainPath, available, total, err := platformFilesystemCapacityDomain(canonical)
	if err != nil {
		return CapacityDomain{}, fmt.Errorf("probe filesystem capacity domain for %q: %w", canonical, err)
	}
	provenance := "unix-filesystem-probe"
	if runtime.GOOS == "windows" {
		provenance = "windows-volume-probe"
	}
	return CapacityDomain{
		ID:         identity,
		Kind:       SurfaceHostFilesystem,
		Identity:   identity,
		Path:       domainPath,
		Provenance: provenance,
		Confidence: "authoritative-filesystem-probe",
		Capacity:   Capacity{Known: true, AvailableBytes: available, TotalBytes: total, ObservedAt: now.UTC()},
	}, nil
}

// SnapshotFilesystemTarget binds a regular file or real directory to a stable
// object identity and shallow metadata fingerprint. Symlinks, junctions,
// reparse points, special files, and redirected ancestors are rejected.
func SnapshotFilesystemTarget(path string) (Target, error) {
	canonical, info, err := inspectFilesystemPath(path)
	if err != nil {
		return Target{}, err
	}
	kind := TargetFile
	if info.IsDir() {
		kind = TargetDirectory
	} else if !info.Mode().IsRegular() {
		return Target{}, fmt.Errorf("storage path %q is not a regular file or real directory", canonical)
	}
	identity, err := platformFilesystemIdentity(canonical, info.IsDir())
	if err != nil {
		return Target{}, fmt.Errorf("read stable filesystem identity for %q: %w", canonical, err)
	}
	metadata := struct {
		Size    int64  `json:"size"`
		Mode    uint32 `json:"mode"`
		ModTime string `json:"modTime"`
	}{
		Size:    info.Size(),
		Mode:    uint32(info.Mode()),
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return Target{}, err
	}
	sum := sha256.Sum256(encoded)
	return Target{
		Kind:        kind,
		Locator:     canonical,
		Identity:    identity,
		Fingerprint: "sha256:" + hex.EncodeToString(sum[:]),
		Match:       MatchExact,
	}, nil
}

func inspectFilesystemPath(path string) (string, os.FileInfo, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
		return "", nil, errors.New("storage filesystem path is empty or contains NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	absolute = filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		rest := strings.TrimPrefix(absolute, filepath.VolumeName(absolute))
		if strings.Contains(rest, ":") {
			return "", nil, fmt.Errorf("storage filesystem path %q contains an alternate-data-stream separator", absolute)
		}
	}
	if err := rejectRedirectedAncestors(absolute); err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, err
	}
	if isFilesystemRedirect(info) {
		return "", nil, fmt.Errorf("storage filesystem path %q is a symlink, junction, or reparse point", absolute)
	}
	evaluated, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, err
	}
	evaluated, err = filepath.Abs(evaluated)
	if err != nil {
		return "", nil, err
	}
	canonicalSpelling, err := platformCanonicalFilesystemPath(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("normalize storage filesystem path %q: %w", absolute, err)
	}
	canonicalSpelling = filepath.Clean(canonicalSpelling)
	if !sameFilesystemPath(canonicalSpelling, filepath.Clean(evaluated)) {
		return "", nil, fmt.Errorf("storage filesystem path %q contains a symlink, junction, or reparse redirection", absolute)
	}
	return canonicalSpelling, info, nil
}

func inspectFilesystemCapacityPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
		return "", errors.New("storage filesystem path is empty or contains NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	evaluated, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	evaluated, err = filepath.Abs(evaluated)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(evaluated); err != nil {
		return "", err
	}
	canonical, err := platformCanonicalFilesystemPath(evaluated)
	if err != nil {
		return "", fmt.Errorf("normalize storage filesystem capacity path %q: %w", evaluated, err)
	}
	return filepath.Clean(canonical), nil
}

func rejectRedirectedAncestors(path string) error {
	cursor := path
	for {
		info, err := os.Lstat(cursor)
		if err != nil {
			return err
		}
		if isFilesystemRedirect(info) {
			return fmt.Errorf("storage filesystem path %q has redirected ancestor %q", path, cursor)
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return nil
		}
		cursor = parent
	}
}

func sameFilesystemPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
