package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FilesystemExecutor removes only exact files or directories strictly below
// explicitly approved roots. Every directory descendant is checked for links,
// reparse points, and special files before removal.
type FilesystemExecutor struct {
	roots []string
}

func NewFilesystemExecutor(allowedRoots ...string) (*FilesystemExecutor, error) {
	executor := &FilesystemExecutor{}
	for _, root := range allowedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		target, err := SnapshotFilesystemTarget(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("validate storage execution root %q: %w", root, err)
		}
		if target.Kind != TargetDirectory {
			return nil, fmt.Errorf("storage execution root %q is not a directory", root)
		}
		executor.roots = append(executor.roots, target.Locator)
	}
	if len(executor.roots) == 0 {
		return nil, fmt.Errorf("at least one existing exact storage execution root is required")
	}
	return executor, nil
}

func (executor *FilesystemExecutor) ObserveExact(_ context.Context, target Target) (Observation, error) {
	observed, err := SnapshotFilesystemTarget(target.Locator)
	if os.IsNotExist(err) {
		return Observation{Exists: false, Target: target}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	return Observation{Exists: true, Target: observed}, nil
}

func (executor *FilesystemExecutor) RemoveExact(ctx context.Context, removal Removal) error {
	if err := validateExactTarget(removal.Target); err != nil {
		return err
	}
	if removal.Target.Kind != TargetFile && removal.Target.Kind != TargetDirectory {
		return fmt.Errorf("filesystem executor does not support target kind %q", removal.Target.Kind)
	}
	if !executor.allowed(removal.Target.Locator) {
		return fmt.Errorf("storage target %q is not strictly below an approved root", removal.Target.Locator)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	observed, err := SnapshotFilesystemTarget(removal.Target.Locator)
	if err != nil {
		return err
	}
	if observed != removal.Target {
		return fmt.Errorf("storage target identity changed before exact removal")
	}
	if observed.Kind == TargetFile {
		return os.Remove(observed.Locator)
	}
	if err := validateRemovalTree(ctx, observed.Locator); err != nil {
		return err
	}
	observedAgain, err := SnapshotFilesystemTarget(removal.Target.Locator)
	if err != nil {
		return err
	}
	if observedAgain != removal.Target {
		return fmt.Errorf("storage directory identity changed during exact removal validation")
	}
	return os.RemoveAll(observed.Locator)
}

func (executor *FilesystemExecutor) allowed(path string) bool {
	for _, root := range executor.roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func validateRemovalTree(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if isFilesystemRedirect(info) {
			return fmt.Errorf("refusing to remove redirected storage descendant %q", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to remove special storage descendant %q", path)
		}
		return nil
	})
}
