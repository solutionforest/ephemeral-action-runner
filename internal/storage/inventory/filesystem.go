package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

func hashFile(path string) (string, uint64, error) {
	before, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return "", 0, err
	}
	if before.Kind != storage.TargetFile {
		return "", 0, fmt.Errorf("%q is not a redirect-free regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	if written < 0 {
		return "", 0, fmt.Errorf("negative byte count while hashing %q", path)
	}
	after, err := storage.SnapshotFilesystemTarget(path)
	if err != nil {
		return "", 0, err
	}
	if after != before {
		return "", 0, fmt.Errorf("file identity or metadata drifted while hashing %q", path)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), uint64(written), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func directoryBytes(path string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if isRedirect(info) {
			return fmt.Errorf("storage inventory path %q contains a symlink, junction, or reparse point", current)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("storage inventory path %q contains a non-regular file", current)
		}
		if info.Size() < 0 || uint64(info.Size()) > math.MaxUint64-total {
			return fmt.Errorf("storage inventory byte count overflow at %q", current)
		}
		total += uint64(info.Size())
		return nil
	})
	return total, err
}

func stableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + ":" + hex.EncodeToString(sum[:])
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func unknownTarget(path string, directory bool) storage.Target {
	kind := storage.TargetFile
	if directory {
		kind = storage.TargetDirectory
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	return storage.Target{Kind: kind, Locator: filepath.Clean(absolute), Match: storage.MatchUnknown}
}
