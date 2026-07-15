package logging

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
	"sort"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	"gopkg.in/natefinch/lumberjack.v2"
)

const controlDirectoryName = ".epar-control"

var (
	registryMu sync.Mutex
	registry   = make(map[string]*rotationEntry)
)

type rotationEntry struct {
	path         string
	root         string
	rotation     Rotation
	writer       *lumberjack.Logger
	lock         *filelock.Lock
	lockPath     string
	metadataPath string
	ownerToken   string
	startedAt    time.Time
	refs         int
	sessions     map[string]TranscriptMetadata
	writeMu      sync.Mutex
}

type rotationHandle struct {
	entry  *rotationEntry
	id     string
	mu     sync.Mutex
	close  error
	closed bool
}

type activeState struct {
	Version    int                  `json:"version"`
	Path       string               `json:"path"`
	PID        int                  `json:"pid"`
	StartedAt  time.Time            `json:"startedAt"`
	OwnerToken string               `json:"ownerToken"`
	Sessions   []TranscriptMetadata `json:"sessions"`
}

func openRotating(root, path string, rotation Rotation, metadata TranscriptMetadata, now time.Time) (*rotationHandle, error) {
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return nil, fmt.Errorf("canonicalize logging root: %w", err)
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, fmt.Errorf("canonicalize log path: %w", err)
	}
	if !pathWithin(canonicalRoot, canonical) {
		return nil, fmt.Errorf("log path %s is outside logging root %s", canonical, canonicalRoot)
	}
	if info, statErr := os.Lstat(canonical); statErr == nil && isLinkOrReparse(info) {
		return nil, fmt.Errorf("refuse symlink or reparse-point log path %s", canonical)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect log path %s: %w", canonical, statErr)
	}
	if err := ensureSafeDirectory(filepath.Dir(canonical), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	handleID, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("create log handle token: %w", err)
	}
	if metadata.SessionID == "" {
		metadata.SessionID = handleID
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if entry := registry[canonical]; entry != nil {
		if entry.rotation != rotation {
			return nil, fmt.Errorf("log path %s is already open with different rotation settings", canonical)
		}
		entry.refs++
		entry.sessions[handleID] = cloneMetadata(metadata)
		if err := writeActiveState(entry); err != nil {
			delete(entry.sessions, handleID)
			entry.refs--
			return nil, err
		}
		return &rotationHandle{entry: entry, id: handleID}, nil
	}

	control := filepath.Join(canonicalRoot, controlDirectoryName)
	lockDir := filepath.Join(control, "locks")
	activeDir := filepath.Join(control, "active")
	if err := ensureSafeDirectory(control, 0o700); err != nil {
		return nil, fmt.Errorf("create log control directory: %w", err)
	}
	if err := ensureSafeDirectory(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log lock directory: %w", err)
	}
	if err := ensureSafeDirectory(activeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create active log directory: %w", err)
	}
	hash := pathHash(canonical)
	lockPath := filepath.Join(lockDir, hash+".lock")
	lock, err := filelock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("log path is active in another process: %w", err)
		}
		return nil, err
	}
	probe, err := os.OpenFile(canonical, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("initialize rotating log file %s: %w", canonical, err)
	}
	if err := probe.Close(); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("close initialized rotating log file %s: %w", canonical, err)
	}
	ownerToken, err := randomToken()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("create active log owner token: %w", err)
	}
	entry := &rotationEntry{
		path:         canonical,
		root:         canonicalRoot,
		rotation:     rotation,
		writer:       &lumberjack.Logger{Filename: canonical, MaxSize: rotation.MaxSizeMiB, MaxBackups: rotation.MaxBackups, Compress: rotation.Compress, LocalTime: false},
		lock:         lock,
		lockPath:     lockPath,
		metadataPath: filepath.Join(activeDir, hash+".json"),
		ownerToken:   ownerToken,
		startedAt:    now.UTC(),
		refs:         1,
		sessions:     map[string]TranscriptMetadata{handleID: cloneMetadata(metadata)},
	}
	if err := writeActiveState(entry); err != nil {
		_ = lock.Close()
		return nil, err
	}
	registry[canonical] = entry
	return &rotationHandle{entry: entry, id: handleID}, nil
}

func (handle *rotationHandle) Write(data []byte) (int, error) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return 0, os.ErrClosed
	}
	handle.entry.writeMu.Lock()
	defer handle.entry.writeMu.Unlock()
	return handle.entry.writer.Write(data)
}

func (handle *rotationHandle) Close() error {
	if handle == nil {
		return nil
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return handle.close
	}
	handle.closed = true

	registryMu.Lock()
	defer registryMu.Unlock()
	entry := handle.entry
	delete(entry.sessions, handle.id)
	entry.refs--
	if entry.refs > 0 {
		handle.close = writeActiveState(entry)
		return handle.close
	}
	delete(registry, entry.path)
	entry.writeMu.Lock()
	writerErr := entry.writer.Close()
	entry.writeMu.Unlock()
	metadataErr := removeOwnedActiveState(entry)
	lockErr := entry.lock.Close()
	handle.close = errors.Join(writerErr, metadataErr, lockErr)
	return handle.close
}

func writeActiveState(entry *rotationEntry) error {
	sessionKeys := make([]string, 0, len(entry.sessions))
	for key := range entry.sessions {
		sessionKeys = append(sessionKeys, key)
	}
	sort.Strings(sessionKeys)
	sessions := make([]TranscriptMetadata, 0, len(sessionKeys))
	for _, key := range sessionKeys {
		sessions = append(sessions, cloneMetadata(entry.sessions[key]))
	}
	state := activeState{Version: 1, Path: entry.path, PID: os.Getpid(), StartedAt: entry.startedAt, OwnerToken: entry.ownerToken, Sessions: sessions}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active log metadata: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(entry.metadataPath), ".active-*.tmp")
	if err != nil {
		return fmt.Errorf("create active log metadata: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure active log metadata: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write active log metadata: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync active log metadata: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close active log metadata: %w", err)
	}
	if err := replaceFile(temporaryPath, entry.metadataPath); err != nil {
		return fmt.Errorf("publish active log metadata: %w", err)
	}
	return nil
}

func removeOwnedActiveState(entry *rotationEntry) error {
	data, err := os.ReadFile(entry.metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read active log metadata during close: %w", err)
	}
	var state activeState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode active log metadata during close: %w", err)
	}
	if state.OwnerToken != entry.ownerToken {
		return fmt.Errorf("active log metadata owner changed for %s", entry.path)
	}
	if err := os.Remove(entry.metadataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove active log metadata: %w", err)
	}
	return nil
}

func activeRegistryPaths() map[string]struct{} {
	registryMu.Lock()
	defer registryMu.Unlock()
	paths := make(map[string]struct{}, len(registry))
	for path := range registry {
		paths[path] = struct{}{}
	}
	return paths
}

func pathHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func cloneMetadata(metadata TranscriptMetadata) TranscriptMetadata {
	copy := metadata
	if metadata.Attributes != nil {
		copy.Attributes = make(map[string]string, len(metadata.Attributes))
		for key, value := range metadata.Attributes {
			copy.Attributes[key] = value
		}
	}
	return copy
}

func ensureSafeDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || isLinkOrReparse(info) {
		return fmt.Errorf("%s is not a safe directory", path)
	}
	return nil
}
