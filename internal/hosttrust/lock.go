package hosttrust

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrConfigLockHeld means another live controller already owns the host-trust
// lock for this canonical configuration path.
var ErrConfigLockHeld = errors.New("host trust config lock is already held")

var (
	errPlatformLockHeld = errors.New("platform file lock is already held")
	configLocks         sync.Map // canonical config path -> owner token
)

// AcquireConfigLock obtains an exclusive, process-lifetime lock for configPath.
// The lock is independent per canonical path, so separate configurations can
// collect host trust concurrently. Call Close when the controller exits.
func AcquireConfigLock(configPath string) (io.Closer, error) {
	canonicalPath, err := canonicalConfigPath(configPath)
	if err != nil {
		return nil, err
	}
	ownerToken, err := randomOwnerToken()
	if err != nil {
		return nil, fmt.Errorf("create host trust lock owner token: %w", err)
	}
	if _, loaded := configLocks.LoadOrStore(canonicalPath, ownerToken); loaded {
		return nil, fmt.Errorf("%w: %s", ErrConfigLockHeld, canonicalPath)
	}
	sharedDir, err := acquireSharedConfigLock(canonicalPath)
	if err != nil {
		configLocks.Delete(canonicalPath)
		return nil, err
	}

	lockPath, err := configLockPath(canonicalPath)
	if err != nil {
		releaseSharedConfigLock(sharedDir)
		configLocks.Delete(canonicalPath)
		return nil, err
	}
	file, err := acquirePlatformConfigLock(lockPath)
	if err != nil {
		releaseSharedConfigLock(sharedDir)
		configLocks.Delete(canonicalPath)
		if errors.Is(err, errPlatformLockHeld) {
			return nil, fmt.Errorf("%w: %s", ErrConfigLockHeld, canonicalPath)
		}
		return nil, fmt.Errorf("acquire host trust config lock %s: %w", canonicalPath, err)
	}
	metadata := configLockMetadata{
		ConfigPath: canonicalPath,
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC(),
		OwnerToken: ownerToken,
	}
	if err := writeConfigLockMetadata(file, metadata); err != nil {
		_ = releasePlatformConfigLock(file)
		_ = file.Close()
		releaseSharedConfigLock(sharedDir)
		configLocks.Delete(canonicalPath)
		return nil, err
	}
	return &configLock{file: file, canonicalPath: canonicalPath, ownerToken: ownerToken, sharedDir: sharedDir}, nil
}

type configLock struct {
	file          *os.File
	canonicalPath string
	ownerToken    string
	sharedDir     string
	once          sync.Once
}

func (lock *configLock) Close() error {
	var result error
	lock.once.Do(func() {
		result = releasePlatformConfigLock(lock.file)
		if err := lock.file.Close(); result == nil {
			result = err
		}
		if token, ok := configLocks.Load(lock.canonicalPath); ok && token == lock.ownerToken {
			configLocks.Delete(lock.canonicalPath)
		}
		releaseSharedConfigLock(lock.sharedDir)
	})
	return result
}

type configLockMetadata struct {
	ConfigPath string    `json:"configPath"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
	OwnerToken string    `json:"ownerToken"`
}

func canonicalConfigPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("host trust config path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve host trust config path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

func acquireSharedConfigLock(canonicalPath string) (string, error) {
	sum := sha256.Sum256([]byte(canonicalPath))
	root, err := hostTrustCacheRoot()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, hex.EncodeToString(sum[:])[:32]+".lock")
	for attempt := 0; attempt < 2; attempt++ {
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", fmt.Errorf("create host trust cache root: %w", err)
		}
		if err := os.Mkdir(directory, 0700); err == nil {
			if err := os.WriteFile(filepath.Join(directory, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
				releaseSharedConfigLock(directory)
				return "", fmt.Errorf("write shared host trust lock owner: %w", err)
			}
			return directory, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create shared host trust lock: %w", err)
		}
		content, readErr := os.ReadFile(filepath.Join(directory, "pid"))
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
		if readErr == nil && parseErr == nil && pid > 0 && platformProcessAlive(pid) {
			return "", fmt.Errorf("%w: %s", ErrConfigLockHeld, canonicalPath)
		}
		if err := os.Remove(filepath.Join(directory, "pid")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove stale shared host trust lock owner: %w", err)
		}
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove stale shared host trust lock: %w", err)
		}
	}
	return "", fmt.Errorf("%w: %s", ErrConfigLockHeld, canonicalPath)
}

func releaseSharedConfigLock(directory string) {
	if directory == "" {
		return
	}
	_ = os.Remove(filepath.Join(directory, "pid"))
	_ = os.Remove(directory)
}

func configLockPath(canonicalPath string) (string, error) {
	sum := sha256.Sum256([]byte(canonicalPath))
	root, err := hostTrustCacheRoot()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, "locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create host trust lock directory: %w", err)
	}
	return filepath.Join(directory, hex.EncodeToString(sum[:])+".lock"), nil
}

func hostTrustCacheRoot() (string, error) {
	if runtime.GOOS == "windows" {
		if root := os.Getenv("LOCALAPPDATA"); root != "" {
			return filepath.Join(root, "ephemeral-action-runner", "host-trust"), nil
		}
		return filepath.Join(os.TempDir(), "ephemeral-action-runner", "host-trust"), nil
	}
	if root := os.Getenv("XDG_CACHE_HOME"); root != "" {
		return filepath.Join(root, "ephemeral-action-runner", "host-trust"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for host trust cache: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "ephemeral-action-runner", "host-trust"), nil
	}
	return filepath.Join(home, ".cache", "ephemeral-action-runner", "host-trust"), nil
}

func randomOwnerToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func writeConfigLockMetadata(file *os.File, metadata configLockMetadata) error {
	content, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode host trust lock metadata: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate host trust lock metadata: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek host trust lock metadata: %w", err)
	}
	if _, err := file.Write(append(content, '\n')); err != nil {
		return fmt.Errorf("write host trust lock metadata: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync host trust lock metadata: %w", err)
	}
	return nil
}
