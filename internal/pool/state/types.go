package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = 1

var (
	ErrAlreadyExists     = errors.New("pool lifecycle record already exists")
	ErrNotFound          = errors.New("pool lifecycle record not found")
	ErrInvalidRecord     = errors.New("invalid pool lifecycle record")
	ErrInvalidTransition = errors.New("invalid pool lifecycle transition")
	ErrCorrupt           = errors.New("pool lifecycle state is corrupt")
)

type Phase string

const (
	PhaseReserved          Phase = "reserved"
	PhaseCreating          Phase = "creating"
	PhaseCreated           Phase = "created"
	PhaseValidating        Phase = "validating"
	PhaseStandby           Phase = "standby"
	PhaseRegistering       Phase = "registering"
	PhaseReady             Phase = "ready"
	PhaseBusy              Phase = "busy"
	PhaseDraining          Phase = "draining"
	PhaseQuarantined       Phase = "quarantined"
	PhaseCleanupPending    Phase = "cleanup-pending"
	PhaseFencing           Phase = "fencing"
	PhaseFenced            Phase = "fenced"
	PhaseRemoteReconciling Phase = "remote-reconciling"
	PhaseRemoteAbsent      Phase = "remote-absent"
	PhaseLocalRemoving     Phase = "local-removing"
	PhaseLocalAbsent       Phase = "local-absent"
	PhaseTombstoned        Phase = "tombstoned"
)

type Action string

const (
	ActionCreateIntent       Action = "create-intent"
	ActionAbandonCreate      Action = "abandon-create"
	ActionCreated            Action = "created"
	ActionValidateIntent     Action = "validate-intent"
	ActionValidated          Action = "validated"
	ActionRegisterIntent     Action = "register-intent"
	ActionRegistered         Action = "registered"
	ActionJobStarted         Action = "job-started"
	ActionJobFinished        Action = "job-finished"
	ActionQuarantine         Action = "quarantine"
	ActionFenceIntent        Action = "fence-intent"
	ActionFenced             Action = "fenced"
	ActionVerifyRemoteIntent Action = "verify-remote-absent-intent"
	ActionRemoteAbsent       Action = "remote-absent"
	ActionRemoveLocalIntent  Action = "remove-local-intent"
	ActionLocalAbsent        Action = "local-absent"
	ActionCleanupPending     Action = "cleanup-pending"
	ActionResumeCleanup      Action = "resume-cleanup"
	ActionTombstone          Action = "tombstone"
)

// Receipt is provider-owned, versioned state. It is intentionally opaque to
// the shared lifecycle package and must not contain credentials.
type Receipt struct {
	Version string          `json:"version"`
	Payload json.RawMessage `json:"payload"`
}

type GitHubIdentity struct {
	ExactName string `json:"exactName"`
	RunnerID  int64  `json:"runnerId,omitempty"`
}

type Lease struct {
	Purpose   string    `json:"purpose"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Quarantine struct {
	Reason     string    `json:"reason"`
	ReportedAt time.Time `json:"reportedAt"`
}

type Cleanup struct {
	FenceIntentAt       *time.Time `json:"fenceIntentAt,omitempty"`
	RemoteVerifyAt      *time.Time `json:"remoteVerifyAt,omitempty"`
	RemoteAbsentAt      *time.Time `json:"remoteAbsentAt,omitempty"`
	LocalRemoveIntentAt *time.Time `json:"localRemoveIntentAt,omitempty"`
	LocalAbsentAt       *time.Time `json:"localAbsentAt,omitempty"`
}

type Record struct {
	Name         string         `json:"name"`
	ProviderType string         `json:"providerType"`
	ProviderID   string         `json:"providerId,omitempty"`
	Receipt      Receipt        `json:"receipt"`
	GitHub       GitHubIdentity `json:"github"`
	Phase        Phase          `json:"phase"`
	Leases       []Lease        `json:"leases"`
	Quarantine   *Quarantine    `json:"quarantine,omitempty"`
	Cleanup      Cleanup        `json:"cleanup"`
	Generation   uint64         `json:"generation"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
	TombstonedAt *time.Time     `json:"tombstonedAt,omitempty"`
}

type CreateSpec struct {
	Name         string
	ProviderType string
	GitHub       GitHubIdentity
}

type Transition struct {
	Action     Action
	ProviderID string
	Receipt    Receipt
	RunnerID   int64
	Reason     string
}

// Discovery stores an unowned provider resource. It is never a cleanup target.
type Discovery struct {
	ProviderType string    `json:"providerType"`
	ProviderID   string    `json:"providerId"`
	ExactName    string    `json:"exactName"`
	Receipt      Receipt   `json:"receipt"`
	ObservedAt   time.Time `json:"observedAt"`
}

func validateName(label, value string) error {
	if len(value) < 1 || len(value) > 128 || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: invalid %s", ErrInvalidRecord, label)
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return fmt.Errorf("%w: invalid %s", ErrInvalidRecord, label)
		}
	}
	return nil
}

func validateReceipt(receipt Receipt) error {
	if err := validateName("receipt version", receipt.Version); err != nil {
		return err
	}
	if len(receipt.Payload) == 0 || !json.Valid(receipt.Payload) {
		return fmt.Errorf("%w: receipt payload must be valid JSON", ErrInvalidRecord)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(receipt.Payload, &object); err != nil || object == nil {
		return fmt.Errorf("%w: receipt payload must be a JSON object", ErrInvalidRecord)
	}
	return nil
}

func cloneRecord(record Record) Record {
	record.Receipt.Payload = append(json.RawMessage(nil), record.Receipt.Payload...)
	record.Leases = append([]Lease(nil), record.Leases...)
	if record.Quarantine != nil {
		copy := *record.Quarantine
		record.Quarantine = &copy
	}
	return record
}
