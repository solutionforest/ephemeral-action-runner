package provider

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlPlaneRecoveryLedgerRoundTripsAcrossReopen(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	want, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 3
		state.NextAttemptAt = wantNext
		state.AdmissionIncident = true
		state.AdmissionIncidentToken = 7
		state.ExpectedIdentities = map[string]string{
			"foreign-config-runner": "provider:foreign-config-runner",
			"report-only-runner":    "provider:report-only-runner",
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want.Attempts != 3 || !want.NextAttemptAt.Equal(wantNext) || !want.AdmissionIncident || want.AdmissionIncidentToken != 7 {
		t.Fatalf("updated state = %#v", want)
	}

	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != want.Attempts || !got.NextAttemptAt.Equal(want.NextAttemptAt) || got.AdmissionIncident != want.AdmissionIncident || got.AdmissionIncidentToken != want.AdmissionIncidentToken || !reflect.DeepEqual(got.ExpectedIdentities, want.ExpectedIdentities) {
		t.Fatalf("reopened state = %#v, want %#v", got, want)
	}
}

func TestControlPlaneRecoveryLedgerSerializesUpdates(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	otherLedger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	const updates = 12
	var group sync.WaitGroup
	errs := make(chan error, updates)
	for index := range updates {
		group.Add(1)
		currentLedger := ledger
		if index%2 == 1 {
			currentLedger = otherLedger
		}
		go func() {
			defer group.Done()
			_, err := currentLedger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
				state.Attempts++
				return nil
			})
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != updates {
		t.Fatalf("serialized attempts = %d, want %d", state.Attempts, updates)
	}
}

func TestControlPlaneRecoveryLedgerFailedRenameKeepsPriorSnapshot(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ledger.fault = func(point string) error {
		if point == "before-rename" {
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 2
		return nil
	}); err == nil {
		t.Fatal("faulted ledger update succeeded")
	}
	ledger.fault = nil
	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 1 {
		t.Fatalf("state after failed rename = %#v, want prior snapshot", state)
	}
}

func TestControlPlaneRecoveryLedgerDirectorySyncFailureLatchesInstance(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	ledger.fault = func(point string) error {
		if point == "before-directory-sync" {
			return errors.New("simulated directory sync failure")
		}
		return nil
	}
	if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 1
		return nil
	}); err == nil || !strings.Contains(err.Error(), "became unusable") {
		t.Fatalf("directory-sync fault error = %v, want latched unusable error", err)
	}
	ledger.fault = nil
	if _, err := ledger.State(context.Background()); err == nil || !strings.Contains(err.Error(), "became unusable") {
		t.Fatalf("latched ledger state error = %v, want persistent fail-closed error", err)
	}
	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 1 {
		t.Fatalf("reopened state after directory-sync fault = %#v, want published snapshot", state)
	}
}

func TestControlPlaneRecoveryLedgerUpdateIfDoesNotApplyStalePredicate(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.AdmissionIncident = true
		state.AdmissionIncidentToken = 9
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, applied, err := ledger.UpdateIfGeneration(context.Background(), state.Generation, func(state *ControlPlaneRecoveryState) error {
		state.Attempts = 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied || current.Attempts != 1 || !current.AdmissionIncident || current.AdmissionIncidentToken != 9 {
		t.Fatalf("stale conditional update = state %#v applied=%t, want newer state unchanged", current, applied)
	}
}

func TestControlPlaneRecoveryLedgerCorruptionFailsClosed(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "recovery ledger is corrupt") {
		t.Fatalf("corrupt ledger open error = %v", err)
	}
}

func TestControlPlaneRecoveryLedgerAcceptsPreExpirySnapshot(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	const fixture = `{"schemaVersion":1,"generation":4,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:00:00Z","admissionIncident":true},"checksum":"14d0758ad8bd6d1ea2dbecc56f40da655a5f8d9c1fd1e94515e02a905c90da72"}`
	if err := os.WriteFile(ledger.Path(), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if state.Attempts != 2 || !state.NextAttemptAt.Equal(wantNext) || !state.AdmissionIncident || state.Generation != 4 {
		t.Fatalf("legacy recovery state = %#v", state)
	}
	if state.RecoveryReservationPhase != RecoveryReservationNone || !state.RecoveryReservationExpiresAt.IsZero() {
		t.Fatalf("legacy recovery state unexpectedly gained reservation metadata = %#v", state)
	}
}

func TestControlPlaneRecoveryLedgerAcceptsActivePreExpirySnapshots(t *testing.T) {
	fixtures := []struct {
		name    string
		phase   RecoveryReservationPhase
		fixture string
	}{
		{
			name:    "reserved",
			phase:   RecoveryReservationReserved,
			fixture: `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"reserved"},"checksum":"4636a38c1da8a212c2d3e5c4435f4f49a75ea9ebb803e520ff76deefdfe4cc36"}`,
		},
		{
			name:    "intervening",
			phase:   RecoveryReservationIntervening,
			fixture: `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"intervening"},"checksum":"76d79db8df1d97ab5b58279fceff89cb7b37ec4cb2a170bb47007dd2f26e66e1"}`,
		},
		{
			name:    "verifying",
			phase:   RecoveryReservationVerifying,
			fixture: `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"verifying"},"checksum":"99f36c24213d4dadfdc0e8e851c736bb0c9ac7c2ed5570e9ea0a7c4ed83f6800"}`,
		},
	}
	wantDeadline := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	for _, test := range fixtures {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			ledger, err := OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ledger.Path(), []byte(test.fixture), 0o600); err != nil {
				t.Fatal(err)
			}

			reopened, err := OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			state, err := reopened.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.Attempts != 2 || state.RecoveryReservationToken != 41 || state.AdmissionIncidentToken != 42 || state.RecoveryReservationPhase != test.phase || state.Generation != 7 {
				t.Fatalf("migrated %s state = %#v", test.name, state)
			}
			if !state.NextAttemptAt.Equal(wantDeadline) || !state.RecoveryReservationExpiresAt.Equal(wantDeadline) {
				t.Fatalf("migrated %s deadlines = next %s expiry %s, want %s", test.name, state.NextAttemptAt, state.RecoveryReservationExpiresAt, wantDeadline)
			}

			updated, updateErr := reopened.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
				state.Attempts++
				return nil
			})
			if test.phase == RecoveryReservationIntervening || test.phase == RecoveryReservationVerifying {
				if updateErr == nil || !strings.Contains(updateErr.Error(), "cannot be rewritten without a durable host-wide identity census") {
					t.Fatalf("unsafe %s rewrite = state %#v error %v, want missing-census refusal", test.name, updated, updateErr)
				}
				return
			}
			if updateErr != nil {
				t.Fatal(updateErr)
			}
			if updated.Attempts != 3 || updated.Generation != 8 || updated.RecoveryReservationPhase != test.phase || !updated.RecoveryReservationExpiresAt.Equal(wantDeadline) || updated.ExpectedIdentities == nil {
				t.Fatalf("rewritten %s state = %#v", test.name, updated)
			}
			data, err := os.ReadFile(reopened.Path())
			if err != nil {
				t.Fatal(err)
			}
			var raw struct {
				State map[string]json.RawMessage `json:"state"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if _, ok := raw.State["recoveryReservationExpiresAt"]; !ok {
				t.Fatalf("rewritten %s snapshot omitted current reservation expiry: %s", test.name, data)
			}
			if _, ok := raw.State["expectedIdentities"]; !ok {
				t.Fatalf("rewritten %s snapshot omitted canonical identity census: %s", test.name, data)
			}
			if _, err := OpenControlPlaneRecoveryLedger(); err != nil {
				t.Fatalf("reopen rewritten %s snapshot: %v", test.name, err)
			}
		})
	}
}

func TestControlPlaneRecoveryLedgerRejectsPhaseLessActiveLegacyReservationAsAmbiguous(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	disk := controlPlaneRecoveryDiskState{
		SchemaVersion: controlPlaneRecoveryStateVersion,
		Generation:    7,
		State: ControlPlaneRecoveryState{
			Attempts:                 2,
			NextAttemptAt:            deadline,
			AdmissionIncident:        true,
			RecoveryReservationToken: 41,
			AdmissionIncidentToken:   42,
		},
	}
	sum, err := controlPlaneRecoveryLegacyChecksum(disk)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		SchemaVersion int                             `json:"schemaVersion"`
		Generation    uint64                          `json:"generation"`
		State         controlPlaneRecoveryLegacyState `json:"state"`
		Checksum      string                          `json:"checksum"`
	}{
		SchemaVersion: disk.SchemaVersion,
		Generation:    disk.Generation,
		State: controlPlaneRecoveryLegacyState{
			Attempts:                 disk.State.Attempts,
			NextAttemptAt:            disk.State.NextAttemptAt,
			AdmissionIncident:        disk.State.AdmissionIncident,
			RecoveryReservationToken: disk.State.RecoveryReservationToken,
			AdmissionIncidentToken:   disk.State.AdmissionIncidentToken,
		},
		Checksum: hex.EncodeToString(sum),
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("recoveryReservationPhase")) {
		t.Fatalf("phase-less legacy fixture unexpectedly contains a durable phase: %s", data)
	}
	if err := os.WriteFile(ledger.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "legacy active recovery reservation has no durable phase and is ambiguous") {
		t.Fatalf("phase-less active legacy ledger open error = %v, want ambiguity refusal", err)
	}
	written, err := os.ReadFile(ledger.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, data) {
		t.Fatalf("ambiguous legacy ledger was rewritten: got %s, want %s", written, data)
	}
}

func TestControlPlaneRecoveryLedgerAcceptsPreCensusCurrentSnapshot(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	disk := controlPlaneRecoveryDiskState{
		SchemaVersion: controlPlaneRecoveryStateVersion,
		Generation:    7,
		State: ControlPlaneRecoveryState{
			Attempts:                     2,
			NextAttemptAt:                deadline,
			RecoveryReservationToken:     41,
			RecoveryReservationPhase:     RecoveryReservationVerifying,
			RecoveryReservationExpiresAt: deadline,
		},
	}
	sum, err := controlPlaneRecoveryPreCensusChecksum(disk)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		SchemaVersion int                                `json:"schemaVersion"`
		Generation    uint64                             `json:"generation"`
		State         controlPlaneRecoveryPreCensusState `json:"state"`
		Checksum      string                             `json:"checksum"`
	}{
		SchemaVersion: disk.SchemaVersion,
		Generation:    disk.Generation,
		State: controlPlaneRecoveryPreCensusState{
			Attempts:                     disk.State.Attempts,
			NextAttemptAt:                disk.State.NextAttemptAt,
			RecoveryReservationToken:     disk.State.RecoveryReservationToken,
			RecoveryReservationPhase:     disk.State.RecoveryReservationPhase,
			RecoveryReservationExpiresAt: disk.State.RecoveryReservationExpiresAt,
		},
		Checksum: hex.EncodeToString(sum),
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 7 || state.RecoveryReservationPhase != RecoveryReservationVerifying || state.ExpectedIdentities != nil {
		t.Fatalf("pre-census current recovery state = %#v", state)
	}
	updated, err := reopened.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.RecoveryReservationPhase = RecoveryReservationReserved
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be rewritten without a durable host-wide identity census") {
		t.Fatalf("pre-census interrupted rewrite = state %#v error %v, want fail-closed refusal", updated, err)
	}
	written, err := os.ReadFile(reopened.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), `"expectedIdentities"`) {
		t.Fatalf("refused pre-census rewrite changed the legacy shape: %s", written)
	}
}

func TestControlPlaneRecoveryLedgerRejectsMalformedIdentityCensus(t *testing.T) {
	tests := []struct {
		name     string
		census   func() map[string]string
		contains string
	}{
		{
			name: "provider id collision",
			census: func() map[string]string {
				return map[string]string{"runner-one": "provider:shared", "runner-two": "provider:shared"}
			},
			contains: "assigns provider id",
		},
		{
			name: "oversized map",
			census: func() map[string]string {
				census := make(map[string]string, controlPlaneRecoveryIdentityLimit+1)
				for index := 0; index <= controlPlaneRecoveryIdentityLimit; index++ {
					name := fmt.Sprintf("runner-%04d", index)
					census[name] = "provider:" + name
				}
				return census
			},
			contains: "maximum is",
		},
		{
			name: "oversized identity",
			census: func() map[string]string {
				return map[string]string{strings.Repeat("n", controlPlaneRecoveryIdentityBytes+1): "provider:one"}
			},
			contains: "invalid immutable name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			ledger, err := OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
				state.ExpectedIdentities = test.census()
				return nil
			}); err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("malformed identity census error = %v, want %q", err, test.contains)
			}
		})
	}
}

func TestControlPlaneRecoveryLedgerIdentityCensusIsChecksummed(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Update(context.Background(), func(state *ControlPlaneRecoveryState) error {
		state.ExpectedIdentities = map[string]string{"foreign-config-runner": "provider:foreign-config-runner"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ledger.Path())
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), "provider:foreign-config-runner", "provider:tampered-foreign-runner", 1)
	if tampered == string(data) {
		t.Fatal("identity census tamper did not change fixture")
	}
	if err := os.WriteFile(ledger.Path(), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "recovery ledger is corrupt: checksum") {
		t.Fatalf("tampered identity census open error = %v, want checksum failure", err)
	}
}

func TestControlPlaneRecoveryLedgerRejectsTamperedPreExpirySnapshot(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	const fixture = `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"verifying"},"checksum":"4636a38c1da8a212c2d3e5c4435f4f49a75ea9ebb803e520ff76deefdfe4cc36"}`
	if err := os.WriteFile(ledger.Path(), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "recovery ledger is corrupt: checksum") {
		t.Fatalf("tampered legacy ledger open error = %v, want checksum failure", err)
	}
}

func TestControlPlaneRecoveryLedgerRejectsPreExpiryActiveSnapshotWithoutDeadline(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	disk := controlPlaneRecoveryDiskState{
		SchemaVersion: controlPlaneRecoveryStateVersion,
		Generation:    7,
		State: ControlPlaneRecoveryState{
			Attempts:                 2,
			RecoveryReservationToken: 41,
			RecoveryReservationPhase: RecoveryReservationIntervening,
		},
	}
	sum, err := controlPlaneRecoveryLegacyChecksum(disk)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		SchemaVersion int                             `json:"schemaVersion"`
		Generation    uint64                          `json:"generation"`
		State         controlPlaneRecoveryLegacyState `json:"state"`
		Checksum      string                          `json:"checksum"`
	}{
		SchemaVersion: disk.SchemaVersion,
		Generation:    disk.Generation,
		State: controlPlaneRecoveryLegacyState{
			Attempts:                 disk.State.Attempts,
			NextAttemptAt:            disk.State.NextAttemptAt,
			RecoveryReservationToken: disk.State.RecoveryReservationToken,
			RecoveryReservationPhase: disk.State.RecoveryReservationPhase,
		},
		Checksum: hex.EncodeToString(sum),
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "active recovery reservation needs token, expiry, and attempt") {
		t.Fatalf("deadline-free legacy ledger open error = %v, want invalid active reservation", err)
	}
}

func TestControlPlaneRecoveryLedgerRejectsCurrentExpiryWithLegacyChecksum(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	const fixture = `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"reserved","recoveryReservationExpiresAt":"2026-09-01T09:30:00Z"},"checksum":"4636a38c1da8a212c2d3e5c4435f4f49a75ea9ebb803e520ff76deefdfe4cc36"}`
	if err := os.WriteFile(ledger.Path(), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "recovery ledger is corrupt: checksum") {
		t.Fatalf("current-shape ledger with legacy checksum open error = %v, want checksum failure", err)
	}
}

func TestControlPlaneRecoveryLedgerRejectsLegacyShapeWithCurrentChecksum(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	legacyShapeWithCurrentChecksum := strings.Replace(string(currentControlPlaneRecoveryFixture(t)), `,"recoveryReservationExpiresAt":"2026-09-01T08:30:00Z"`, "", 1)
	legacyShapeWithCurrentChecksum = strings.Replace(legacyShapeWithCurrentChecksum, `,"expectedIdentities":{"foreign-config-runner":"provider:foreign-config-runner"}`, "", 1)
	if strings.Contains(legacyShapeWithCurrentChecksum, "recoveryReservationExpiresAt") || strings.Contains(legacyShapeWithCurrentChecksum, "expectedIdentities") {
		t.Fatal("test fixture still has current-only reservation expiry")
	}
	if err := os.WriteFile(ledger.Path(), []byte(legacyShapeWithCurrentChecksum), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), "recovery ledger is corrupt: checksum") {
		t.Fatalf("legacy-shape ledger with current checksum open error = %v, want checksum failure", err)
	}
}

func TestControlPlaneRecoveryLedgerRejectsExpiryAliasWithLegacyChecksum(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	const fixture = `{"schemaVersion":1,"generation":7,"state":{"attempts":2,"nextAttemptAt":"2026-09-01T08:30:00Z","admissionIncident":true,"recoveryReservationToken":41,"admissionIncidentToken":42,"recoveryReservationPhase":"reserved","RecoveryReservationExpiresAt":"2026-09-01T09:30:00Z"},"checksum":"4636a38c1da8a212c2d3e5c4435f4f49a75ea9ebb803e520ff76deefdfe4cc36"}`
	if err := os.WriteFile(ledger.Path(), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), `unsupported state field "RecoveryReservationExpiresAt"`) {
		t.Fatalf("aliased current-shape ledger open error = %v, want unsupported-key failure", err)
	}
}

func TestControlPlaneRecoveryLedgerAcceptsExactCurrentShape(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	data := currentControlPlaneRecoveryFixture(t)
	if err := os.WriteFile(ledger.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 2 || state.Generation != 7 || state.RecoveryReservationToken != 41 || state.RecoveryReservationPhase != RecoveryReservationReserved || state.ExpectedIdentities["foreign-config-runner"] != "provider:foreign-config-runner" {
		t.Fatalf("exact current recovery state = %#v", state)
	}
}

func TestControlPlaneRecoveryLedgerRejectsUnsupportedJSONShapeBeforeChecksum(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name: "unknown envelope field",
			mutate: func(data string) string {
				return strings.Replace(data, `,"checksum":`, `,"unexpectedEnvelope":true,"checksum":`, 1)
			},
			want: `unsupported envelope field "unexpectedEnvelope"`,
		},
		{
			name: "duplicate envelope field",
			mutate: func(data string) string {
				return strings.Replace(data, `,"generation":7,`, `,"generation":7,"generation":7,`, 1)
			},
			want: `duplicate envelope field "generation"`,
		},
		{
			name: "case aliased envelope field",
			mutate: func(data string) string {
				return strings.Replace(data, `,"generation":7,`, `,"Generation":7,`, 1)
			},
			want: `unsupported envelope field "Generation"`,
		},
		{
			name: "unknown state field",
			mutate: func(data string) string {
				return strings.Replace(data, `,"recoveryReservationPhase":"reserved","recoveryReservationExpiresAt":`, `,"recoveryReservationPhase":"reserved","unexpectedState":true,"recoveryReservationExpiresAt":`, 1)
			},
			want: `unsupported state field "unexpectedState"`,
		},
		{
			name: "duplicate state field",
			mutate: func(data string) string {
				return strings.Replace(data, `,"admissionIncident":true,`, `,"admissionIncident":true,"admissionIncident":true,`, 1)
			},
			want: `duplicate state field "admissionIncident"`,
		},
		{
			name: "case aliased state field",
			mutate: func(data string) string {
				return strings.Replace(data, `"attempts":2,`, `"Attempts":2,`, 1)
			},
			want: `unsupported state field "Attempts"`,
		},
		{
			name: "null expected identities",
			mutate: func(data string) string {
				return strings.Replace(data, `"expectedIdentities":{"foreign-config-runner":"provider:foreign-config-runner"}`, `"expectedIdentities":null`, 1)
			},
			want: "expected identities must be an object",
		},
		{
			name: "duplicate expected identity name",
			mutate: func(data string) string {
				return strings.Replace(data, `"expectedIdentities":{"foreign-config-runner":"provider:foreign-config-runner"}`, `"expectedIdentities":{"foreign-config-runner":"provider:foreign-config-runner","foreign-config-runner":"provider:other"}`, 1)
			},
			want: `duplicate expected identity name "foreign-config-runner"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			ledger, err := OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			canonical := currentControlPlaneRecoveryFixture(t)
			fixture := []byte(test.mutate(string(canonical)))
			if string(fixture) == string(canonical) {
				t.Fatal("test mutation did not change the fixture")
			}
			if err := os.WriteFile(ledger.Path(), fixture, 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := OpenControlPlaneRecoveryLedger(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported-shape open error = %v, want %q", err, test.want)
			} else if strings.Contains(err.Error(), "checksum") {
				t.Fatalf("unsupported-shape error reached checksum verification: %v", err)
			}
			got, err := os.ReadFile(ledger.Path())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(fixture) {
				t.Fatalf("unsupported-shape fixture was rewritten: got %s, want %s", got, fixture)
			}
		})
	}
}

func currentControlPlaneRecoveryFixture(t *testing.T) []byte {
	t.Helper()
	disk := controlPlaneRecoveryDiskState{
		SchemaVersion: controlPlaneRecoveryStateVersion,
		Generation:    7,
		State: ControlPlaneRecoveryState{
			Attempts:                     2,
			NextAttemptAt:                time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
			AdmissionIncident:            true,
			RecoveryReservationToken:     41,
			AdmissionIncidentToken:       42,
			RecoveryReservationPhase:     RecoveryReservationReserved,
			RecoveryReservationExpiresAt: time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
			ExpectedIdentities: map[string]string{
				"foreign-config-runner": "provider:foreign-config-runner",
			},
		},
	}
	sum, err := controlPlaneRecoveryChecksum(disk)
	if err != nil {
		t.Fatal(err)
	}
	disk.Checksum = hex.EncodeToString(sum)
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
