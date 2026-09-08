package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const (
	controlPlaneRecoveryStateFilename = "control-plane-recovery-v1.json"
	controlPlaneRecoveryStateLock     = ".control-plane-recovery-state.lock"
	controlPlaneRecoveryStateVersion  = 1
	controlPlaneRecoveryIdentityLimit = 4096
	controlPlaneRecoveryIdentityBytes = 128
)

// RecoveryReservationPhase describes which part of a provider recovery may
// have crossed the external daemon boundary. It is durable so a restarted
// controller can take over a reservation that never began, while treating an
// interrupted daemon mutation as verification-only.
type RecoveryReservationPhase string

const (
	RecoveryReservationNone        RecoveryReservationPhase = ""
	RecoveryReservationReserved    RecoveryReservationPhase = "reserved"
	RecoveryReservationIntervening RecoveryReservationPhase = "intervening"
	RecoveryReservationVerifying   RecoveryReservationPhase = "verifying"
)

// ControlPlaneRecoveryState is the durable, host-scoped recovery budget for a
// provider control plane. It deliberately contains policy state only; command
// output and credentials never belong in this file.
type ControlPlaneRecoveryState struct {
	Attempts                     int                      `json:"attempts,omitempty"`
	NextAttemptAt                time.Time                `json:"nextAttemptAt,omitempty"`
	AdmissionIncident            bool                     `json:"admissionIncident,omitempty"`
	RecoveryReservationToken     uint64                   `json:"recoveryReservationToken,omitempty"`
	AdmissionIncidentToken       uint64                   `json:"admissionIncidentToken,omitempty"`
	RecoveryReservationPhase     RecoveryReservationPhase `json:"recoveryReservationPhase,omitempty"`
	RecoveryReservationExpiresAt time.Time                `json:"recoveryReservationExpiresAt,omitempty"`
	// ExpectedIdentities is the exact host-wide immutable name-to-provider-ID
	// census captured before a recovery reservation may cross the provider
	// boundary. An empty, non-nil map is a valid census of an empty daemon;
	// nil identifies a compatible pre-census ledger shape.
	ExpectedIdentities map[string]string `json:"expectedIdentities"`
	// Generation is the durable snapshot version returned to callers for
	// conditional completion. It is metadata in the enclosing disk record and
	// is intentionally excluded from the serialized policy state and checksum.
	Generation uint64 `json:"-"`
}

type controlPlaneRecoveryDiskState struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Generation    uint64                    `json:"generation"`
	State         ControlPlaneRecoveryState `json:"state"`
	Checksum      string                    `json:"checksum"`
}

type controlPlaneRecoveryChecksumState struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Generation    uint64                    `json:"generation"`
	State         ControlPlaneRecoveryState `json:"state"`
}

// controlPlaneRecoveryPreCensusState is the schema-1 state shape written
// after reservation expiry became durable but before the host-wide exact
// identity census was added. Its checksum remains accepted only for that
// exact JSON shape.
type controlPlaneRecoveryPreCensusState struct {
	Attempts                     int                      `json:"attempts,omitempty"`
	NextAttemptAt                time.Time                `json:"nextAttemptAt,omitempty"`
	AdmissionIncident            bool                     `json:"admissionIncident,omitempty"`
	RecoveryReservationToken     uint64                   `json:"recoveryReservationToken,omitempty"`
	AdmissionIncidentToken       uint64                   `json:"admissionIncidentToken,omitempty"`
	RecoveryReservationPhase     RecoveryReservationPhase `json:"recoveryReservationPhase,omitempty"`
	RecoveryReservationExpiresAt time.Time                `json:"recoveryReservationExpiresAt,omitempty"`
}

type controlPlaneRecoveryPreCensusChecksumState struct {
	SchemaVersion int                                `json:"schemaVersion"`
	Generation    uint64                             `json:"generation"`
	State         controlPlaneRecoveryPreCensusState `json:"state"`
}

// controlPlaneRecoveryLegacyState is the schema-1 state shape written before
// RecoveryReservationExpiresAt was added. RecoveryReservationPhase was
// already durable in that shape and must remain part of its checksum. Because
// time.Time is a struct, adding the expiry field changed the checksum even
// when it was zero. Accepting this exact older checksum lets an interrupted
// upgrade recover its existing retry budget; the next successful mutation
// rewrites the canonical current shape.
type controlPlaneRecoveryLegacyState struct {
	Attempts                 int                      `json:"attempts,omitempty"`
	NextAttemptAt            time.Time                `json:"nextAttemptAt,omitempty"`
	AdmissionIncident        bool                     `json:"admissionIncident,omitempty"`
	RecoveryReservationToken uint64                   `json:"recoveryReservationToken,omitempty"`
	AdmissionIncidentToken   uint64                   `json:"admissionIncidentToken,omitempty"`
	RecoveryReservationPhase RecoveryReservationPhase `json:"recoveryReservationPhase,omitempty"`
}

type controlPlaneRecoveryLegacyChecksumState struct {
	SchemaVersion int                             `json:"schemaVersion"`
	Generation    uint64                          `json:"generation"`
	State         controlPlaneRecoveryLegacyState `json:"state"`
}

type controlPlaneRecoveryJSONShape uint8

const (
	controlPlaneRecoveryJSONShapeLegacy controlPlaneRecoveryJSONShape = iota
	controlPlaneRecoveryJSONShapePreCensus
	controlPlaneRecoveryJSONShapeCurrent
)

// ControlPlaneRecoveryLedger is a checksummed, atomically replaced state file
// serialized across cooperating EPAR controller processes. It is separate
// from per-pool lifecycle state because one Docker Sandboxes daemon may serve
// multiple pools and configurations.
type ControlPlaneRecoveryLedger struct {
	directory string
	path      string
	lockPath  string
	mu        sync.Mutex
	now       func() time.Time
	fault     func(string) error // test-only crash-boundary injection
	unusable  error              // post-publication durability failure; fail closed
}

// OpenControlPlaneRecoveryLedger opens the host-scoped ledger next to the
// Docker Sandboxes control-plane lock. A corrupt or unsupported snapshot is
// returned as an error; callers must not reinterpret it as an empty budget.
func OpenControlPlaneRecoveryLedger() (*ControlPlaneRecoveryLedger, error) {
	root, err := storagecatalog.DefaultRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve EPAR state root for Docker Sandboxes recovery ledger: %w", err)
	}
	directory := filepath.Join(root, controlPlaneLockDirectory)
	if err := ensureControlPlaneRecoveryDirectory(directory); err != nil {
		return nil, fmt.Errorf("prepare Docker Sandboxes recovery ledger directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker Sandboxes recovery ledger directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Docker Sandboxes recovery ledger directory must be a real directory: %s", directory)
	}
	ledger := &ControlPlaneRecoveryLedger{
		directory: directory,
		path:      filepath.Join(directory, controlPlaneRecoveryStateFilename),
		lockPath:  filepath.Join(directory, controlPlaneRecoveryStateLock),
		now:       time.Now,
	}
	if err := ledger.withLock(context.Background(), func() error { _, err := ledger.load(); return err }); err != nil {
		return nil, err
	}
	return ledger, nil
}

func ensureControlPlaneRecoveryDirectory(directory string) error {
	missing, err := missingControlPlaneRecoveryDirectories(directory)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		// Sync each newly created directory's parent from the top down so the
		// directory entries themselves survive a host crash before the ledger
		// file is ever published.
		for index := len(missing) - 1; index >= 0; index-- {
			if err := prepareControlPlaneRecoveryDirectory(filepath.Dir(missing[index])); err != nil {
				return fmt.Errorf("sync parent directory: %w", err)
			}
		}
	}
	return prepareControlPlaneRecoveryDirectory(directory)
}

func missingControlPlaneRecoveryDirectories(directory string) ([]string, error) {
	var missing []string
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("directory must be a real directory: %s", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect directory %s: %w", current, err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return missing, nil
}

func (ledger *ControlPlaneRecoveryLedger) Path() string {
	if ledger == nil {
		return ""
	}
	return ledger.path
}

// State reads the latest durable recovery budget.
func (ledger *ControlPlaneRecoveryLedger) State(ctx context.Context) (ControlPlaneRecoveryState, error) {
	if ledger == nil {
		return ControlPlaneRecoveryState{}, errors.New("Docker Sandboxes recovery ledger is nil")
	}
	var result ControlPlaneRecoveryState
	err := ledger.withLock(ctx, func() error {
		state, err := ledger.load()
		if err != nil {
			return err
		}
		result = cloneControlPlaneRecoveryState(state.State)
		return nil
	})
	return result, err
}

// Update applies one short, atomic policy-state mutation. The callback runs
// while the ledger lock is held, but must never perform provider operations.
func (ledger *ControlPlaneRecoveryLedger) Update(ctx context.Context, mutate func(*ControlPlaneRecoveryState) error) (ControlPlaneRecoveryState, error) {
	if ledger == nil {
		return ControlPlaneRecoveryState{}, errors.New("Docker Sandboxes recovery ledger is nil")
	}
	if mutate == nil {
		return ControlPlaneRecoveryState{}, errors.New("Docker Sandboxes recovery ledger mutation is nil")
	}
	var result ControlPlaneRecoveryState
	err := ledger.withLock(ctx, func() error {
		disk, err := ledger.load()
		if err != nil {
			return err
		}
		candidate := cloneControlPlaneRecoveryState(disk.State)
		if err := mutate(&candidate); err != nil {
			return err
		}
		if err := canonicalizeControlPlaneRecoveryState(disk.State, &candidate); err != nil {
			return err
		}
		if err := validateControlPlaneRecoveryState(candidate); err != nil {
			return err
		}
		disk.Generation++
		candidate.Generation = disk.Generation
		disk.State = candidate
		if err := ledger.save(disk); err != nil {
			return err
		}
		result = cloneControlPlaneRecoveryState(candidate)
		return nil
	})
	return result, err
}

// UpdateIfGeneration applies a mutation only when the caller still owns the
// exact snapshot it observed. The check and mutation happen under the same
// cross-process lock, so a stale controller cannot erase a newer cooldown or
// admission incident.
func (ledger *ControlPlaneRecoveryLedger) UpdateIfGeneration(ctx context.Context, expected uint64, mutate func(*ControlPlaneRecoveryState) error) (state ControlPlaneRecoveryState, applied bool, err error) {
	return ledger.UpdateIf(ctx, func(current ControlPlaneRecoveryState) bool {
		return current.Generation == expected
	}, mutate)
}

// UpdateIf applies a mutation only when matches accepts the latest durable
// state. The predicate, mutation, and atomic publication are one locked
// operation, allowing independent recovery and admission tokens to coexist
// without a stale manager overwriting an unrelated update.
func (ledger *ControlPlaneRecoveryLedger) UpdateIf(ctx context.Context, matches func(ControlPlaneRecoveryState) bool, mutate func(*ControlPlaneRecoveryState) error) (state ControlPlaneRecoveryState, applied bool, err error) {
	if ledger == nil {
		return ControlPlaneRecoveryState{}, false, errors.New("Docker Sandboxes recovery ledger is nil")
	}
	if matches == nil {
		return ControlPlaneRecoveryState{}, false, errors.New("Docker Sandboxes recovery ledger predicate is nil")
	}
	if mutate == nil {
		return ControlPlaneRecoveryState{}, false, errors.New("Docker Sandboxes recovery ledger mutation is nil")
	}
	err = ledger.withLock(ctx, func() error {
		disk, loadErr := ledger.load()
		if loadErr != nil {
			return loadErr
		}
		if !matches(cloneControlPlaneRecoveryState(disk.State)) {
			state = cloneControlPlaneRecoveryState(disk.State)
			return nil
		}
		candidate := cloneControlPlaneRecoveryState(disk.State)
		if mutateErr := mutate(&candidate); mutateErr != nil {
			return mutateErr
		}
		if canonicalizeErr := canonicalizeControlPlaneRecoveryState(disk.State, &candidate); canonicalizeErr != nil {
			return canonicalizeErr
		}
		if validateErr := validateControlPlaneRecoveryState(candidate); validateErr != nil {
			return validateErr
		}
		disk.Generation++
		candidate.Generation = disk.Generation
		disk.State = candidate
		if saveErr := ledger.save(disk); saveErr != nil {
			return saveErr
		}
		state = cloneControlPlaneRecoveryState(candidate)
		applied = true
		return nil
	})
	return state, applied, err
}

func (ledger *ControlPlaneRecoveryLedger) withLock(ctx context.Context, operation func() error) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.unusable != nil {
		return ledger.unusable
	}
	for {
		lock, err := filelock.Acquire(ledger.lockPath)
		if err == nil {
			defer lock.Close()
			return operation()
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (ledger *ControlPlaneRecoveryLedger) load() (controlPlaneRecoveryDiskState, error) {
	data, err := os.ReadFile(ledger.path)
	if os.IsNotExist(err) {
		return controlPlaneRecoveryDiskState{SchemaVersion: controlPlaneRecoveryStateVersion}, nil
	}
	if err != nil {
		return controlPlaneRecoveryDiskState{}, fmt.Errorf("read Docker Sandboxes recovery ledger: %w", err)
	}
	shape, err := validateControlPlaneRecoveryJSONShape(data)
	if err != nil {
		return controlPlaneRecoveryDiskState{}, fmt.Errorf("Docker Sandboxes recovery ledger is corrupt: %w", err)
	}
	var state controlPlaneRecoveryDiskState
	if err := json.Unmarshal(data, &state); err != nil {
		return controlPlaneRecoveryDiskState{}, fmt.Errorf("Docker Sandboxes recovery ledger is corrupt: decode: %v", err)
	}
	if state.SchemaVersion != controlPlaneRecoveryStateVersion {
		return controlPlaneRecoveryDiskState{}, fmt.Errorf("Docker Sandboxes recovery ledger is corrupt: schema %d", state.SchemaVersion)
	}
	checksumMatches := false
	switch shape {
	case controlPlaneRecoveryJSONShapeLegacy:
		sum, checksumErr := controlPlaneRecoveryLegacyChecksum(state)
		checksumMatches = checksumErr == nil && state.Checksum == hex.EncodeToString(sum)
	case controlPlaneRecoveryJSONShapePreCensus:
		sum, checksumErr := controlPlaneRecoveryPreCensusChecksum(state)
		checksumMatches = checksumErr == nil && state.Checksum == hex.EncodeToString(sum)
	case controlPlaneRecoveryJSONShapeCurrent:
		sum, checksumErr := controlPlaneRecoveryChecksum(state)
		checksumMatches = checksumErr == nil && state.Checksum == hex.EncodeToString(sum)
	}
	if !checksumMatches {
		return controlPlaneRecoveryDiskState{}, errors.New("Docker Sandboxes recovery ledger is corrupt: checksum")
	}
	if shape == controlPlaneRecoveryJSONShapeLegacy && state.State.RecoveryReservationToken != 0 {
		// A still-active reservation used NextAttemptAt as its takeover deadline
		// before the dedicated expiry field existed. Only an already durable phase
		// can distinguish a reservation that had not crossed the daemon boundary
		// from one whose older writer crashed after intervention. A phase-less
		// active reservation is therefore ambiguous and must fail closed rather
		// than being upgraded into authority for another restart.
		if state.State.RecoveryReservationPhase == RecoveryReservationNone {
			return controlPlaneRecoveryDiskState{}, errors.New("Docker Sandboxes recovery ledger is corrupt: legacy active recovery reservation has no durable phase and is ambiguous")
		}
		state.State.RecoveryReservationExpiresAt = state.State.NextAttemptAt
	}
	if err := validateControlPlaneRecoveryState(state.State); err != nil {
		return controlPlaneRecoveryDiskState{}, fmt.Errorf("Docker Sandboxes recovery ledger is corrupt: %w", err)
	}
	state.State.Generation = state.Generation
	return state, nil
}

// validateControlPlaneRecoveryJSONShape validates the object keys before the
// typed decoder can discard unknown fields, accept case aliases, or collapse
// duplicate keys. It returns whether the nested state has the exact legacy
// shape, which is the only shape eligible for the legacy checksum.
func validateControlPlaneRecoveryJSONShape(data []byte) (controlPlaneRecoveryJSONShape, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return 0, fmt.Errorf("decode envelope: %v", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return 0, errors.New("envelope must be an object")
	}

	seen := make(map[string]struct{}, 4)
	var nestedState json.RawMessage
	nestedStatePresent := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return 0, fmt.Errorf("decode envelope field: %v", err)
		}
		field, ok := token.(string)
		if !ok {
			return 0, errors.New("envelope field name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return 0, fmt.Errorf("duplicate envelope field %q", field)
		}
		seen[field] = struct{}{}
		switch field {
		case "schemaVersion", "generation", "state", "checksum":
		default:
			return 0, fmt.Errorf("unsupported envelope field %q", field)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return 0, fmt.Errorf("decode envelope field %q: %v", field, err)
		}
		if field == "state" {
			nestedState = value
			nestedStatePresent = true
		}
	}
	if token, err := decoder.Token(); err != nil {
		return 0, fmt.Errorf("close envelope: %v", err)
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return 0, errors.New("envelope must end with an object")
	}
	if err := ensureControlPlaneRecoveryJSONEOF(decoder); err != nil {
		return 0, fmt.Errorf("trailing data after envelope: %v", err)
	}

	for _, field := range []string{"schemaVersion", "generation", "state", "checksum"} {
		if _, present := seen[field]; !present {
			return 0, fmt.Errorf("missing envelope field %q", field)
		}
	}
	if !nestedStatePresent {
		return 0, errors.New("state field is missing")
	}
	shape, err := validateControlPlaneRecoveryStateJSONShape(nestedState)
	if err != nil {
		return 0, err
	}
	return shape, nil
}

func validateControlPlaneRecoveryStateJSONShape(data []byte) (controlPlaneRecoveryJSONShape, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return 0, fmt.Errorf("decode state: %v", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return 0, errors.New("state must be an object")
	}

	seen := make(map[string]struct{}, 8)
	nextAttemptAtPresent := false
	recoveryReservationExpiresAtPresent := false
	expectedIdentitiesPresent := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return 0, fmt.Errorf("decode state field: %v", err)
		}
		field, ok := token.(string)
		if !ok {
			return 0, errors.New("state field name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return 0, fmt.Errorf("duplicate state field %q", field)
		}
		seen[field] = struct{}{}
		switch field {
		case "attempts", "nextAttemptAt", "admissionIncident", "recoveryReservationToken", "admissionIncidentToken", "recoveryReservationPhase":
			if field == "nextAttemptAt" {
				nextAttemptAtPresent = true
			}
		case "recoveryReservationExpiresAt":
			recoveryReservationExpiresAtPresent = true
		case "expectedIdentities":
			expectedIdentitiesPresent = true
		default:
			return 0, fmt.Errorf("unsupported state field %q", field)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return 0, fmt.Errorf("decode state field %q: %v", field, err)
		}
		if field == "expectedIdentities" {
			if err := validateControlPlaneRecoveryExpectedIdentitiesJSONShape(value); err != nil {
				return 0, fmt.Errorf("decode state field %q: %v", field, err)
			}
		}
	}
	if token, err := decoder.Token(); err != nil {
		return 0, fmt.Errorf("close state: %v", err)
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return 0, errors.New("state must end with an object")
	}
	if err := ensureControlPlaneRecoveryJSONEOF(decoder); err != nil {
		return 0, fmt.Errorf("trailing data after state: %v", err)
	}
	if !nextAttemptAtPresent {
		return 0, errors.New("state field \"nextAttemptAt\" is missing")
	}
	if expectedIdentitiesPresent && !recoveryReservationExpiresAtPresent {
		return 0, errors.New("state field \"expectedIdentities\" requires \"recoveryReservationExpiresAt\"")
	}
	switch {
	case expectedIdentitiesPresent:
		return controlPlaneRecoveryJSONShapeCurrent, nil
	case recoveryReservationExpiresAtPresent:
		return controlPlaneRecoveryJSONShapePreCensus, nil
	default:
		return controlPlaneRecoveryJSONShapeLegacy, nil
	}
}

func validateControlPlaneRecoveryExpectedIdentitiesJSONShape(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("expected identities must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("expected identity name must be a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate expected identity name %q", name)
		}
		if len(seen) >= controlPlaneRecoveryIdentityLimit {
			return fmt.Errorf("expected identity census exceeds maximum of %d entries", controlPlaneRecoveryIdentityLimit)
		}
		seen[name] = struct{}{}
		var providerID string
		if err := decoder.Decode(&providerID); err != nil {
			return fmt.Errorf("expected provider id for %q must be a string", name)
		}
	}
	if token, err := decoder.Token(); err != nil {
		return err
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return errors.New("expected identities must end with an object")
	}
	return ensureControlPlaneRecoveryJSONEOF(decoder)
}

func ensureControlPlaneRecoveryJSONEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func (ledger *ControlPlaneRecoveryLedger) save(state controlPlaneRecoveryDiskState) error {
	sum, err := controlPlaneRecoveryChecksum(state)
	if err != nil {
		return err
	}
	state.Checksum = hex.EncodeToString(sum)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := prepareControlPlaneRecoveryDirectory(ledger.directory); err != nil {
		return fmt.Errorf("prepare Docker Sandboxes recovery ledger directory durability: %w", err)
	}
	if ledger.fault != nil {
		if err := ledger.fault("before-temp"); err != nil {
			return err
		}
	}
	file, err := os.CreateTemp(ledger.directory, ".control-plane-recovery-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if ledger.fault != nil {
		if err := ledger.fault("before-rename"); err != nil {
			return err
		}
	}
	if err := replaceControlPlaneRecoveryFile(temp, ledger.path); err != nil {
		return err
	}
	committed = true
	if ledger.fault != nil {
		if err := ledger.fault("before-directory-sync"); err != nil {
			ledger.unusable = fmt.Errorf("Docker Sandboxes recovery ledger became unusable after publication: %w", err)
			return ledger.unusable
		}
	}
	if err := finalizeControlPlaneRecoveryDirectory(ledger.directory); err != nil {
		ledger.unusable = fmt.Errorf("Docker Sandboxes recovery ledger became unusable after publication: sync directory: %w", err)
		return ledger.unusable
	}
	return nil
}

func controlPlaneRecoveryChecksum(state controlPlaneRecoveryDiskState) ([]byte, error) {
	encoded, err := json.Marshal(controlPlaneRecoveryChecksumState{
		SchemaVersion: state.SchemaVersion,
		Generation:    state.Generation,
		State:         state.State,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func controlPlaneRecoveryPreCensusChecksum(state controlPlaneRecoveryDiskState) ([]byte, error) {
	encoded, err := json.Marshal(controlPlaneRecoveryPreCensusChecksumState{
		SchemaVersion: state.SchemaVersion,
		Generation:    state.Generation,
		State: controlPlaneRecoveryPreCensusState{
			Attempts:                     state.State.Attempts,
			NextAttemptAt:                state.State.NextAttemptAt,
			AdmissionIncident:            state.State.AdmissionIncident,
			RecoveryReservationToken:     state.State.RecoveryReservationToken,
			AdmissionIncidentToken:       state.State.AdmissionIncidentToken,
			RecoveryReservationPhase:     state.State.RecoveryReservationPhase,
			RecoveryReservationExpiresAt: state.State.RecoveryReservationExpiresAt,
		},
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func controlPlaneRecoveryLegacyChecksum(state controlPlaneRecoveryDiskState) ([]byte, error) {
	encoded, err := json.Marshal(controlPlaneRecoveryLegacyChecksumState{
		SchemaVersion: state.SchemaVersion,
		Generation:    state.Generation,
		State: controlPlaneRecoveryLegacyState{
			Attempts:                 state.State.Attempts,
			NextAttemptAt:            state.State.NextAttemptAt,
			AdmissionIncident:        state.State.AdmissionIncident,
			RecoveryReservationToken: state.State.RecoveryReservationToken,
			AdmissionIncidentToken:   state.State.AdmissionIncidentToken,
			RecoveryReservationPhase: state.State.RecoveryReservationPhase,
		},
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func validateControlPlaneRecoveryState(state ControlPlaneRecoveryState) error {
	if state.Attempts < 0 {
		return errors.New("negative recovery attempt count")
	}
	if state.RecoveryReservationToken != 0 && state.Attempts == 0 {
		return errors.New("recovery reservation token requires an active attempt")
	}
	if !state.AdmissionIncident && state.AdmissionIncidentToken != 0 {
		return errors.New("admission incident token requires an active incident")
	}
	switch state.RecoveryReservationPhase {
	case RecoveryReservationNone:
		if state.RecoveryReservationToken != 0 || !state.RecoveryReservationExpiresAt.IsZero() {
			return errors.New("recovery reservation phase is missing for active reservation")
		}
	case RecoveryReservationReserved, RecoveryReservationIntervening, RecoveryReservationVerifying:
		if state.RecoveryReservationToken == 0 || state.RecoveryReservationExpiresAt.IsZero() || state.Attempts == 0 {
			return errors.New("active recovery reservation needs token, expiry, and attempt")
		}
	default:
		return fmt.Errorf("unsupported recovery reservation phase %q", state.RecoveryReservationPhase)
	}
	if len(state.ExpectedIdentities) > controlPlaneRecoveryIdentityLimit {
		return fmt.Errorf("expected identity census contains %d entries; maximum is %d", len(state.ExpectedIdentities), controlPlaneRecoveryIdentityLimit)
	}
	seenProviderIDs := make(map[string]string, len(state.ExpectedIdentities))
	for name, providerID := range state.ExpectedIdentities {
		if strings.TrimSpace(name) != name || name == "" || strings.ContainsRune(name, 0) || len(name) > controlPlaneRecoveryIdentityBytes {
			return fmt.Errorf("expected identity census contains invalid immutable name %q", name)
		}
		if strings.TrimSpace(providerID) != providerID || providerID == "" || strings.ContainsRune(providerID, 0) || len(providerID) > controlPlaneRecoveryIdentityBytes {
			return fmt.Errorf("expected identity census contains invalid provider id for %q", name)
		}
		if previous, duplicate := seenProviderIDs[providerID]; duplicate && previous != name {
			return fmt.Errorf("expected identity census assigns provider id %q to both %q and %q", providerID, previous, name)
		}
		seenProviderIDs[providerID] = name
	}
	return nil
}

func canonicalizeControlPlaneRecoveryState(previous ControlPlaneRecoveryState, state *ControlPlaneRecoveryState) error {
	if state.ExpectedIdentities == nil {
		if previous.RecoveryReservationPhase == RecoveryReservationIntervening || previous.RecoveryReservationPhase == RecoveryReservationVerifying || state.RecoveryReservationPhase == RecoveryReservationIntervening || state.RecoveryReservationPhase == RecoveryReservationVerifying {
			return errors.New("interrupted recovery reservation cannot be rewritten without a durable host-wide identity census")
		}
		state.ExpectedIdentities = make(map[string]string)
	}
	return nil
}

func cloneControlPlaneRecoveryState(state ControlPlaneRecoveryState) ControlPlaneRecoveryState {
	if state.ExpectedIdentities == nil {
		return state
	}
	state.ExpectedIdentities = cloneControlPlaneRecoveryIdentities(state.ExpectedIdentities)
	return state
}

func cloneControlPlaneRecoveryIdentities(identities map[string]string) map[string]string {
	if identities == nil {
		return nil
	}
	cloned := make(map[string]string, len(identities))
	for name, providerID := range identities {
		cloned[name] = providerID
	}
	return cloned
}
