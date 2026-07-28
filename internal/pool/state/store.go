package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/filelock"
)

const snapshotFilename = "pool-lifecycle-v1.json"
const lockFilename = ".pool-lifecycle.lock"

type diskState struct {
	SchemaVersion int         `json:"schemaVersion"`
	Generation    uint64      `json:"generation"`
	Records       []Record    `json:"records"`
	Discoveries   []Discovery `json:"discoveries"`
	Checksum      string      `json:"checksum"`
}

type checksumState struct {
	SchemaVersion int         `json:"schemaVersion"`
	Generation    uint64      `json:"generation"`
	Records       []Record    `json:"records"`
	Discoveries   []Discovery `json:"discoveries"`
}

// Store serializes state across goroutines and cooperating controller processes.
type Store struct {
	directory string
	path      string
	lockPath  string
	mu        sync.Mutex
	now       func() time.Time
	fault     func(string) error // test-only crash boundary injection
}

func Open(directory string) (*Store, error) {
	if directory == "" {
		return nil, fmt.Errorf("%w: state directory is empty", ErrInvalidRecord)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create lifecycle state directory: %w", err)
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve lifecycle state directory: %w", err)
	}
	store := &Store{directory: directory, path: filepath.Join(directory, snapshotFilename), lockPath: filepath.Join(directory, lockFilename), now: time.Now}
	if err := store.withLock(context.Background(), func() error { _, err := store.load(); return err }); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Reserve(ctx context.Context, spec CreateSpec) (Record, error) {
	if err := validateName("instance name", spec.Name); err != nil {
		return Record{}, err
	}
	if err := validateName("provider type", spec.ProviderType); err != nil {
		return Record{}, err
	}
	if err := validateName("GitHub runner name", spec.GitHub.ExactName); err != nil {
		return Record{}, err
	}
	var result Record
	err := s.mutate(ctx, func(state *diskState, now time.Time) error {
		if _, found := findRecord(state.Records, spec.Name); found {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, spec.Name)
		}
		state.Generation++
		result = Record{Name: spec.Name, ProviderType: spec.ProviderType, GitHub: spec.GitHub, Phase: PhaseReserved, Generation: state.Generation, CreatedAt: now, UpdatedAt: now}
		state.Records = append(state.Records, result)
		sort.Slice(state.Records, func(i, j int) bool { return state.Records[i].Name < state.Records[j].Name })
		return nil
	})
	return cloneRecord(result), err
}

func (s *Store) Transition(ctx context.Context, name string, transition Transition) (Record, error) {
	if err := validateName("instance name", name); err != nil {
		return Record{}, err
	}
	var result Record
	err := s.mutate(ctx, func(state *diskState, now time.Time) error {
		index, found := findRecord(state.Records, name)
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		record := cloneRecord(state.Records[index])
		if err := applyTransition(&record, transition, now); err != nil {
			return err
		}
		state.Generation++
		record.Generation, record.UpdatedAt = state.Generation, now
		state.Records[index], result = record, record
		return nil
	})
	return cloneRecord(result), err
}

func (s *Store) AcquireLease(ctx context.Context, name string, lease Lease) (Record, error) {
	if err := validateName("instance name", name); err != nil {
		return Record{}, err
	}
	if err := validateName("lease purpose", lease.Purpose); err != nil {
		return Record{}, err
	}
	if err := validateName("lease holder", lease.Holder); err != nil {
		return Record{}, err
	}
	var result Record
	err := s.mutate(ctx, func(state *diskState, now time.Time) error {
		index, found := findRecord(state.Records, name)
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		record := cloneRecord(state.Records[index])
		if record.Phase == PhaseTombstoned || !lease.ExpiresAt.After(now) {
			return fmt.Errorf("%w: cannot acquire lease", ErrInvalidTransition)
		}
		retained := record.Leases[:0]
		for _, existing := range record.Leases {
			if existing.ExpiresAt.After(now) && !(existing.Purpose == lease.Purpose && existing.Holder == lease.Holder) {
				retained = append(retained, existing)
			}
		}
		record.Leases = append(retained, Lease{Purpose: lease.Purpose, Holder: lease.Holder, ExpiresAt: lease.ExpiresAt.UTC()})
		state.Generation++
		record.Generation, record.UpdatedAt = state.Generation, now
		state.Records[index], result = record, record
		return nil
	})
	return cloneRecord(result), err
}

func (s *Store) ReleaseLease(ctx context.Context, name, purpose, holder string) (Record, error) {
	if err := validateName("instance name", name); err != nil {
		return Record{}, err
	}
	if err := validateName("lease purpose", purpose); err != nil {
		return Record{}, err
	}
	if err := validateName("lease holder", holder); err != nil {
		return Record{}, err
	}
	var result Record
	err := s.mutate(ctx, func(state *diskState, now time.Time) error {
		index, found := findRecord(state.Records, name)
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		record := cloneRecord(state.Records[index])
		retained := record.Leases[:0]
		for _, lease := range record.Leases {
			if !(lease.Purpose == purpose && lease.Holder == holder) && lease.ExpiresAt.After(now) {
				retained = append(retained, lease)
			}
		}
		record.Leases = retained
		state.Generation++
		record.Generation, record.UpdatedAt = state.Generation, now
		state.Records[index], result = record, record
		return nil
	})
	return cloneRecord(result), err
}

// ReportUnknown records inventory that was not created by this state store.
// It is intentionally not exposed to Transition or cleanup operations.
func (s *Store) ReportUnknown(ctx context.Context, discovery Discovery) (Discovery, error) {
	if err := validateName("provider type", discovery.ProviderType); err != nil {
		return Discovery{}, err
	}
	if err := validateName("provider id", discovery.ProviderID); err != nil {
		return Discovery{}, err
	}
	if err := validateName("discovered instance name", discovery.ExactName); err != nil {
		return Discovery{}, err
	}
	if err := validateReceipt(discovery.Receipt); err != nil {
		return Discovery{}, err
	}
	err := s.mutate(ctx, func(state *diskState, now time.Time) error {
		discovery.ObservedAt = now
		state.Generation++
		for i, existing := range state.Discoveries {
			if existing.ProviderType == discovery.ProviderType && existing.ProviderID == discovery.ProviderID {
				state.Discoveries[i] = discovery
				return nil
			}
		}
		state.Discoveries = append(state.Discoveries, discovery)
		sort.Slice(state.Discoveries, func(i, j int) bool { return discoveryKey(state.Discoveries[i]) < discoveryKey(state.Discoveries[j]) })
		return nil
	})
	return discovery, err
}

func (s *Store) Read(ctx context.Context, name string) (Record, error) {
	if err := validateName("instance name", name); err != nil {
		return Record{}, err
	}
	var result Record
	err := s.withLock(ctx, func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		index, found := findRecord(state.Records, name)
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		result = cloneRecord(state.Records[index])
		return nil
	})
	return result, err
}
func (s *Store) List(ctx context.Context) ([]Record, error) {
	var result []Record
	err := s.withLock(ctx, func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		result = make([]Record, len(state.Records))
		for i := range state.Records {
			result[i] = cloneRecord(state.Records[i])
		}
		return nil
	})
	return result, err
}
func (s *Store) Discoveries(ctx context.Context) ([]Discovery, error) {
	var result []Discovery
	err := s.withLock(ctx, func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		result = append([]Discovery(nil), state.Discoveries...)
		return nil
	})
	return result, err
}

func (s *Store) mutate(ctx context.Context, operation func(*diskState, time.Time) error) error {
	return s.withLock(ctx, func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		if err := operation(&state, s.now().UTC()); err != nil {
			return err
		}
		if err := validateState(state); err != nil {
			return err
		}
		return s.save(state)
	})
}
func (s *Store) withLock(ctx context.Context, operation func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		lock, err := filelock.Acquire(s.lockPath)
		if err == nil {
			defer lock.Close()
			return operation()
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *Store) load() (diskState, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return diskState{SchemaVersion: SchemaVersion, Records: []Record{}, Discoveries: []Discovery{}}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		return diskState{}, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}
	if state.SchemaVersion != SchemaVersion {
		return diskState{}, fmt.Errorf("%w: schema %d", ErrCorrupt, state.SchemaVersion)
	}
	sum, err := checksum(state)
	if err != nil || state.Checksum != hex.EncodeToString(sum) {
		return diskState{}, fmt.Errorf("%w: checksum", ErrCorrupt)
	}
	if err := validateState(state); err != nil {
		return diskState{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return state, nil
}
func (s *Store) save(state diskState) error {
	sum, err := checksum(state)
	if err != nil {
		return err
	}
	state.Checksum = hex.EncodeToString(sum)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if s.fault != nil {
		if err := s.fault("before-temp"); err != nil {
			return err
		}
	}
	file, err := os.CreateTemp(s.directory, ".pool-lifecycle-*.tmp")
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
	if s.fault != nil {
		if err := s.fault("before-rename"); err != nil {
			return err
		}
	}
	if err := os.Rename(temp, s.path); err != nil {
		return err
	}
	committed = true
	return nil
}
func checksum(state diskState) ([]byte, error) {
	encoded, err := json.Marshal(checksumState{SchemaVersion: state.SchemaVersion, Generation: state.Generation, Records: state.Records, Discoveries: state.Discoveries})
	if err != nil {
		return nil, err
	}
	result := sha256.Sum256(encoded)
	return result[:], nil
}

func findRecord(records []Record, name string) (int, bool) {
	index := sort.Search(len(records), func(i int) bool { return records[i].Name >= name })
	return index, index < len(records) && records[index].Name == name
}
func discoveryKey(discovery Discovery) string {
	return discovery.ProviderType + "\x00" + discovery.ProviderID
}
func timePtr(value time.Time) *time.Time { copy := value; return &copy }

func applyTransition(record *Record, transition Transition, now time.Time) error {
	if record.Phase == PhaseTombstoned {
		return invalid(record, transition.Action)
	}
	switch transition.Action {
	case ActionCreateIntent:
		return move(record, PhaseReserved, PhaseCreating, transition.Action)
	case ActionAbandonCreate:
		if record.Phase != PhaseReserved && record.Phase != PhaseCreating {
			return invalid(record, transition.Action)
		}
		if record.ProviderID != "" || !emptyReceipt(record.Receipt) || len(activeLeases(record.Leases, now)) != 0 {
			return invalid(record, transition.Action)
		}
		record.Cleanup.RemoteVerifyAt = timePtr(now)
		record.Cleanup.RemoteAbsentAt = timePtr(now)
		record.Cleanup.LocalRemoveIntentAt = timePtr(now)
		record.Cleanup.LocalAbsentAt = timePtr(now)
		record.Phase = PhaseTombstoned
		record.TombstonedAt = timePtr(now)
	case ActionCreated:
		if record.Phase != PhaseCreating || validateName("provider id", transition.ProviderID) != nil || validateReceipt(transition.Receipt) != nil {
			return invalid(record, transition.Action)
		}
		record.ProviderID, record.Receipt, record.Phase = transition.ProviderID, transition.Receipt, PhaseCreated
	case ActionValidateIntent:
		return move(record, PhaseCreated, PhaseValidating, transition.Action)
	case ActionValidated:
		return move(record, PhaseValidating, PhaseStandby, transition.Action)
	case ActionRegisterIntent:
		return move(record, PhaseStandby, PhaseRegistering, transition.Action)
	case ActionRegistered:
		if record.Phase != PhaseRegistering || transition.RunnerID <= 0 {
			return invalid(record, transition.Action)
		}
		record.GitHub.RunnerID, record.Phase = transition.RunnerID, PhaseReady
	case ActionJobStarted:
		return move(record, PhaseReady, PhaseBusy, transition.Action)
	case ActionJobFinished:
		return move(record, PhaseBusy, PhaseDraining, transition.Action)
	case ActionQuarantine:
		if transition.Reason == "" || !quarantineAllowed(record.Phase) {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Quarantine = PhaseQuarantined, &Quarantine{Reason: transition.Reason, ReportedAt: now}
	case ActionFenceIntent:
		if !fenceAllowed(record.Phase) {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Cleanup.FenceIntentAt = PhaseFencing, timePtr(now)
	case ActionFenced:
		return move(record, PhaseFencing, PhaseFenced, transition.Action)
	case ActionVerifyRemoteIntent:
		if record.Phase != PhaseFenced {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Cleanup.RemoteVerifyAt = PhaseRemoteReconciling, timePtr(now)
	case ActionRemoteAbsent:
		if record.Phase != PhaseRemoteReconciling {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Cleanup.RemoteAbsentAt = PhaseRemoteAbsent, timePtr(now)
	case ActionRemoveLocalIntent:
		if record.Phase != PhaseRemoteAbsent {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Cleanup.LocalRemoveIntentAt = PhaseLocalRemoving, timePtr(now)
	case ActionLocalAbsent:
		if record.Phase != PhaseLocalRemoving {
			return invalid(record, transition.Action)
		}
		record.Phase, record.Cleanup.LocalAbsentAt = PhaseLocalAbsent, timePtr(now)
	case ActionCleanupPending:
		if record.Phase != PhaseFencing && record.Phase != PhaseRemoteReconciling && record.Phase != PhaseLocalRemoving {
			return invalid(record, transition.Action)
		}
		record.Phase = PhaseCleanupPending
	case ActionResumeCleanup:
		if record.Phase != PhaseCleanupPending {
			return invalid(record, transition.Action)
		}
		switch {
		case record.Cleanup.LocalAbsentAt != nil:
			record.Phase = PhaseLocalAbsent
		case record.Cleanup.LocalRemoveIntentAt != nil:
			record.Phase = PhaseLocalRemoving
		case record.Cleanup.RemoteAbsentAt != nil:
			record.Phase = PhaseRemoteAbsent
		case record.Cleanup.RemoteVerifyAt != nil:
			record.Phase = PhaseRemoteReconciling
		default:
			record.Phase = PhaseFencing
		}
	case ActionTombstone:
		if record.Phase != PhaseLocalAbsent || record.Cleanup.RemoteAbsentAt == nil || record.Cleanup.LocalAbsentAt == nil || len(activeLeases(record.Leases, now)) != 0 {
			return invalid(record, transition.Action)
		}
		record.Phase, record.TombstonedAt = PhaseTombstoned, timePtr(now)
	default:
		return fmt.Errorf("%w: unknown action %q", ErrInvalidTransition, transition.Action)
	}
	return nil
}

func move(record *Record, from, to Phase, action Action) error {
	if record.Phase != from {
		return invalid(record, action)
	}
	record.Phase = to
	return nil
}
func invalid(record *Record, action Action) error {
	return fmt.Errorf("%w: %s from %s", ErrInvalidTransition, action, record.Phase)
}
func fenceAllowed(phase Phase) bool {
	switch phase {
	case PhaseCreated, PhaseValidating, PhaseStandby, PhaseRegistering, PhaseReady, PhaseBusy, PhaseDraining, PhaseQuarantined:
		return true
	default:
		return false
	}
}
func quarantineAllowed(phase Phase) bool {
	switch phase {
	case PhaseReserved, PhaseCreating, PhaseCreated, PhaseValidating, PhaseStandby, PhaseRegistering, PhaseReady, PhaseBusy, PhaseDraining:
		return true
	default:
		return false
	}
}
func activeLeases(leases []Lease, now time.Time) []Lease {
	result := make([]Lease, 0, len(leases))
	for _, lease := range leases {
		if lease.ExpiresAt.After(now) {
			result = append(result, lease)
		}
	}
	return result
}

func validateState(state diskState) error {
	if state.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema", ErrInvalidRecord)
	}
	for i := range state.Records {
		record := state.Records[i]
		if err := validateRecord(record); err != nil {
			return err
		}
		if i > 0 && state.Records[i-1].Name >= record.Name {
			return fmt.Errorf("%w: records are not sorted", ErrInvalidRecord)
		}
	}
	for i, discovery := range state.Discoveries {
		if err := validateDiscovery(discovery); err != nil {
			return err
		}
		if i > 0 && discoveryKey(state.Discoveries[i-1]) >= discoveryKey(discovery) {
			return fmt.Errorf("%w: discoveries are not sorted", ErrInvalidRecord)
		}
	}
	return nil
}
func validateRecord(record Record) error {
	if err := validateName("instance name", record.Name); err != nil {
		return err
	}
	if err := validateName("provider type", record.ProviderType); err != nil {
		return err
	}
	if err := validateName("GitHub runner name", record.GitHub.ExactName); err != nil {
		return err
	}
	if !validPhase(record.Phase) || record.Generation == 0 || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return fmt.Errorf("%w: record phase, generation, or time", ErrInvalidRecord)
	}
	if record.Phase != PhaseReserved && record.Phase != PhaseCreating && !(record.Phase == PhaseQuarantined && record.ProviderID == "" && emptyReceipt(record.Receipt)) && !(record.Phase == PhaseTombstoned && record.ProviderID == "" && emptyReceipt(record.Receipt)) {
		if err := validateName("provider id", record.ProviderID); err != nil {
			return err
		}
		if err := validateReceipt(record.Receipt); err != nil {
			return err
		}
	}
	for _, lease := range record.Leases {
		if err := validateName("lease purpose", lease.Purpose); err != nil {
			return err
		}
		if err := validateName("lease holder", lease.Holder); err != nil {
			return err
		}
		if lease.ExpiresAt.IsZero() {
			return fmt.Errorf("%w: lease expiry", ErrInvalidRecord)
		}
	}
	if record.Quarantine != nil && (record.Quarantine.Reason == "" || record.Quarantine.ReportedAt.IsZero()) {
		return fmt.Errorf("%w: quarantine", ErrInvalidRecord)
	}
	if record.Phase == PhaseTombstoned && (record.TombstonedAt == nil || record.Cleanup.RemoteAbsentAt == nil || record.Cleanup.LocalAbsentAt == nil) {
		return fmt.Errorf("%w: tombstone needs exact absence", ErrInvalidRecord)
	}
	return nil
}

func emptyReceipt(receipt Receipt) bool {
	return receipt.Version == "" && (len(receipt.Payload) == 0 || string(receipt.Payload) == "null")
}
func validateDiscovery(discovery Discovery) error {
	if err := validateName("provider type", discovery.ProviderType); err != nil {
		return err
	}
	if err := validateName("provider id", discovery.ProviderID); err != nil {
		return err
	}
	if err := validateName("discovered instance name", discovery.ExactName); err != nil {
		return err
	}
	if err := validateReceipt(discovery.Receipt); err != nil {
		return err
	}
	if discovery.ObservedAt.IsZero() {
		return fmt.Errorf("%w: discovery time", ErrInvalidRecord)
	}
	return nil
}
func validPhase(phase Phase) bool {
	switch phase {
	case PhaseReserved, PhaseCreating, PhaseCreated, PhaseValidating, PhaseStandby, PhaseRegistering, PhaseReady, PhaseBusy, PhaseDraining, PhaseQuarantined, PhaseCleanupPending, PhaseFencing, PhaseFenced, PhaseRemoteReconciling, PhaseRemoteAbsent, PhaseLocalRemoving, PhaseLocalAbsent, PhaseTombstoned:
		return true
	default:
		return false
	}
}
