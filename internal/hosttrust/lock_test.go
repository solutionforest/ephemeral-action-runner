package hosttrust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestAcquireConfigLockExcludesSameConfig(t *testing.T) {
	setLockCacheForTest(t)
	config := filepath.Join(t.TempDir(), "config.yml")
	first, err := AcquireConfigLock(config)
	if err != nil {
		t.Fatalf("AcquireConfigLock first: %v", err)
	}
	defer first.Close()
	second, err := AcquireConfigLock(config)
	if second != nil {
		_ = second.Close()
		t.Fatal("second same-config lock unexpectedly succeeded")
	}
	if !errors.Is(err, ErrConfigLockHeld) {
		t.Fatalf("second same-config error = %v, want ErrConfigLockHeld", err)
	}
}

func TestAcquireConfigLockReleaseAllowsReacquire(t *testing.T) {
	setLockCacheForTest(t)
	config := filepath.Join(t.TempDir(), "config.yml")
	first, err := AcquireConfigLock(config)
	if err != nil {
		t.Fatalf("AcquireConfigLock first: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first lock: %v", err)
	}
	second, err := AcquireConfigLock(config)
	if err != nil {
		t.Fatalf("AcquireConfigLock after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second lock: %v", err)
	}
}

func TestAcquireConfigLockAllowsDifferentConfigs(t *testing.T) {
	setLockCacheForTest(t)
	directory := t.TempDir()
	first, err := AcquireConfigLock(filepath.Join(directory, "one.yml"))
	if err != nil {
		t.Fatalf("AcquireConfigLock first: %v", err)
	}
	defer first.Close()
	second, err := AcquireConfigLock(filepath.Join(directory, "two.yml"))
	if err != nil {
		t.Fatalf("AcquireConfigLock second different config: %v", err)
	}
	defer second.Close()
}

func TestAcquireConfigLockHonorsLiveSharedWrapperLock(t *testing.T) {
	setLockCacheForTest(t)
	config := filepath.Join(t.TempDir(), "config.yml")
	canonical, err := canonicalConfigPath(config)
	if err != nil {
		t.Fatal(err)
	}
	root, err := hostTrustCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canonical))
	lockDir := filepath.Join(root, hex.EncodeToString(sum[:])[:32]+".lock")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireConfigLock(config)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("native lock ignored a live official-wrapper lock")
	}
	if !errors.Is(err, ErrConfigLockHeld) {
		t.Fatalf("AcquireConfigLock error = %v, want ErrConfigLockHeld", err)
	}
}

func TestAcquireConfigLockRecoversStaleSharedWrapperLock(t *testing.T) {
	setLockCacheForTest(t)
	config := filepath.Join(t.TempDir(), "config.yml")
	canonical, err := canonicalConfigPath(config)
	if err != nil {
		t.Fatal(err)
	}
	root, err := hostTrustCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canonical))
	lockDir := filepath.Join(root, hex.EncodeToString(sum[:])[:32]+".lock")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte("2147483647\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireConfigLock(config)
	if err != nil {
		t.Fatalf("AcquireConfigLock with stale shared lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireConfigLockCanonicalizesSymlinks(t *testing.T) {
	setLockCacheForTest(t)
	directory := t.TempDir()
	realConfig := filepath.Join(directory, "config.yml")
	if err := os.WriteFile(realConfig, []byte("image: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias.yml")
	if err := os.Symlink(realConfig, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	first, err := AcquireConfigLock(realConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireConfigLock(alias)
	if second != nil {
		_ = second.Close()
		t.Fatal("symlink alias acquired a distinct config lock")
	}
	if !errors.Is(err, ErrConfigLockHeld) {
		t.Fatalf("symlink alias error = %v, want ErrConfigLockHeld", err)
	}
}

func setLockCacheForTest(t *testing.T) {
	t.Helper()
	cache := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", cache)
	case "darwin":
		t.Setenv("HOME", cache)
	default:
		t.Setenv("XDG_CACHE_HOME", cache)
	}
}
