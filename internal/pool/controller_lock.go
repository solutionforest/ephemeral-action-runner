package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/solutionforest/ephemeral-action-runner/internal/hosttrust"
)

// AcquirePoolControllerLock excludes another mutating controller for the same
// canonical configuration, provider, and pool prefix on this host.
func (m *Manager) AcquirePoolControllerLock() (io.Closer, error) {
	providerType := strings.TrimSpace(strings.ToLower(m.Config.Provider.Type))
	namePrefix := strings.TrimSpace(strings.ToLower(m.Config.Pool.NamePrefix))
	if providerType == "" || namePrefix == "" {
		return nil, fmt.Errorf("acquire pool controller lock: provider.type and pool.namePrefix are required")
	}
	configPath := strings.TrimSpace(m.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(m.ProjectRoot, ".local", "config.yml")
	}
	canonicalConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("acquire pool controller lock: resolve config path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(canonicalConfig); resolveErr == nil {
		canonicalConfig = resolved
	}
	canonicalConfig = filepath.Clean(canonicalConfig)
	if runtime.GOOS == "windows" {
		canonicalConfig = strings.ToLower(canonicalConfig)
	}
	identity := canonicalConfig + "\x00" + providerType + "\x00" + namePrefix
	sum := sha256.Sum256([]byte(identity))
	syntheticPath := filepath.Join(os.TempDir(), "ephemeral-action-runner", "pool-controller", hex.EncodeToString(sum[:])+".identity")
	lock, err := hosttrust.AcquireConfigLock(syntheticPath)
	if err != nil {
		return nil, fmt.Errorf("acquire pool controller lock for provider %q prefix %q: %w", providerType, namePrefix, err)
	}
	return lock, nil
}
