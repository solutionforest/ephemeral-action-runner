package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
)

// RetentionPolicy applies category ages first, then an aggregate total-byte
// budget to the remaining recognized files. A zero value disables that limit.
type RetentionPolicy struct {
	MaxTotalBytes   int64
	ManagerMaxAge   time.Duration
	InstanceMaxAge  time.Duration
	BuildMaxAge     time.Duration
	ErrorMaxAge     time.Duration
	BenchmarkMaxAge time.Duration
	Now             func() time.Time
}

func (policy RetentionPolicy) maxAge(category Category) time.Duration {
	switch category {
	case CategoryManager:
		return policy.ManagerMaxAge
	case CategoryInstances:
		return policy.InstanceMaxAge
	case CategoryBuilds:
		return policy.BuildMaxAge
	case CategoryErrors:
		return policy.ErrorMaxAge
	case CategoryBenchmarks:
		return policy.BenchmarkMaxAge
	default:
		return 0
	}
}

// RetentionAction describes the outcome or protection applied to one entry.
type RetentionAction string

const (
	RetentionKeep        RetentionAction = "keep"
	RetentionProtected   RetentionAction = "protected"
	RetentionWouldDelete RetentionAction = "would-delete"
	RetentionDeleted     RetentionAction = "deleted"
	RetentionSkipped     RetentionAction = "skipped"
)

// RetentionEntry is a shallow, evidence-bearing retention decision.
type RetentionEntry struct {
	Path       string          `json:"path"`
	Category   Category        `json:"category,omitempty"`
	Size       int64           `json:"size"`
	ModTime    time.Time       `json:"modTime"`
	Recognized bool            `json:"recognized"`
	Active     bool            `json:"active,omitempty"`
	Action     RetentionAction `json:"action"`
	Reason     string          `json:"reason"`
	info       os.FileInfo
}

// RetentionReport summarizes a list, dry-run, or deletion pass.
type RetentionReport struct {
	DryRun           bool             `json:"dryRun"`
	Scanned          int              `json:"scanned"`
	Recognized       int              `json:"recognized"`
	Protected        int              `json:"protected"`
	WouldDelete      int              `json:"wouldDelete"`
	Deleted          int              `json:"deleted"`
	ReclaimedBytes   int64            `json:"reclaimedBytes"`
	TotalBytesBefore int64            `json:"totalBytesBefore"`
	TotalBytesAfter  int64            `json:"totalBytesAfter"`
	Warnings         []string         `json:"warnings,omitempty"`
	Entries          []RetentionEntry `json:"entries"`
}

// Summary returns a compact stable summary suitable for list/dry-run output.
func (report RetentionReport) Summary() string {
	return fmt.Sprintf("scanned=%d recognized=%d protected=%d would_delete=%d deleted=%d reclaimed_bytes=%d total_before=%d total_after=%d warnings=%d", report.Scanned, report.Recognized, report.Protected, report.WouldDelete, report.Deleted, report.ReclaimedBytes, report.TotalBytesBefore, report.TotalBytesAfter, len(report.Warnings))
}

// ListRetention performs the same classification and planning as a dry-run
// prune without modifying the filesystem.
func ListRetention(root string, policy RetentionPolicy) (RetentionReport, error) {
	return pruneRetention(root, policy, true)
}

// PruneRetention removes planned entries unless dryRun is true.
func PruneRetention(root string, policy RetentionPolicy, dryRun bool) (RetentionReport, error) {
	return pruneRetention(root, policy, dryRun)
}

// ListRetention lists this runtime's logging root.
func (runtime *Runtime) ListRetention(policy RetentionPolicy) (RetentionReport, error) {
	return ListRetention(runtime.root, policy)
}

// PruneRetention prunes this runtime's logging root.
func (runtime *Runtime) PruneRetention(policy RetentionPolicy, dryRun bool) (RetentionReport, error) {
	return PruneRetention(runtime.root, policy, dryRun)
}

func pruneRetention(root string, policy RetentionPolicy, dryRun bool) (RetentionReport, error) {
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return RetentionReport{}, err
	}
	rootInfo, err := os.Lstat(canonicalRoot)
	if errors.Is(err, os.ErrNotExist) {
		return RetentionReport{DryRun: dryRun}, nil
	}
	if err != nil {
		return RetentionReport{}, fmt.Errorf("inspect logging root: %w", err)
	}
	if !rootInfo.IsDir() || isLinkOrReparse(rootInfo) {
		return RetentionReport{}, fmt.Errorf("logging root %s is not a safe directory", canonicalRoot)
	}
	if policy.MaxTotalBytes < 0 {
		return RetentionReport{}, errors.New("retention max total bytes must not be negative")
	}
	for _, age := range []time.Duration{policy.ManagerMaxAge, policy.InstanceMaxAge, policy.BuildMaxAge, policy.ErrorMaxAge, policy.BenchmarkMaxAge} {
		if age < 0 {
			return RetentionReport{}, errors.New("retention max ages must not be negative")
		}
	}
	now := time.Now().UTC()
	if policy.Now != nil {
		now = policy.Now().UTC()
	}
	report := RetentionReport{DryRun: dryRun}
	active, uncertain := discoverActivePaths(canonicalRoot, &report)
	entries, err := scanRetentionEntries(canonicalRoot, active, uncertain, &report)
	if err != nil {
		return RetentionReport{}, err
	}
	report.Entries = entries

	deletions := make(map[int]string)
	for index := range report.Entries {
		entry := &report.Entries[index]
		if entry.Action == RetentionProtected {
			report.Protected++
		}
		if !entry.Recognized {
			continue
		}
		report.Recognized++
		report.TotalBytesBefore += entry.Size
		if entry.Action == RetentionProtected {
			continue
		}
		maxAge := policy.maxAge(entry.Category)
		if maxAge > 0 && !entry.ModTime.After(now.Add(-maxAge)) {
			deletions[index] = "category-age"
		}
	}

	remaining := report.TotalBytesBefore
	for index := range deletions {
		remaining -= report.Entries[index].Size
	}
	if policy.MaxTotalBytes > 0 && remaining > policy.MaxTotalBytes {
		eligible := make([]int, 0, len(report.Entries))
		for index, entry := range report.Entries {
			if entry.Recognized && entry.Action != RetentionProtected {
				if _, selected := deletions[index]; !selected {
					eligible = append(eligible, index)
				}
			}
		}
		sort.Slice(eligible, func(i, j int) bool {
			left, right := report.Entries[eligible[i]], report.Entries[eligible[j]]
			if left.ModTime.Equal(right.ModTime) {
				return left.Path < right.Path
			}
			return left.ModTime.Before(right.ModTime)
		})
		for _, index := range eligible {
			if remaining <= policy.MaxTotalBytes {
				break
			}
			deletions[index] = "aggregate-budget"
			remaining -= report.Entries[index].Size
		}
		if remaining > policy.MaxTotalBytes {
			report.Warnings = append(report.Warnings, fmt.Sprintf("retention aggregate budget remains exceeded: projected_bytes=%d budget_bytes=%d protected_or_active_bytes=%d", remaining, policy.MaxTotalBytes, remaining))
		}
	}

	for index, reason := range deletions {
		entry := &report.Entries[index]
		entry.Reason = reason
		entry.Action = RetentionWouldDelete
		report.WouldDelete++
		if dryRun {
			continue
		}
		if warning := deleteRetentionEntry(canonicalRoot, entry); warning != "" {
			entry.Action = RetentionSkipped
			entry.Reason = "safe-skip"
			report.Warnings = append(report.Warnings, warning)
			continue
		}
		entry.Action = RetentionDeleted
		report.Deleted++
		report.ReclaimedBytes += entry.Size
	}
	if dryRun {
		var projectedBytes int64
		for index := range deletions {
			projectedBytes += report.Entries[index].Size
		}
		report.TotalBytesAfter = report.TotalBytesBefore - projectedBytes
	} else {
		report.TotalBytesAfter = report.TotalBytesBefore - report.ReclaimedBytes
	}
	sort.Slice(report.Entries, func(i, j int) bool { return report.Entries[i].Path < report.Entries[j].Path })
	return report, nil
}

func scanRetentionEntries(root string, active map[string]struct{}, uncertain bool, report *RetentionReport) ([]RetentionEntry, error) {
	rootEntries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read logging root: %w", err)
	}
	var entries []RetentionEntry
	for _, directoryEntry := range rootEntries {
		path := filepath.Join(root, directoryEntry.Name())
		if directoryEntry.Name() == controlDirectoryName {
			entries = append(entries, protectedEntry(path, "control"))
			continue
		}
		category := Category(directoryEntry.Name())
		if directoryEntry.IsDir() && category.validTranscriptCategory() {
			info, infoErr := directoryEntry.Info()
			if infoErr != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("safe skip %s: %v", path, infoErr))
				entries = append(entries, protectedEntry(path, "inspection-error"))
				continue
			}
			if isLinkOrReparse(info) {
				entries = append(entries, protectedEntry(path, "symlink-or-reparse"))
				continue
			}
			children, readErr := os.ReadDir(path)
			if readErr != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("safe skip category %s: %v", path, readErr))
				entries = append(entries, protectedEntry(path, "inspection-error"))
				continue
			}
			for _, child := range children {
				entries = append(entries, classifyRetentionEntry(root, filepath.Join(path, child.Name()), child, active, uncertain, report))
			}
			continue
		}
		entries = append(entries, classifyRetentionEntry(root, path, directoryEntry, active, uncertain, report))
	}
	return entries, nil
}

func classifyRetentionEntry(root, path string, directoryEntry os.DirEntry, active map[string]struct{}, uncertain bool, report *RetentionReport) RetentionEntry {
	report.Scanned++
	info, err := directoryEntry.Info()
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("safe skip %s: %v", path, err))
		return protectedEntry(path, "inspection-error")
	}
	entry := RetentionEntry{Path: path, Size: info.Size(), ModTime: info.ModTime().UTC(), Action: RetentionProtected, Reason: "unknown", info: info}
	if !info.Mode().IsRegular() || isLinkOrReparse(info) {
		entry.Reason = "non-regular-or-reparse"
		return entry
	}
	recognized, ok := recognizePath(root, path)
	if !ok {
		return entry
	}
	entry.Recognized = true
	entry.Category = recognized.category
	if recognized.current {
		entry.Reason = "current"
		return entry
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		entry.Reason = "canonicalization-error"
		report.Warnings = append(report.Warnings, fmt.Sprintf("safe skip %s: %v", path, err))
		return entry
	}
	if _, ok := active[canonical]; ok {
		entry.Active = true
		entry.Reason = "active"
		return entry
	}
	if recognized.backup {
		activeBase, baseErr := canonicalPath(activeLockPathForRecognized(canonical, recognized))
		if baseErr != nil {
			entry.Reason = "canonicalization-error"
			report.Warnings = append(report.Warnings, fmt.Sprintf("safe skip backup base %s: %v", path, baseErr))
			return entry
		}
		if _, ok := active[activeBase]; ok {
			entry.Active = true
			entry.Reason = "active"
			return entry
		}
	}
	if uncertain {
		entry.Reason = "active-state-uncertain"
		return entry
	}
	entry.Action = RetentionKeep
	entry.Reason = "within-policy"
	return entry
}

func protectedEntry(path, reason string) RetentionEntry {
	return RetentionEntry{Path: path, Action: RetentionProtected, Reason: reason}
}

func deleteRetentionEntry(root string, entry *RetentionEntry) string {
	canonical, err := canonicalPath(entry.Path)
	if err != nil || !pathWithin(root, canonical) {
		return fmt.Sprintf("safe skip %s: path no longer resolves inside logging root", entry.Path)
	}
	if _, active := activeRegistryPaths()[canonical]; active {
		return fmt.Sprintf("safe skip %s: path became active", entry.Path)
	}
	lockDir := filepath.Join(root, controlDirectoryName, "locks")
	if err := ensureSafeDirectory(filepath.Join(root, controlDirectoryName), 0o700); err != nil {
		return fmt.Sprintf("safe skip %s: create retention control directory: %v", entry.Path, err)
	}
	if err := ensureSafeDirectory(lockDir, 0o700); err != nil {
		return fmt.Sprintf("safe skip %s: create retention lock directory: %v", entry.Path, err)
	}
	recognized, ok := recognizePath(root, canonical)
	if !ok {
		return fmt.Sprintf("safe skip %s: filename is no longer recognized", entry.Path)
	}
	lockTarget, err := canonicalPath(activeLockPathForRecognized(canonical, recognized))
	if err != nil {
		return fmt.Sprintf("safe skip %s: resolve active lock target: %v", entry.Path, err)
	}
	lock, err := filelock.Acquire(filepath.Join(lockDir, pathHash(lockTarget)+".lock"))
	if err != nil {
		return fmt.Sprintf("safe skip %s: acquire deletion lock: %v", entry.Path, err)
	}
	defer lock.Close()
	info, err := os.Lstat(canonical)
	if err != nil {
		return fmt.Sprintf("safe skip %s: re-inspect before delete: %v", entry.Path, err)
	}
	if !info.Mode().IsRegular() || isLinkOrReparse(info) || entry.info == nil || !os.SameFile(entry.info, info) || info.Size() != entry.Size || !info.ModTime().Equal(entry.info.ModTime()) {
		return fmt.Sprintf("safe skip %s: file identity or metadata changed", entry.Path)
	}
	if err := os.Remove(canonical); err != nil {
		return fmt.Sprintf("safe skip %s: remove failed: %v", entry.Path, err)
	}
	return ""
}

func discoverActivePaths(root string, report *RetentionReport) (map[string]struct{}, bool) {
	active := activeRegistryPaths()
	control := filepath.Join(root, controlDirectoryName)
	lockDir := filepath.Join(control, "locks")
	activeDir := filepath.Join(control, "active")
	if info, err := os.Lstat(control); err == nil {
		if !info.IsDir() || isLinkOrReparse(info) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("active log control path %s is not a safe directory", control))
			return active, true
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return active, false
	} else {
		report.Warnings = append(report.Warnings, fmt.Sprintf("cannot inspect active log control path %s: %v", control, err))
		return active, true
	}
	hashes := make(map[string]struct{})
	uncertain := false
	collect := func(directory, extension string) {
		info, statErr := os.Lstat(directory)
		if errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if statErr != nil || !info.IsDir() || isLinkOrReparse(info) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot safely inspect active log state %s", directory))
			uncertain = true
			return
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot inspect active log state %s: %v", directory, err))
			uncertain = true
			return
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), extension) {
				hashes[strings.TrimSuffix(entry.Name(), extension)] = struct{}{}
			}
		}
	}
	collect(lockDir, ".lock")
	collect(activeDir, ".json")
	for hash := range hashes {
		if len(hash) != 64 {
			report.Warnings = append(report.Warnings, fmt.Sprintf("ignored malformed active-state key %q", hash))
			uncertain = true
			continue
		}
		lockPath := filepath.Join(lockDir, hash+".lock")
		if err := ensureSafeDirectory(lockDir, 0o700); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot create active-state lock directory: %v", err))
			uncertain = true
			break
		}
		lock, err := filelock.Acquire(lockPath)
		if err == nil {
			metadataPath := filepath.Join(activeDir, hash+".json")
			if removeErr := os.Remove(metadataPath); removeErr == nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("recovered stale active log metadata %s", metadataPath))
			} else if !errors.Is(removeErr, os.ErrNotExist) {
				report.Warnings = append(report.Warnings, fmt.Sprintf("cannot remove stale active log metadata %s: %v", metadataPath, removeErr))
			}
			_ = lock.Close()
			continue
		}
		if !errors.Is(err, filelock.ErrLocked) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot determine active log state for %s: %v", hash, err))
			uncertain = true
			continue
		}
		metadataPath := filepath.Join(activeDir, hash+".json")
		data, readErr := os.ReadFile(metadataPath)
		if readErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("active lock %s has no readable metadata: %v", hash, readErr))
			uncertain = true
			continue
		}
		var state activeState
		if decodeErr := json.Unmarshal(data, &state); decodeErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("active lock %s has invalid metadata: %v", hash, decodeErr))
			uncertain = true
			continue
		}
		canonical, canonicalErr := canonicalPath(state.Path)
		if canonicalErr != nil || pathHash(canonical) != hash || !pathWithin(root, canonical) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("active lock %s has unsafe path metadata", hash))
			uncertain = true
			continue
		}
		active[canonical] = struct{}{}
	}
	return active, uncertain
}
