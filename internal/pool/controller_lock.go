package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const poolControllerLockDirectory = "pool-controller-locks"

type poolControllerLockOwner struct {
	ConfigPath string    `json:"configPath"`
	Provider   string    `json:"provider"`
	NamePrefix string    `json:"namePrefix"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
}

type poolControllerLocks struct {
	config *filelock.Lock
	prefix *filelock.Lock
}

func (locks *poolControllerLocks) Close() error {
	if locks == nil {
		return nil
	}
	var result error
	if locks.prefix != nil {
		result = errors.Join(result, locks.prefix.Close())
	}
	if locks.config != nil {
		result = errors.Join(result, locks.config.Close())
	}
	return result
}

// AcquirePoolControllerLock excludes another mutating controller that uses
// either this canonical configuration path or this normalized pool prefix.
// Config locking prevents a second controller from bypassing an active one by
// editing provider or prefix fields in place; prefix locking protects global
// provider and GitHub runner identities across configurations and projects.
func (m *Manager) AcquirePoolControllerLock() (io.Closer, error) {
	providerType := strings.TrimSpace(strings.ToLower(m.Config.Provider.Type))
	namePrefix := strings.TrimSpace(strings.ToLower(m.Config.Pool.NamePrefix))
	if providerType == "" || namePrefix == "" {
		return nil, fmt.Errorf("acquire pool controller lock: provider.type and pool.namePrefix are required")
	}
	canonicalConfig, err := m.canonicalPoolControllerConfigPath()
	if err != nil {
		return nil, err
	}
	root, err := storagecatalog.DefaultRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve EPAR state root for pool controller lock: %w", err)
	}
	lockRoot := filepath.Join(root, poolControllerLockDirectory)
	if err := os.MkdirAll(lockRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create pool controller lock directory: %w", err)
	}
	configPath := poolControllerLockPath(lockRoot, "config", canonicalConfig)
	prefixPath := poolControllerLockPath(lockRoot, "prefix", namePrefix)
	configLock, err := filelock.Acquire(configPath)
	if err != nil {
		return nil, poolControllerLockError("configuration", canonicalConfig, configPath, err)
	}
	prefixLock, err := filelock.Acquire(prefixPath)
	if err != nil {
		_ = configLock.Close()
		return nil, poolControllerLockError("pool.namePrefix", namePrefix, prefixPath, err)
	}
	locks := &poolControllerLocks{config: configLock, prefix: prefixLock}
	owner := poolControllerLockOwner{
		ConfigPath: canonicalConfig,
		Provider:   providerType,
		NamePrefix: namePrefix,
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC(),
	}
	if err := writePoolControllerLockOwner(configLock, owner); err != nil {
		_ = locks.Close()
		return nil, err
	}
	if err := writePoolControllerLockOwner(prefixLock, owner); err != nil {
		_ = locks.Close()
		return nil, err
	}
	if m.LifecycleStateEnabled && m.LifecycleState == nil {
		lifecycleState, err := OpenLifecycleState(m.ProjectRoot, m.ConfigPath)
		if err != nil {
			_ = locks.Close()
			return nil, fmt.Errorf("open lifecycle state after acquiring pool controller locks: %w", err)
		}
		m.LifecycleState = lifecycleState
	}
	return locks, nil
}

func (m *Manager) canonicalPoolControllerConfigPath() (string, error) {
	configPath := strings.TrimSpace(m.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(m.ProjectRoot, ".local", "config.yml")
	}
	canonicalConfig, err := storagecatalog.CanonicalPath(configPath)
	if err != nil {
		return "", fmt.Errorf("acquire pool controller lock: resolve config path: %w", err)
	}
	return canonicalConfig, nil
}

func poolControllerLockPath(root, kind, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(root, kind+"-"+hex.EncodeToString(sum[:])+".lock")
}

func writePoolControllerLockOwner(lock *filelock.Lock, owner poolControllerLockOwner) error {
	content, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode pool controller lock owner: %w", err)
	}
	if err := lock.ReplaceContent(append(content, '\n')); err != nil {
		return fmt.Errorf("write pool controller lock owner metadata: %w", err)
	}
	return nil
}

func poolControllerLockError(kind, identity, path string, lockErr error) error {
	if !errors.Is(lockErr, filelock.ErrLocked) {
		return fmt.Errorf("acquire pool controller %s lock for %q: %w", kind, identity, lockErr)
	}
	message := fmt.Sprintf("pool controller %s lock is already held for %q", kind, identity)
	if owner, err := readPoolControllerLockOwner(path); err == nil {
		message += fmt.Sprintf(" (owner config=%q provider=%q prefix=%q pid=%d startedAt=%s)", owner.ConfigPath, owner.Provider, owner.NamePrefix, owner.PID, owner.StartedAt.UTC().Format(time.RFC3339))
	}
	return errors.New(message)
}

func readPoolControllerLockOwner(path string) (poolControllerLockOwner, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return poolControllerLockOwner{}, err
	}
	var owner poolControllerLockOwner
	if err := json.Unmarshal(content, &owner); err != nil {
		return poolControllerLockOwner{}, err
	}
	if owner.ConfigPath == "" || owner.Provider == "" || owner.NamePrefix == "" || owner.PID <= 0 || owner.StartedAt.IsZero() {
		return poolControllerLockOwner{}, errors.New("invalid pool controller lock owner metadata")
	}
	return owner, nil
}
