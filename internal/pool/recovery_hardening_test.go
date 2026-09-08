package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	poolstate "github.com/solutionforest/ephemeral-action-runner/internal/pool/state"
	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
)

func seedRecoveryIdentity(t *testing.T, store *poolstate.Store, name, providerID string) {
	t.Helper()
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-sandboxes", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionCreateIntent}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{
		Action:     poolstate.ActionCreated,
		ProviderID: providerID,
		Receipt:    poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"` + providerID + `"}`)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionTimeoutQuarantinesAndRetainsUncertainCreateCapacity(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	providerDouble := &fakeProvider{}
	lifecycle := &partialCreateLifecycle{
		Lifecycle: provider.AdaptLegacy(providerDouble),
		create: func() (provider.Instance, error) {
			return provider.Instance{}, provider.NewUncertainCreateAdmissionFailure("create Docker Sandboxes instance", context.DeadlineExceeded)
		},
	}
	manager := newRegisteredTestManager(t, providerDouble, nil)
	manager.Config.Provider.Type = "docker-sandboxes"
	manager.Lifecycle = lifecycle
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.LifecycleState = store
	const name = "epar-test-uncertain-create"

	vm, err := manager.provisionOne(context.Background(), name, false, false)
	if err == nil || !errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("provisionOne() error = %v, want typed admission failure", err)
	}
	if !vm.CreateOutcomeUncertain || vm.Phase != LifecycleQuarantined {
		t.Fatalf("provisioned instance = %#v, want uncertain quarantined capacity", vm)
	}
	if got := atomic.LoadInt32(&providerDouble.listCalls); got != 0 {
		t.Fatalf("provider inventory calls during uncertain rollback = %d, want 0", got)
	}

	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseQuarantined || record.ProviderID != "" || record.Quarantine == nil || !strings.HasPrefix(record.Quarantine.Reason, createOutcomeUncertainReason) {
		t.Fatalf("uncertain lifecycle record = %#v", record)
	}

	active, err := manager.reconcileLocalInventoryWithContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	retained, found := active[name]
	if !found || !retained.CreateOutcomeUncertain || retained.Phase != LifecycleQuarantined {
		t.Fatalf("reconciled uncertain capacity = %#v, found=%t", retained, found)
	}

	active, err = manager.reconcilePhysicalPool(context.Background(), active, false)
	if err != nil {
		t.Fatal(err)
	}
	retained, found = active[name]
	if !found || retained.Phase != LifecycleQuarantined {
		t.Fatalf("unregistered reconciliation released uncertain capacity = %#v, found=%t", retained, found)
	}

	if err := manager.cleanupLifecycleRecordWithRemoteAbsence(context.Background(), record, nil, true); err == nil || !strings.Contains(err.Error(), "outcome remains uncertain") {
		t.Fatalf("uncertain cleanup error = %v, want absence-only tombstone refusal", err)
	}
	record, err = store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseQuarantined {
		t.Fatalf("uncertain record phase after refused cleanup = %s, want quarantined", record.Phase)
	}
}

func TestInterruptedCreatingRecordRetainsCapacityAfterManagerRestart(t *testing.T) {
	providerDouble := &fakeProvider{}
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-interrupted-create"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{
		Name:         name,
		ProviderType: "docker-sandboxes",
		GitHub:       poolstate.GitHubIdentity{ExactName: name},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionCreateIntent}); err != nil {
		t.Fatal(err)
	}

	// Reopen the same durable namespace to model a controller restart after
	// the create intent was committed but before quarantine could be saved.
	reopened, err := poolstate.Open(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	manager := newRegisteredTestManager(t, providerDouble, nil)
	manager.Config.Provider.Type = "docker-sandboxes"
	manager.LifecycleState = reopened

	active, err := manager.reconcileLocalInventoryWithContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	vm, found := active[name]
	if !found || !vm.ProviderOwned || !vm.CreateOutcomeUncertain || vm.Phase != LifecycleQuarantined {
		t.Fatalf("reconciled interrupted create = %#v, found=%t", vm, found)
	}
	interrupted, err := reopened.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.cleanupLifecycleRecordWithRemoteAbsence(context.Background(), interrupted, nil, true); err == nil || !strings.Contains(err.Error(), "outcome remains uncertain") {
		t.Fatalf("interrupted create cleanup error = %v, want durable uncertainty fence", err)
	}
}

func TestGenericQuarantinePreservesUncertainCreateFence(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "epar-test-generic-quarantine"
	if _, err := store.Reserve(context.Background(), poolstate.CreateSpec{Name: name, ProviderType: "docker-sandboxes", GitHub: poolstate.GitHubIdentity{ExactName: name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionCreateIntent}); err != nil {
		t.Fatal(err)
	}
	manager := Manager{LifecycleState: store}
	if err := manager.quarantineLifecycle(context.Background(), name, errors.New("generic provider failure")); err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseQuarantined || !record.CreateOutcomeUncertain || !isUncertainCreateRecord(record) {
		t.Fatalf("generic quarantine record = %#v, want durable uncertain fence", record)
	}
	if err := manager.cleanupLifecycleRecordWithRemoteAbsence(context.Background(), record, nil, true); err == nil || !strings.Contains(err.Error(), "outcome remains uncertain") {
		t.Fatalf("generic quarantine cleanup error = %v, want uncertainty fence", err)
	}
}

func TestHistoricalUncertainCreateQuarantineReasonsRemainFenced(t *testing.T) {
	for _, reason := range []string{interruptedCreateNoIdentityReason, interruptedCreateGitHubIdentityReason} {
		t.Run(reason, func(t *testing.T) {
			record := poolstate.Record{Phase: poolstate.PhaseQuarantined, Quarantine: &poolstate.Quarantine{Reason: reason}}
			if !isUncertainCreateRecord(record) {
				t.Fatalf("isUncertainCreateRecord(%q) = false, want true", reason)
			}
		})
	}
}

func TestImmediateAdmissionFailureDoesNotCreateUncertainCapacityFence(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	providerDouble := &fakeProvider{}
	lifecycle := &partialCreateLifecycle{
		Lifecycle: provider.AdaptLegacy(providerDouble),
		create: func() (provider.Instance, error) {
			return provider.Instance{}, provider.NewControlPlaneAdmissionFailure("create Docker Sandboxes instance", errors.New("failed to run sandbox container"))
		},
	}
	manager := newRegisteredTestManager(t, providerDouble, nil)
	manager.Config.Provider.Type = "docker-sandboxes"
	manager.Lifecycle = lifecycle
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.LifecycleState = store

	vm, err := manager.provisionOne(context.Background(), "epar-test-immediate-admission", false, false)
	if err == nil || !errors.Is(err, provider.ErrControlPlaneAdmissionFailure) {
		t.Fatalf("provisionOne() error = %v, want typed admission failure", err)
	}
	if errors.Is(err, provider.ErrCreateOutcomeUncertain) {
		t.Fatalf("provisionOne() error = %v, immediate admission failure became uncertain", err)
	}
	if vm.CreateOutcomeUncertain {
		t.Fatalf("provisioned instance = %#v, want no uncertain capacity fence", vm)
	}
	record, err := store.Read(context.Background(), "epar-test-immediate-admission")
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseTombstoned {
		t.Fatalf("immediate admission lifecycle record phase = %s, want ordinary rollback tombstone", record.Phase)
	}
}

type changingInventoryLifecycle struct {
	provider.Lifecycle
	snapshots [][]provider.InventoryItem
	index     int
}

func recoveryInventoryItem(name, providerID string) provider.InventoryItem {
	return provider.InventoryItem{
		Instance: provider.Instance{Name: name, ProviderID: providerID, State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
		Workspaces: []string{
			filepath.Join("/tmp", name),
		},
	}
}

func (lifecycle *changingInventoryLifecycle) Inventory(context.Context) ([]provider.InventoryItem, error) {
	if lifecycle.index >= len(lifecycle.snapshots) {
		return lifecycle.snapshots[len(lifecycle.snapshots)-1], nil
	}
	items := lifecycle.snapshots[lifecycle.index]
	lifecycle.index++
	return items, nil
}

func TestProviderRecoveryRejectsChangingInventoryIdentitySets(t *testing.T) {
	cfg := configForRecoveryHardeningTest()
	first := provider.InventoryItem{Instance: provider.Instance{Name: "epar-test-one", ProviderID: "provider:one", State: "running", Source: "shell"}, State: "running", Source: "shell", Workspaces: []string{"/tmp/epar-test-one"}}
	second := provider.InventoryItem{Instance: provider.Instance{Name: "epar-test-one", ProviderID: "provider:two", State: "running", Source: "shell"}, State: "running", Source: "shell", Workspaces: []string{"/tmp/epar-test-one"}}
	manager := Manager{
		Config:    cfg,
		Lifecycle: &changingInventoryLifecycle{snapshots: [][]provider.InventoryItem{{first}, {second}, {second}}},
	}

	if err := manager.verifyProviderInventoryAfterRecovery(context.Background()); err == nil || !strings.Contains(err.Error(), "changed the provider identity set") {
		t.Fatalf("stable inventory verification error = %v, want identity-set change", err)
	}
}

func TestProviderInventoryEvidenceRejectsIncompleteOrInconsistentItems(t *testing.T) {
	valid := provider.InventoryItem{Instance: provider.Instance{Name: "epar-test-one", ProviderID: "provider:one", State: "running", Source: "shell"}, State: "running", Source: "shell", Workspaces: []string{"/tmp/epar-test-one"}}
	tests := []struct {
		name string
		item provider.InventoryItem
	}{
		{name: "missing provider id", item: func() provider.InventoryItem { item := valid; item.Instance.ProviderID = ""; return item }()},
		{name: "missing state", item: func() provider.InventoryItem { item := valid; item.State = ""; return item }()},
		{name: "inconsistent state", item: func() provider.InventoryItem { item := valid; item.State = "stopped"; return item }()},
		{name: "inconsistent source", item: func() provider.InventoryItem { item := valid; item.Source = "vm"; return item }()},
		{name: "missing workspace", item: func() provider.InventoryItem { item := valid; item.Workspaces = nil; return item }()},
		{name: "duplicate workspace", item: func() provider.InventoryItem {
			item := valid
			item.Workspaces = []string{"/tmp/epar-test-one", "/tmp/epar-test-one"}
			return item
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := providerInventoryEvidenceSet([]provider.InventoryItem{test.item}); err == nil {
				t.Fatal("providerInventoryEvidenceSet() succeeded, want invalid safety evidence")
			}
		})
	}
}

func TestProviderInventoryEvidenceIgnoresWorkspaceOrdering(t *testing.T) {
	first := provider.InventoryItem{Instance: provider.Instance{Name: "epar-test-one", ProviderID: "provider:one", State: "running", Source: "shell"}, State: "running", Source: "shell", Workspaces: []string{"/tmp/b", "/tmp/a"}}
	second := first
	second.Workspaces = []string{"/tmp/a", "/tmp/b"}
	left, err := providerInventoryEvidenceSet([]provider.InventoryItem{first})
	if err != nil {
		t.Fatal(err)
	}
	right, err := providerInventoryEvidenceSet([]provider.InventoryItem{second})
	if err != nil {
		t.Fatal(err)
	}
	if !sameProviderInventoryEvidenceSet(left, right) {
		t.Fatalf("workspace ordering changed safety evidence: left=%v right=%v", left, right)
	}
}

func TestRecoveryPersistsForeignPrefixCensusAndFencesItsOmission(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	local := recoveryInventoryItem("epar-test-local-runner", "provider:local-runner")
	foreign := recoveryInventoryItem("another-config-runner", "provider:foreign-runner")
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventorySets: [][]provider.InventoryItem{
			{local, foreign}, // required pre-intervention host-wide census
			{local},          // every post-recovery view omits the foreign identity
			{local},
			{local},
		},
	}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		providerRecoveryLedger: ledger,
	}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || recoveryErr != nil {
		t.Fatalf("foreign-prefix recovery = handled %t error %v, want handled verification retry", handled, recoveryErr)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.ExpectedIdentities[local.Instance.Name] != local.Instance.ProviderID || state.ExpectedIdentities[foreign.Instance.Name] != foreign.Instance.ProviderID {
		t.Fatalf("durable host-wide census = %#v, want local and foreign exact identities", state.ExpectedIdentities)
	}
	if state.RecoveryReservationPhase != provider.RecoveryReservationVerifying || state.RecoveryReservationToken == 0 {
		t.Fatalf("post-omission recovery state = %#v, want retained verification-only reservation", state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 || lifecycle.inventoryCalls != 2 {
		t.Fatalf("foreign omission calls = intervention=%d inventory=%d, want one census, one failed verification, and one intervention", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestDurableReportOnlyCensusOmissionNeverTriggersSecondIntervention(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	const (
		name       = "another-config-report-only"
		providerID = "provider:report-only"
	)
	if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
		state.Attempts = 1
		state.NextAttemptAt = now.Add(-time.Second)
		state.RecoveryReservationToken = 31
		state.RecoveryReservationPhase = provider.RecoveryReservationVerifying
		state.RecoveryReservationExpiresAt = now.Add(-time.Second)
		state.ExpectedIdentities = map[string]string{name: providerID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		providerRecoveryLedger: ledger,
		now:                    func() time.Time { return now },
	}

	for attempt := 0; attempt < 2; attempt++ {
		handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
		if !handled || recoveryErr != nil {
			t.Fatalf("verification-only attempt %d = handled %t error %v", attempt+1, handled, recoveryErr)
		}
		now = now.Add(2 * time.Minute)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.ExpectedIdentities[name] != providerID || state.RecoveryReservationPhase != provider.RecoveryReservationVerifying || state.RecoveryReservationToken == 0 {
		t.Fatalf("retained report-only recovery census = %#v", state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 2 {
		t.Fatalf("verification-only omission calls = intervention=%d inventory=%d, want two probes and no second intervention", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestFailedPreInterventionCensusDoesNotRecoverControlPlane(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("census unavailable"))},
	}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		providerRecoveryLedger: ledger,
	}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "host-wide Docker Sandboxes recovery identity census") {
		t.Fatalf("failed pre-intervention census = handled %t error %v, want handled census failure", handled, recoveryErr)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.RecoveryReservationToken != 0 || state.Attempts != 0 || len(state.ExpectedIdentities) != 0 || state.NextAttemptAt.IsZero() {
		t.Fatalf("failed census durable state = %#v, want durable backoff without reservation or census", state)
	}
	handled, recoveryErr = manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || recoveryErr != nil {
		t.Fatalf("failed census cooldown = handled %t error %v, want handled durable backoff", handled, recoveryErr)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.inventoryCalls != 1 || lifecycle.recoverCalls != 0 {
		t.Fatalf("failed census calls = inventory=%d intervention=%d, want one inventory and no intervention", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestReportOnlyDiscoveryMustAppearInPreInterventionCensus(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		name       = "another-config-report-only-discovery"
		providerID = "provider:report-only-discovery"
	)
	if _, err := store.ReportUnknown(context.Background(), poolstate.Discovery{
		ProviderType: "docker-sandboxes",
		ProviderID:   providerID,
		ExactName:    name,
		Receipt:      poolstate.Receipt{Version: "v1", Payload: []byte(`{"providerId":"provider:report-only-discovery"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		LifecycleState:         store,
		providerRecoveryLedger: ledger,
	}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneAdmissionFailure)
	if !handled || recoveryErr == nil || !strings.Contains(recoveryErr.Error(), name) || !strings.Contains(recoveryErr.Error(), providerID) {
		t.Fatalf("report-only census omission = handled %t error %v, want exact discovery omission", handled, recoveryErr)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.inventoryCalls != 1 || lifecycle.recoverCalls != 0 {
		t.Fatalf("report-only census omission calls = inventory=%d intervention=%d, want one census and no intervention", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestPreInterventionResetRequiresCompleteDurableCensus(t *testing.T) {
	for _, test := range []struct {
		name        string
		inventory   []provider.InventoryItem
		wantCleared bool
		wantError   bool
	}{
		{name: "complete census clears reserved state", inventory: []provider.InventoryItem{recoveryInventoryItem("another-config-runner", "provider:foreign")}, wantCleared: true},
		{name: "omitted identity retains reserved state", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			ledger, err := provider.OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
			if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
				state.Attempts = 1
				state.NextAttemptAt = now.Add(-time.Second)
				state.RecoveryReservationToken = 37
				state.RecoveryReservationPhase = provider.RecoveryReservationReserved
				state.RecoveryReservationExpiresAt = now.Add(-time.Second)
				state.ExpectedIdentities = map[string]string{"another-config-runner": "provider:foreign"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			lifecycle := &controlPlaneRecoveryLifecycle{inventoryItems: test.inventory}
			manager := Manager{
				Config:                 configForRecoveryHardeningTest(),
				Lifecycle:              lifecycle,
				providerRecoveryLedger: ledger,
				now:                    func() time.Time { return now },
			}

			handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
			if !handled || (recoveryErr != nil) != test.wantError {
				t.Fatalf("pre-intervention reset = handled %t error %v, want error=%t", handled, recoveryErr, test.wantError)
			}
			state, err := ledger.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCleared {
				if state.RecoveryReservationToken != 0 || state.RecoveryReservationPhase != provider.RecoveryReservationNone || len(state.ExpectedIdentities) != 0 {
					t.Fatalf("safe reset state = %#v, want cleared reservation and census", state)
				}
			} else if state.RecoveryReservationToken != 37 || state.RecoveryReservationPhase != provider.RecoveryReservationReserved || state.ExpectedIdentities["another-config-runner"] != "provider:foreign" || !state.NextAttemptAt.After(now) {
				t.Fatalf("unsafe reset state = %#v, want retained reservation and census", state)
			}
			lifecycle.mu.Lock()
			defer lifecycle.mu.Unlock()
			if lifecycle.inventoryCalls != 1 || lifecycle.recoverCalls != 0 {
				t.Fatalf("pre-intervention reset calls = inventory=%d intervention=%d, want one inventory and no intervention", lifecycle.inventoryCalls, lifecycle.recoverCalls)
			}
		})
	}
}

func TestReportOnlyInstancesAreExcludedFromRunnerLivenessProbes(t *testing.T) {
	tests := []struct {
		name string
		vm   ProvisionedInstance
		want bool
	}{
		{name: "unowned report-only", vm: ProvisionedInstance{Phase: LifecycleQuarantined}, want: false},
		{name: "owned identityless report-only", vm: ProvisionedInstance{Phase: LifecycleQuarantined, ProviderOwned: true}, want: false},
		{name: "owned uncertain create", vm: ProvisionedInstance{Phase: LifecycleQuarantined, ProviderOwned: true, CreateOutcomeUncertain: true}, want: false},
		{name: "owned recovery inventory uncertain", vm: ProvisionedInstance{Phase: LifecycleQuarantined, ProviderOwned: true, ProviderID: "provider:one", RecoveryInventoryUncertain: true}, want: false},
		{name: "owned exact identity", vm: ProvisionedInstance{Phase: LifecycleQuarantined, ProviderOwned: true, ProviderID: "provider:one"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldProbeRunnerLiveness(test.vm); got != test.want {
				t.Fatalf("shouldProbeRunnerLiveness(%#v) = %t, want %t", test.vm, got, test.want)
			}
		})
	}
}

func TestInterruptedRecoveryReservationFencesDurableIdentityBeforeReconciliation(t *testing.T) {
	for _, phase := range []provider.RecoveryReservationPhase{
		provider.RecoveryReservationIntervening,
		provider.RecoveryReservationVerifying,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			store, err := poolstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const (
				name       = "epar-test-recovery-restart"
				providerID = "provider:recovery-restart"
			)
			seedRecoveryIdentity(t, store, name, providerID)

			ledger, err := provider.OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
			if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
				state.Attempts = 1
				state.NextAttemptAt = now.Add(-time.Second)
				state.RecoveryReservationToken = 17
				state.RecoveryReservationPhase = phase
				state.RecoveryReservationExpiresAt = now.Add(-time.Second)
				state.ExpectedIdentities = map[string]string{name: providerID}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			lifecycle := &controlPlaneRecoveryLifecycle{}
			manager := Manager{
				Config:                 configForRecoveryHardeningTest(),
				Lifecycle:              lifecycle,
				LifecycleState:         store,
				providerRecoveryLedger: ledger,
				now:                    func() time.Time { return now },
			}
			if err := manager.waitForProviderRecoveryWindow(context.Background()); err != nil {
				t.Fatal(err)
			}

			record, err := store.Read(context.Background(), name)
			if err != nil {
				t.Fatal(err)
			}
			if record.Phase != poolstate.PhaseQuarantined || !record.RecoveryInventoryUncertain {
				t.Fatalf("post-restart lifecycle record = %#v, want durable recovery-inventory fence", record)
			}
			reconciled, err := manager.reconcileLocalInventoryWithContext(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			vm, found := reconciled[name]
			if !found || vm.ProviderID != providerID || !vm.ProviderOwned || !vm.RecoveryInventoryUncertain || vm.Phase != LifecycleQuarantined {
				t.Fatalf("reconciled durable capacity = %#v found=%t, want exact quarantined slot", vm, found)
			}
			state, err := ledger.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.RecoveryReservationToken == 0 || state.RecoveryReservationPhase != provider.RecoveryReservationVerifying || !state.NextAttemptAt.After(now) || state.ExpectedIdentities[name] != providerID {
				t.Fatalf("post-verification recovery state = %#v, want verification-only fenced cooldown with retained census", state)
			}
			lifecycle.mu.Lock()
			defer lifecycle.mu.Unlock()
			if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 2 {
				t.Fatalf("restart recovery invoked inventory=%d intervention=%d, want verification plus reconciliation and no second intervention", lifecycle.inventoryCalls, lifecycle.recoverCalls)
			}
		})
	}
}

func TestRecoveryDoesNotInterveneOnEmptyCensusWhenDurableIdentityIsMissing(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		name       = "epar-test-empty-recovery-inventory"
		providerID = "provider:empty-recovery-inventory"
	)
	seedRecoveryIdentity(t, store, name, providerID)
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		LifecycleState:         store,
		providerRecoveryLedger: ledger,
	}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if !handled || recoveryErr == nil {
		t.Fatalf("empty inventory recovery = handled %t error %v; want handled fail-closed census error", handled, recoveryErr)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseCreated || record.RecoveryInventoryUncertain {
		t.Fatalf("empty inventory recovery record = %#v, want unchanged durable capacity before provider intervention", record)
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 0 || state.RecoveryReservationToken != 0 || len(state.ExpectedIdentities) != 0 || state.NextAttemptAt.IsZero() {
		t.Fatalf("empty inventory recovery state = %#v, want durable backoff without a reservation or successful census", state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 1 {
		t.Fatalf("empty inventory recovery calls = intervention=%d inventory=%d, want one incomplete pre-intervention probe and no intervention", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestRecoveryInterventionFailureFencesDurableIdentity(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		name       = "epar-test-failed-recovery-fence"
		providerID = "provider:failed-recovery-fence"
	)
	seedRecoveryIdentity(t, store, name, providerID)
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("wedged"))},
		inventorySets: [][]provider.InventoryItem{{recoveryInventoryItem(name, providerID)}},
		recoverErr:    errors.New("daemon intervention failed after crossing its boundary"),
	}
	manager := Manager{
		Config:                 configForRecoveryHardeningTest(),
		Lifecycle:              lifecycle,
		LifecycleState:         store,
		providerRecoveryLedger: ledger,
	}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if !handled || recoveryErr != nil {
		t.Fatalf("failed intervention recovery = handled %t error %v; want handled fail-closed retry", handled, recoveryErr)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != poolstate.PhaseQuarantined || !record.RecoveryInventoryUncertain {
		t.Fatalf("failed intervention record = %#v, want quarantined durable identity fence", record)
	}
	recoveryState, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recoveryState.ExpectedIdentities[name] != providerID || recoveryState.RecoveryReservationPhase != provider.RecoveryReservationVerifying || recoveryState.RecoveryReservationToken == 0 {
		t.Fatalf("failed intervention recovery state = %#v, want retained census and verification-only reservation", recoveryState)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 || lifecycle.inventoryCalls != 2 {
		t.Fatalf("failed intervention calls = intervention=%d inventory=%d, want one initial probe, one census, and one intervention", lifecycle.recoverCalls, lifecycle.inventoryCalls)
	}
}

func TestExpiredReservedRecoveryReservationMayRetryIntervention(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
		state.Attempts = 1
		state.NextAttemptAt = now.Add(-time.Second)
		state.RecoveryReservationToken = 19
		state.RecoveryReservationPhase = provider.RecoveryReservationReserved
		state.RecoveryReservationExpiresAt = now.Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{
		inventoryErrs: []error{provider.NewControlPlaneFailure("inventory Docker Sandboxes", errors.New("still unavailable")), nil, nil, nil},
	}
	manager := Manager{Config: configForRecoveryHardeningTest(), Lifecycle: lifecycle, providerRecoveryLedger: ledger, now: func() time.Time { return now }}
	handled, err := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if err != nil || !handled {
		t.Fatalf("expired reserved recovery = handled %t error %v, want successful retry", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 1 || lifecycle.inventoryCalls != 5 {
		t.Fatalf("expired reserved recovery invoked inventory=%d intervention=%d, want one recheck, census, retry, and stable verification", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestExactInventoryObservationClearsDurableAndInMemoryRecoveryUncertainty(t *testing.T) {
	store, err := poolstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		name       = "epar-test-recovery-observed"
		providerID = "provider:recovery-observed"
	)
	seedRecoveryIdentity(t, store, name, providerID)
	if _, err := store.Transition(context.Background(), name, poolstate.Transition{Action: poolstate.ActionRecoveryInventoryUncertain, Reason: recoveryInventoryUncertainReason}); err != nil {
		t.Fatal(err)
	}
	item := provider.InventoryItem{
		Instance: provider.Instance{Name: name, ProviderID: providerID, State: "running", Source: "shell"},
		State:    "running",
		Source:   "shell",
	}
	manager := Manager{
		Config:         configForRecoveryHardeningTest(),
		Lifecycle:      &changingInventoryLifecycle{snapshots: [][]provider.InventoryItem{{item}}},
		LifecycleState: store,
	}
	known := map[string]ProvisionedInstance{name: {
		Name:                       name,
		ProviderID:                 providerID,
		ProviderOwned:              true,
		Phase:                      LifecycleQuarantined,
		RecoveryInventoryUncertain: true,
	}}
	reconciled, err := manager.reconcileLocalInventoryWithContext(context.Background(), known)
	if err != nil {
		t.Fatal(err)
	}
	if vm := reconciled[name]; vm.RecoveryInventoryUncertain {
		t.Fatalf("reconciled instance = %#v, exact positive inventory left in-memory uncertainty set", vm)
	}
	record, err := store.Read(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecoveryInventoryUncertain {
		t.Fatalf("lifecycle record = %#v, exact positive inventory left durable uncertainty set", record)
	}
	stale := reconciled[name]
	stale.RecoveryInventoryUncertain = true
	reconciled[name] = stale
	reconciled, err = manager.reconcileLocalInventoryWithContext(context.Background(), reconciled)
	if err != nil {
		t.Fatal(err)
	}
	if vm := reconciled[name]; vm.RecoveryInventoryUncertain {
		t.Fatalf("reconciled instance = %#v, stale in-memory uncertainty overrode cleared durable state", vm)
	}
}

func TestProviderRecoveryBudgetSurvivesManagerRestart(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if _, err := ledger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
		state.Attempts = 2
		state.NextAttemptAt = next
		state.AdmissionIncident = true
		state.AdmissionIncidentToken = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{providerRecoveryLedger: reopened, now: func() time.Time { return next.Add(-time.Minute) }}
	if err := manager.loadProviderRecoveryState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.providerRecoveryTries != 2 || !manager.providerRecoveryNext.Equal(next) || !manager.providerAdmissionRecoveryAttempted {
		t.Fatalf("restarted manager recovery state = tries=%d next=%s admission=%t", manager.providerRecoveryTries, manager.providerRecoveryNext, manager.providerAdmissionRecoveryAttempted)
	}
	if manager.providerRecoveryWindowReady() {
		t.Fatal("restarted manager ignored durable cooldown")
	}
	before, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	permitted, incident, err := manager.reserveProviderRecoveryWithContext(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if permitted || !incident {
		t.Fatalf("restarted admission reservation = permitted %t incident %t, want suppressed incident", permitted, incident)
	}
	after, err := reopened.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("suppressed reservation mutated durable state from %#v to %#v", before, after)
	}
}

func TestProviderRecoveryBeginPersistsCrashSafetyWindow(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	manager := Manager{providerRecoveryLedger: ledger, now: func() time.Time { return now }}
	attempt, err := manager.beginProviderRecoveryAttemptWithContext(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
	restartedLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	state, err := restartedLedger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 1 || !state.NextAttemptAt.Equal(now.Add(10*time.Minute)) || state.RecoveryReservationToken == 0 {
		t.Fatalf("durable in-flight recovery state = %#v", state)
	}
}

func TestProviderRecoveryBeginRefusesWindowWonByAnotherManager(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	firstLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	secondLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	first := Manager{providerRecoveryLedger: firstLedger, now: func() time.Time { return now }}
	second := Manager{providerRecoveryLedger: secondLedger, now: func() time.Time { return now }}

	attempt, err := first.beginProviderRecoveryAttemptWithContext(context.Background(), 10*time.Minute)
	if err != nil || attempt != 1 {
		t.Fatalf("first recovery attempt = %d, %v; want attempt 1", attempt, err)
	}
	if _, err := second.beginProviderRecoveryAttemptWithContext(context.Background(), 10*time.Minute); !errors.Is(err, errProviderRecoveryWindowNotReady) {
		t.Fatalf("second recovery attempt error = %v, want durable cooldown refusal", err)
	}
	state, err := firstLedger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 1 || !state.NextAttemptAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("cross-manager recovery state = %#v, want first reservation only", state)
	}
}

func TestProviderRecoveryStaleReservationCannotClearNewerAttempt(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	firstLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	secondLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	first := Manager{providerRecoveryLedger: firstLedger, now: func() time.Time { return now }}
	second := Manager{providerRecoveryLedger: secondLedger, now: func() time.Time { return now }}
	firstReservation, err := first.beginProviderRecoveryReservationWithContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	secondReservation, err := second.beginProviderRecoveryReservationWithContext(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if firstReservation.reservationToken == 0 || secondReservation.reservationToken == firstReservation.reservationToken {
		t.Fatalf("reservation tokens = first=%d second=%d, want distinct nonzero tokens", firstReservation.reservationToken, secondReservation.reservationToken)
	}
	if applied, err := first.completeProviderRecoveryReservationWithContext(context.Background(), firstReservation); err != nil || applied {
		t.Fatalf("stale completion = applied %t, error %v; want no-op", applied, err)
	}
	if _, _, applied, err := first.recordProviderRecoveryFailureForReservationWithContext(context.Background(), firstReservation); err != nil || applied {
		t.Fatalf("stale failure record = applied %t, error %v; want no-op", applied, err)
	}
	state, err := firstLedger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 2 || state.RecoveryReservationToken != secondReservation.reservationToken {
		t.Fatalf("state after stale reservation mutations = %#v, want newer reservation intact", state)
	}
}

func TestProviderAdmissionResolutionCannotClearNewerIncident(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	firstLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	secondLedger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	first := Manager{providerRecoveryLedger: firstLedger}
	second := Manager{providerRecoveryLedger: secondLedger}
	permitted, incident, err := first.reserveProviderRecoveryWithContext(context.Background(), true)
	if err != nil || !permitted || incident {
		t.Fatalf("first admission reservation = permitted %t incident %t error %v", permitted, incident, err)
	}
	firstToken := first.providerAdmissionRecoveryToken
	if firstToken == 0 {
		t.Fatal("first admission reservation did not persist a token")
	}
	if _, err := secondLedger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
		state.AdmissionIncident = false
		state.AdmissionIncidentToken = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	permitted, incident, err = second.reserveProviderRecoveryWithContext(context.Background(), true)
	if err != nil || !permitted || incident {
		t.Fatalf("second admission reservation = permitted %t incident %t error %v", permitted, incident, err)
	}
	secondToken := second.providerAdmissionRecoveryToken
	if secondToken == 0 || secondToken == firstToken {
		t.Fatalf("admission tokens = first=%d second=%d, want distinct nonzero tokens", firstToken, secondToken)
	}
	if err := first.resetProviderAdmissionRecoveryWithContext(context.Background(), firstToken); err != nil {
		t.Fatal(err)
	}
	state, err := firstLedger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.AdmissionIncident || state.AdmissionIncidentToken != secondToken {
		t.Fatalf("state after stale admission resolution = %#v, want newer incident intact", state)
	}
}

func TestSuccessfulCreateRefreshesAdmissionTokenAndPreservesNewerIncident(t *testing.T) {
	for _, test := range []struct {
		name                       string
		publishNewerDuringCreate   bool
		wantIncident               bool
		wantAdmissionIncidentToken uint64
	}{
		{name: "refreshes cached state and rearms recovery"},
		{name: "newer incident survives stale successful create", publishNewerDuringCreate: true, wantIncident: true, wantAdmissionIncidentToken: 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EPAR_STATE_HOME", t.TempDir())
			managerLedger, err := provider.OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			otherLedger, err := provider.OpenControlPlaneRecoveryLedger()
			if err != nil {
				t.Fatal(err)
			}
			providerDouble := &fakeProvider{}
			manager := newRegisteredTestManager(t, providerDouble, nil)
			manager.Config.Provider.Type = "docker-sandboxes"
			manager.providerRecoveryLedger = managerLedger
			if err := manager.loadProviderRecoveryState(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := otherLedger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
				state.AdmissionIncident = true
				state.AdmissionIncidentToken = 41
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			store, err := poolstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager.LifecycleState = store
			const name = "epar-test-admission-token-refresh"
			manager.Lifecycle = &partialCreateLifecycle{
				Lifecycle: provider.AdaptLegacy(providerDouble),
				create: func() (provider.Instance, error) {
					if err := providerDouble.Clone(context.Background(), "image", name); err != nil {
						return provider.Instance{}, err
					}
					if test.publishNewerDuringCreate {
						if _, err := otherLedger.Update(context.Background(), func(state *provider.ControlPlaneRecoveryState) error {
							state.AdmissionIncident = true
							state.AdmissionIncidentToken = 42
							return nil
						}); err != nil {
							return provider.Instance{}, err
						}
					}
					return provider.Instance{
						Name:           name,
						ProviderID:     "fake:" + name,
						Source:         "image",
						State:          "running",
						ReceiptVersion: "v1",
						Receipt:        []byte(`{"providerId":"fake:` + name + `"}`),
					}, nil
				},
			}
			if _, err := manager.provisionOne(context.Background(), name, false, false); err != nil {
				t.Fatal(err)
			}
			state, err := otherLedger.State(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.AdmissionIncident != test.wantIncident || state.AdmissionIncidentToken != test.wantAdmissionIncidentToken {
				t.Fatalf("post-create admission state = %#v, want incident=%t token=%d", state, test.wantIncident, test.wantAdmissionIncidentToken)
			}
		})
	}
}

func TestProviderRecoveryCancellationDoesNotRestartOrMutateLedger(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{}
	cfg := configForRecoveryHardeningTest()
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{Config: cfg, Lifecycle: lifecycle, providerRecoveryLedger: ledger}
	before, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, cause := range []error{provider.ErrControlPlaneFailure, errors.Join(provider.ErrControlPlaneFailure, context.Canceled)} {
		handled, recoveryErr := manager.recoverProviderControlPlane(ctx, cause)
		if handled || recoveryErr != nil {
			t.Fatalf("cancelled recovery cause %v = handled %t error %v; want no-op", cause, handled, recoveryErr)
		}
	}
	state, err := ledger.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, before) {
		t.Fatalf("cancelled recovery changed durable state from %#v to %#v", before, state)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 0 {
		t.Fatalf("cancelled recovery invoked inventory=%d recovery=%d; want both zero", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestUncertainCreateFailureDoesNotAuthorizeControlPlaneRecovery(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	lifecycle := &controlPlaneRecoveryLifecycle{}
	manager := Manager{Config: configForRecoveryHardeningTest(), Lifecycle: lifecycle}
	cause := provider.NewUncertainCreateFailure(
		"create Docker Sandboxes instance",
		provider.NewControlPlaneFailure("identity readback", context.DeadlineExceeded),
	)

	handled, err := manager.recoverProviderControlPlane(context.Background(), cause)
	if err != nil || handled {
		t.Fatalf("uncertain create recovery = handled %t error %v; want no recovery", handled, err)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 0 {
		t.Fatalf("uncertain create invoked inventory=%d recovery=%d; want both zero", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestRecoverySupervisorLedgerFailureUsesInMemoryBackoff(t *testing.T) {
	t.Setenv("EPAR_STATE_HOME", t.TempDir())
	ledger, err := provider.OpenControlPlaneRecoveryLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.Path(), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := &controlPlaneRecoveryLifecycle{}
	manager := Manager{Config: configForRecoveryHardeningTest(), Lifecycle: lifecycle, providerRecoveryLedger: ledger}

	handled, recoveryErr := manager.recoverProviderControlPlane(context.Background(), provider.ErrControlPlaneFailure)
	if !handled || recoveryErr == nil {
		t.Fatalf("corrupt recovery ledger = handled %t error %v; want handled error", handled, recoveryErr)
	}
	if !manager.providerRecoveryNext.After(time.Now()) {
		t.Fatalf("in-memory recovery cooldown = %s, want future backoff", manager.providerRecoveryNext)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.recoverCalls != 0 || lifecycle.inventoryCalls != 0 {
		t.Fatalf("corrupt recovery ledger invoked inventory=%d recovery=%d; want both zero", lifecycle.inventoryCalls, lifecycle.recoverCalls)
	}
}

func TestProviderAdmissionRecoveryResetIsDockerSandboxesExclusiveOnly(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		recovery   string
		wantActive bool
	}{
		{name: "docker sandboxes exclusive auto", provider: "docker-sandboxes", recovery: config.DockerSandboxesRecoveryModeExclusiveAuto, wantActive: true},
		{name: "docker sandboxes observe", provider: "docker-sandboxes", recovery: config.DockerSandboxesRecoveryModeObserve, wantActive: false},
		{name: "docker container", provider: "docker-container", recovery: config.DockerSandboxesRecoveryModeExclusiveAuto, wantActive: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := Manager{Config: config.Config{Provider: config.ProviderConfig{Type: test.provider}, DockerSandboxes: config.DockerSandboxesConfig{RecoveryMode: test.recovery}}}
			if got := manager.dockerSandboxesExclusiveRecovery(); got != test.wantActive {
				t.Fatalf("dockerSandboxesExclusiveRecovery() = %t, want %t", got, test.wantActive)
			}
		})
	}
}

func configForRecoveryHardeningTest() config.Config {
	cfg := config.Default()
	cfg.Provider.Type = "docker-sandboxes"
	return cfg
}
