// Package catalog persists exact, per-user EPAR resource custody across
// projects and configuration files.
package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
)

const SchemaVersion = 1

type Custody string

const (
	CustodyGenerated Custody = "generated"
	CustodyAcquired  Custody = "acquired"
)

type State string

const (
	StateCurrent        State = "current"
	StateStaging        State = "staging"
	StateSuperseded     State = "superseded"
	StateCleanupPending State = "cleanup-pending"
)

type Reference struct {
	ConfigID     string    `json:"configId"`
	ManifestHash string    `json:"manifestHash,omitempty"`
	Role         string    `json:"role,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Resource struct {
	Key             string      `json:"key"`
	BackendID       string      `json:"backendId"`
	InstallationIDs []string    `json:"installationIds,omitempty"`
	Kind            string      `json:"kind"`
	Provider        string      `json:"provider,omitempty"`
	Role            string      `json:"role,omitempty"`
	Locator         string      `json:"locator"`
	Identity        string      `json:"identity"`
	Fingerprint     string      `json:"fingerprint,omitempty"`
	Custody         Custody     `json:"custody"`
	ManifestHash    string      `json:"manifestHash,omitempty"`
	IntroducedTags  []string    `json:"introducedTags,omitempty"`
	State           State       `json:"state"`
	References      []Reference `json:"references,omitempty"`
	CreatedAt       time.Time   `json:"createdAt"`
	LastSeenAt      time.Time   `json:"lastSeenAt"`
	SupersededAt    *time.Time  `json:"supersededAt,omitempty"`
	LeaseExpiresAt  *time.Time  `json:"leaseExpiresAt,omitempty"`
	CleanupError    string      `json:"cleanupError,omitempty"`
}

type Config struct {
	ID                   string     `json:"id"`
	InstallationID       string     `json:"installationId"`
	Path                 string     `json:"path"`
	ProjectRoot          string     `json:"projectRoot"`
	BuildCacheLimitBytes uint64     `json:"buildCacheLimitBytes,omitempty"`
	ControllerLeaseUntil *time.Time `json:"controllerLeaseUntil,omitempty"`
	LastSeenAt           time.Time  `json:"lastSeenAt"`
}

type Journal struct {
	ID               string    `json:"id"`
	Operation        string    `json:"operation"`
	ResourceKey      string    `json:"resourceKey,omitempty"`
	BackendID        string    `json:"backendId,omitempty"`
	ConfigID         string    `json:"configId,omitempty"`
	Role             string    `json:"role,omitempty"`
	Locator          string    `json:"locator,omitempty"`
	PreviousIdentity string    `json:"previousIdentity,omitempty"`
	Phase            string    `json:"phase"`
	StartedAt        time.Time `json:"startedAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Error            string    `json:"error,omitempty"`
}

type Catalog struct {
	SchemaVersion  int        `json:"schemaVersion"`
	InstallationID string     `json:"installationId"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	Configs        []Config   `json:"configs,omitempty"`
	Resources      []Resource `json:"resources,omitempty"`
	Journals       []Journal  `json:"journals,omitempty"`
}

type Store struct {
	root string
}

func DefaultRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv("EPAR_STATE_HOME")); override != "" {
		return filepath.Abs(override)
	}
	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		if base == "" {
			return "", errors.New("LOCALAPPDATA is required for the EPAR host resource catalog")
		}
		return filepath.Join(base, "ephemeral-action-runner", "state"), nil
	}
	if runtime.GOOS == "linux" {
		if base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); base != "" {
			return filepath.Join(base, "ephemeral-action-runner"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for EPAR host resource catalog: %w", err)
		}
		return filepath.Join(home, ".local", "state", "ephemeral-action-runner"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user state directory for EPAR host resource catalog: %w", err)
	}
	return filepath.Join(base, "ephemeral-action-runner", "state"), nil
}

func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve EPAR host resource catalog root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create EPAR host resource catalog root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("EPAR host resource catalog root must be a real directory: %s", absolute)
	}
	return &Store{root: absolute}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Path() string { return filepath.Join(s.root, "resources-v1.json") }

func (s *Store) AcquireBackendLock(ctx context.Context, backendID string) (*filelock.Lock, error) {
	if strings.TrimSpace(backendID) == "" {
		return nil, errors.New("backend identity is required")
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(backendID)))
	path := filepath.Join(s.root, "backend-"+hex.EncodeToString(sum[:12])+".lock")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		lock, err := filelock.Acquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, fmt.Errorf("acquire EPAR backend lock for %s: %w", backendID, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for EPAR backend lock for %s: %w", backendID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Store) WithLock(now time.Time, update func(*Catalog) error) (Catalog, error) {
	if update == nil {
		return Catalog{}, errors.New("catalog update function is required")
	}
	lock, err := filelock.Acquire(filepath.Join(s.root, "resources-v1.lock"))
	if err != nil {
		return Catalog{}, fmt.Errorf("acquire EPAR host resource catalog lock: %w", err)
	}
	defer lock.Close()
	value, err := s.loadUnlocked(now)
	if err != nil {
		return Catalog{}, err
	}
	if err := update(&value); err != nil {
		return Catalog{}, err
	}
	normalize(&value)
	value.UpdatedAt = now.UTC()
	if err := s.writeUnlocked(value); err != nil {
		return Catalog{}, err
	}
	return value, nil
}

func (s *Store) Load(now time.Time) (Catalog, error) {
	lock, err := filelock.Acquire(filepath.Join(s.root, "resources-v1.lock"))
	if err != nil {
		return Catalog{}, fmt.Errorf("acquire EPAR host resource catalog lock: %w", err)
	}
	defer lock.Close()
	return s.loadUnlocked(now)
}

func (s *Store) loadUnlocked(now time.Time) (Catalog, error) {
	content, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		installationID, idErr := randomID()
		if idErr != nil {
			return Catalog{}, idErr
		}
		return Catalog{SchemaVersion: SchemaVersion, InstallationID: installationID, UpdatedAt: now.UTC()}, nil
	}
	if err != nil {
		return Catalog{}, fmt.Errorf("read EPAR host resource catalog: %w", err)
	}
	var value Catalog
	if err := json.Unmarshal(content, &value); err != nil {
		return Catalog{}, fmt.Errorf("decode EPAR host resource catalog: %w", err)
	}
	if value.SchemaVersion != SchemaVersion || strings.TrimSpace(value.InstallationID) == "" {
		return Catalog{}, fmt.Errorf("unsupported or incomplete EPAR host resource catalog schema %d", value.SchemaVersion)
	}
	normalize(&value)
	return value, nil
}

func (s *Store) writeUnlocked(value Catalog) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode EPAR host resource catalog: %w", err)
	}
	temp, err := os.CreateTemp(s.root, ".resources-v1-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(content, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, s.Path()); err != nil {
		return fmt.Errorf("publish EPAR host resource catalog: %w", err)
	}
	return nil
}

func ConfigID(projectRoot, configPath string) (string, error) {
	root, err := canonicalPath(projectRoot)
	if err != nil {
		return "", err
	}
	path, err := canonicalPath(configPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root + "\x00" + path))
	return hex.EncodeToString(sum[:12]), nil
}

func ResourceKey(backendID, kind, identity string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(backendID) + "\x00" + strings.TrimSpace(kind) + "\x00" + strings.TrimSpace(identity)))
	return hex.EncodeToString(sum[:16])
}

func RegisterConfig(value *Catalog, projectRoot, configPath string, now time.Time) (Config, error) {
	id, err := ConfigID(projectRoot, configPath)
	if err != nil {
		return Config{}, err
	}
	root, err := canonicalPath(projectRoot)
	if err != nil {
		return Config{}, err
	}
	path, err := canonicalPath(configPath)
	if err != nil {
		return Config{}, err
	}
	installationSum := sha256.Sum256([]byte(value.InstallationID + "\x00" + root))
	record := Config{ID: id, InstallationID: hex.EncodeToString(installationSum[:12]), Path: path, ProjectRoot: root, LastSeenAt: now.UTC()}
	for index := range value.Configs {
		if value.Configs[index].ID == id {
			if value.Configs[index].InstallationID != "" {
				record.InstallationID = value.Configs[index].InstallationID
			}
			record.BuildCacheLimitBytes = value.Configs[index].BuildCacheLimitBytes
			record.ControllerLeaseUntil = value.Configs[index].ControllerLeaseUntil
			value.Configs[index] = record
			return record, nil
		}
	}
	value.Configs = append(value.Configs, record)
	return record, nil
}

func RefreshControllerLease(value *Catalog, configID string, expiresAt time.Time) error {
	if expiresAt.IsZero() {
		return errors.New("controller lease expiry is required")
	}
	for index := range value.Configs {
		if value.Configs[index].ID == configID {
			expiry := expiresAt.UTC()
			value.Configs[index].ControllerLeaseUntil = &expiry
			return nil
		}
	}
	return fmt.Errorf("catalog configuration %s is not registered", configID)
}

func ReleaseControllerLease(value *Catalog, configID string) {
	for index := range value.Configs {
		if value.Configs[index].ID == configID {
			value.Configs[index].ControllerLeaseUntil = nil
			return
		}
	}
}

func UpsertResource(value *Catalog, resource Resource) error {
	if resource.BackendID == "" || resource.Kind == "" || resource.Identity == "" || resource.Locator == "" {
		return errors.New("catalog resource backend, kind, identity, and locator are required")
	}
	if resource.Custody != CustodyGenerated && resource.Custody != CustodyAcquired {
		return fmt.Errorf("unsupported catalog custody %q", resource.Custody)
	}
	if resource.Key == "" {
		resource.Key = ResourceKey(resource.BackendID, resource.Kind, resource.Identity)
	}
	for index := range value.Resources {
		if value.Resources[index].Key == resource.Key {
			resource.InstallationIDs = mergeStrings(value.Resources[index].InstallationIDs, resource.InstallationIDs)
			resource.CreatedAt = value.Resources[index].CreatedAt
			if resource.CreatedAt.IsZero() {
				resource.CreatedAt = time.Now().UTC()
			}
			value.Resources[index] = resource
			return nil
		}
	}
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = time.Now().UTC()
	}
	value.Resources = append(value.Resources, resource)
	return nil
}

func mergeStrings(groups ...[]string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func ReplaceConfigReferences(value *Catalog, configID string, references map[string]Reference, now time.Time) {
	ReplaceConfigRoleReferences(value, configID, "", references, now)
}

// ReplaceConfigRoleReferences atomically replaces one config's references for
// a single logical role without disturbing its other provider or bootstrap
// resources. An empty role preserves the original all-reference behavior.
func ReplaceConfigRoleReferences(value *Catalog, configID, role string, references map[string]Reference, now time.Time) {
	for index := range value.Resources {
		resource := &value.Resources[index]
		filtered := resource.References[:0]
		for _, reference := range resource.References {
			if reference.ConfigID != configID || (role != "" && reference.Role != role) {
				filtered = append(filtered, reference)
			}
		}
		resource.References = filtered
		if reference, found := references[resource.Key]; found {
			reference.ConfigID = configID
			if role != "" {
				reference.Role = role
			}
			reference.UpdatedAt = now.UTC()
			resource.References = append(resource.References, reference)
			resource.State = StateCurrent
			resource.SupersededAt = nil
			resource.CleanupError = ""
		} else if len(resource.References) == 0 && resource.State == StateCurrent {
			when := now.UTC()
			resource.State = StateSuperseded
			resource.SupersededAt = &when
		}
	}
}

// Compact removes references to missing configurations without a live lease,
// drops missing resources through the supplied exact observer, and discards
// completed journals. Observer errors preserve the resource fail-closed.
func Compact(value *Catalog, now time.Time, exists func(Resource) (bool, error)) []string {
	var warnings []string
	liveConfigs := make(map[string]bool)
	configs := value.Configs[:0]
	for _, config := range value.Configs {
		configPresent := false
		if info, err := os.Lstat(config.Path); err == nil && info.Mode().IsRegular() {
			configPresent = true
		}
		leaseActive := config.ControllerLeaseUntil != nil && config.ControllerLeaseUntil.After(now)
		if configPresent || leaseActive {
			liveConfigs[config.ID] = true
			configs = append(configs, config)
		}
	}
	value.Configs = configs
	resources := value.Resources[:0]
	for _, resource := range value.Resources {
		references := resource.References[:0]
		for _, reference := range resource.References {
			if liveConfigs[reference.ConfigID] {
				references = append(references, reference)
			}
		}
		resource.References = references
		present, err := exists(resource)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("catalog resource %s could not be observed: %v", resource.Key, err))
			resources = append(resources, resource)
			continue
		}
		if !present {
			continue
		}
		resource.LastSeenAt = now.UTC()
		if len(resource.References) == 0 && resource.State == StateCurrent {
			when := now.UTC()
			resource.State = StateSuperseded
			resource.SupersededAt = &when
		}
		resources = append(resources, resource)
	}
	value.Resources = resources
	resourceKeys := make(map[string]bool, len(value.Resources))
	for _, resource := range value.Resources {
		resourceKeys[resource.Key] = true
	}
	journals := value.Journals[:0]
	for _, journal := range value.Journals {
		if journal.Phase != "complete" && (journal.ResourceKey == "" || resourceKeys[journal.ResourceKey]) {
			journals = append(journals, journal)
		}
	}
	value.Journals = journals
	return warnings
}

func normalize(value *Catalog) {
	sort.Slice(value.Configs, func(i, j int) bool { return value.Configs[i].ID < value.Configs[j].ID })
	sort.Slice(value.Resources, func(i, j int) bool { return value.Resources[i].Key < value.Resources[j].Key })
	for index := range value.Resources {
		resource := &value.Resources[index]
		sort.Strings(resource.InstallationIDs)
		sort.Strings(resource.IntroducedTags)
		sort.Slice(resource.References, func(i, j int) bool { return resource.References[i].ConfigID < resource.References[j].ConfigID })
	}
	sort.Slice(value.Journals, func(i, j int) bool { return value.Journals[i].ID < value.Journals[j].ID })
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
}

func randomID() (string, error) {
	content := make([]byte, 16)
	if _, err := rand.Read(content); err != nil {
		return "", fmt.Errorf("generate EPAR installation identity: %w", err)
	}
	return hex.EncodeToString(content), nil
}
