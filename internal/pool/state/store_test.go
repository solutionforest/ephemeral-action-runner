package state

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func receipt(t *testing.T) Receipt {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"external": "exact-id"})
	if err != nil {
		t.Fatal(err)
	}
	return Receipt{Version: "v1", Payload: payload}
}
func reserve(t *testing.T, store *Store, name string) Record {
	t.Helper()
	record, err := store.Reserve(context.Background(), CreateSpec{Name: name, ProviderType: "test-provider", GitHub: GitHubIdentity{ExactName: name + "-runner"}})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLifecycleAllowsOnlyDocumentedTransitions(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := reserve(t, store, "instance-a")
	sequence := []Transition{{Action: ActionCreateIntent}, {Action: ActionCreated, ProviderID: "provider:1", Receipt: receipt(t)}, {Action: ActionValidateIntent}, {Action: ActionValidated}, {Action: ActionRegisterIntent}, {Action: ActionRegistered, RunnerID: 42}, {Action: ActionJobStarted}, {Action: ActionJobFinished}, {Action: ActionFenceIntent}, {Action: ActionFenced}, {Action: ActionVerifyRemoteIntent}, {Action: ActionRemoteAbsent}, {Action: ActionRemoveLocalIntent}, {Action: ActionLocalAbsent}, {Action: ActionTombstone}}
	for _, transition := range sequence {
		record, err = store.Transition(context.Background(), record.Name, transition)
		if err != nil {
			t.Fatalf("%s from %s: %v", transition.Action, record.Phase, err)
		}
	}
	if record.Phase != PhaseTombstoned {
		t.Fatalf("phase = %s", record.Phase)
	}
	if _, err := store.Transition(context.Background(), record.Name, Transition{Action: ActionJobStarted}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestForbiddenTransitionsAndQuarantineResume(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := reserve(t, store, "instance-b")
	for _, transition := range []Transition{{Action: ActionValidated}, {Action: ActionRegistered, RunnerID: 1}, {Action: ActionTombstone}} {
		if _, err := store.Transition(context.Background(), record.Name, transition); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s error = %v", transition.Action, err)
		}
	}
	if _, err := store.Transition(context.Background(), record.Name, Transition{Action: ActionQuarantine, Reason: "create outcome is uncertain"}); err != nil {
		t.Fatalf("quarantine uncertain create = %v", err)
	}
	record = reserve(t, store, "instance-b2")
	if _, err := store.Transition(context.Background(), record.Name, Transition{Action: ActionCreateIntent}); err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionCreated, ProviderID: "provider:quarantine", Receipt: receipt(t)})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionQuarantine, Reason: "provider inventory differed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []Transition{{Action: ActionFenceIntent}, {Action: ActionCleanupPending}, {Action: ActionResumeCleanup}} {
		record, err = store.Transition(context.Background(), record.Name, transition)
		if err != nil {
			t.Fatalf("%s: %v", transition.Action, err)
		}
	}
	if record.Phase != PhaseFencing {
		t.Fatalf("phase = %s", record.Phase)
	}
}

func TestAbandonCreateRequiresNoProviderIdentityAndRecordsExactAbsence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reserved-instance", "creating-instance"} {
		record := reserve(t, store, name)
		if name == "creating-instance" {
			record, err = store.Transition(context.Background(), name, Transition{Action: ActionCreateIntent})
			if err != nil {
				t.Fatal(err)
			}
		}
		record, err = store.Transition(context.Background(), name, Transition{Action: ActionAbandonCreate})
		if err != nil {
			t.Fatalf("abandon %s: %v", name, err)
		}
		if record.Phase != PhaseTombstoned || record.Cleanup.RemoteAbsentAt == nil || record.Cleanup.LocalAbsentAt == nil {
			t.Fatalf("abandoned record = %#v", record)
		}
	}
}

func TestProviderReceiptAndUnknownDiscoveryRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := reserve(t, store, "instance-c")
	if _, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionCreateIntent}); err != nil {
		t.Fatal(err)
	}
	want := receipt(t)
	if _, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionCreated, ProviderID: "provider:2", Receipt: want}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReportUnknown(context.Background(), Discovery{ProviderType: "test-provider", ProviderID: "foreign:1", ExactName: "foreign-instance", Receipt: want}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Read(context.Background(), record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got.Receipt.Payload, want.Payload) {
		t.Fatalf("receipt = %s", got.Receipt.Payload)
	}
	discoveries, err := reopened.Discoveries(context.Background())
	if err != nil || len(discoveries) != 1 || discoveries[0].ExactName != "foreign-instance" {
		t.Fatalf("discoveries = %#v err=%v", discoveries, err)
	}
	if _, err = reopened.Transition(context.Background(), "foreign-instance", Transition{Action: ActionFenceIntent}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown resource became owned: %v", err)
	}
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func TestLeaseExpiryAndTombstoneProtection(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Now().UTC()
	store.now = func() time.Time { return fixed }
	record := reserve(t, store, "instance-d")
	if _, err = store.AcquireLease(context.Background(), record.Name, Lease{Purpose: "cleanup", Holder: "controller", ExpiresAt: fixed.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []Transition{{Action: ActionCreateIntent}, {Action: ActionCreated, ProviderID: "provider:3", Receipt: receipt(t)}, {Action: ActionFenceIntent}, {Action: ActionFenced}, {Action: ActionVerifyRemoteIntent}, {Action: ActionRemoteAbsent}, {Action: ActionRemoveLocalIntent}, {Action: ActionLocalAbsent}} {
		if _, err = store.Transition(context.Background(), record.Name, transition); err != nil {
			t.Fatalf("%s: %v", transition.Action, err)
		}
	}
	if _, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionTombstone}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("tombstone with lease = %v", err)
	}
	if _, err = store.ReleaseLease(context.Background(), record.Name, "cleanup", "controller"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AcquireLease(context.Background(), record.Name, Lease{Purpose: "audit", Holder: "controller", ExpiresAt: fixed.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fixed = fixed.Add(2 * time.Hour)
	if _, err = store.Transition(context.Background(), record.Name, Transition{Action: ActionTombstone}); err != nil {
		t.Fatal(err)
	}
}

func TestPartialWriteKeepsPriorSnapshotAndCorruptionFailsClosed(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "instance-e")
	store.fault = func(point string) error {
		if point == "before-rename" {
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, err = store.Reserve(context.Background(), CreateSpec{Name: "instance-f", ProviderType: "test-provider", GitHub: GitHubIdentity{ExactName: "instance-f-runner"}}); err == nil {
		t.Fatal("faulted write succeeded")
	}
	store.fault = nil
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.List(context.Background())
	if err != nil || len(records) != 1 || records[0].Name != "instance-e" {
		t.Fatalf("recovery records=%#v err=%v", records, err)
	}
	if err := os.WriteFile(reopened.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt open error = %v", err)
	}
}

func TestRejectsUnknownPhaseFromDisk(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	reserve(t, store, "instance-g")
	state, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	state.Records[0].Phase = "invented"
	sum, err := checksum(state)
	if err != nil {
		t.Fatal(err)
	}
	state.Checksum = hex.EncodeToString(sum)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown phase error = %v", err)
	}
}

func TestConcurrentReservationsAreSerialized(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const count = 12
	var wg sync.WaitGroup
	errorsCh := make(chan error, count)
	for index := 0; index < count; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Reserve(context.Background(), CreateSpec{Name: fmt.Sprintf("instance-%d", index), ProviderType: "test-provider", GitHub: GitHubIdentity{ExactName: fmt.Sprintf("runner-%d", index)}})
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != count {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
}
