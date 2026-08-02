package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/config"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const updatePolicyStateSchemaVersion = 1

// UpdatePolicyState is the per-configuration durable record for remote image
// and Actions runner freshness. Local desired inputs are represented by
// LocalInputHash and are never allowed to become stale merely because a remote
// check is not due.
type UpdatePolicyState struct {
	SchemaVersion         int                   `json:"schemaVersion"`
	LocalInputHash        string                `json:"localInputHash"`
	LastAttemptAt         time.Time             `json:"lastAttemptAt,omitempty"`
	LastSuccessfulCheckAt time.Time             `json:"lastSuccessfulCheckAt,omitempty"`
	NextEligibleAt        time.Time             `json:"nextEligibleAt,omitempty"`
	NextRetryAt           time.Time             `json:"nextRetryAt,omitempty"`
	ConsecutiveFailures   int                   `json:"consecutiveFailures,omitempty"`
	TimeZone              string                `json:"timeZone,omitempty"`
	PolicyFrequency       string                `json:"policyFrequency,omitempty"`
	PolicyTime            string                `json:"policyTime,omitempty"`
	LastResolvedManifest  *Manifest             `json:"lastResolvedManifest,omitempty"`
	LastResolvedSource    *ResolvedDockerSource `json:"lastResolvedSource,omitempty"`
	PendingManifest       *Manifest             `json:"pendingManifest,omitempty"`
	PendingSource         *ResolvedDockerSource `json:"pendingSource,omitempty"`
	DeferredReason        string                `json:"deferredReason,omitempty"`
	LastError             string                `json:"lastError,omitempty"`
}

// UpdatePolicyStatus is safe to expose through status and console output.
type UpdatePolicyStatus struct {
	Frequency             string
	UpdateTime            string
	LastSuccessfulCheckAt time.Time
	NextEligibleAt        time.Time
	NextRetryAt           time.Time
	Pending               bool
	PendingIdentity       string
	DeferredReason        string
	LastError             string
}

type RemoteUpdateCheck struct {
	Due             bool
	Changed         bool
	CurrentManifest *Manifest
	PendingManifest *Manifest
	NextEligibleAt  time.Time
	NextRetryAt     time.Time
}

func UpdatePolicyStatePath(projectRoot, configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(projectRoot, ".local", "config.yml")
	}
	configID, err := storagecatalog.ConfigID(projectRoot, configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectRoot, ".local", "state", "image", configID, "update-policy.json"), nil
}

func ReadUpdatePolicyState(projectRoot, configPath string) (UpdatePolicyState, error) {
	path, err := UpdatePolicyStatePath(projectRoot, configPath)
	if err != nil {
		return UpdatePolicyState{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return UpdatePolicyState{}, err
	}
	var state UpdatePolicyState
	if err := json.Unmarshal(content, &state); err != nil {
		return UpdatePolicyState{}, fmt.Errorf("parse image update state: %w", err)
	}
	if state.SchemaVersion != updatePolicyStateSchemaVersion {
		return UpdatePolicyState{}, fmt.Errorf("unsupported image update state schema %d", state.SchemaVersion)
	}
	return state, nil
}

func writeUpdatePolicyState(projectRoot, configPath string, state UpdatePolicyState) error {
	path, err := UpdatePolicyStatePath(projectRoot, configPath)
	if err != nil {
		return err
	}
	state.SchemaVersion = updatePolicyStateSchemaVersion
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicFile(path, append(content, '\n'), 0o600)
}

func (m *Coordinator) readUpdatePolicyState() (UpdatePolicyState, error) {
	state, err := ReadUpdatePolicyState(m.ProjectRoot, m.effectiveConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return UpdatePolicyState{SchemaVersion: updatePolicyStateSchemaVersion}, nil
	}
	return state, err
}

func (m *Coordinator) writeUpdatePolicyState(state UpdatePolicyState) error {
	return writeUpdatePolicyState(m.ProjectRoot, m.effectiveConfigPath(), state)
}

func bootstrapUpdatePolicyState(state *UpdatePolicyState, image config.ImageConfig, localManifest, resolvedManifest Manifest, source *ResolvedDockerSource, activatedAt time.Time, location *time.Location) (bool, error) {
	if state.LocalInputHash != "" || state.LastResolvedManifest != nil || activatedAt.IsZero() {
		return false, nil
	}
	localHash, err := ManifestHash(localManifest)
	if err != nil {
		return false, err
	}
	projected := resolvedManifest
	projected.SourceImage = localManifest.SourceImage
	projected.SourcePlatform = localManifest.SourcePlatform
	projected.SourceDigest = localManifest.SourceDigest
	projected.SourcePlatformDigest = localManifest.SourcePlatformDigest
	projected.RunnerSelector = localManifest.RunnerSelector
	projected.RunnerVersion = localManifest.RunnerVersion
	projected.RunnerAssetName = localManifest.RunnerAssetName
	projected.RunnerAssetURL = localManifest.RunnerAssetURL
	projected.RunnerAssetDigest = localManifest.RunnerAssetDigest
	projectedHash, err := ManifestHash(projected)
	if err != nil {
		return false, err
	}
	if projectedHash != localHash {
		return false, nil
	}
	if location == nil {
		location = time.Local
	}
	state.LocalInputHash = localHash
	state.LastResolvedManifest = &resolvedManifest
	if source != nil {
		copy := *source
		state.LastResolvedSource = &copy
	}
	if err := scheduleNextSuccess(state, image, activatedAt.In(location)); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Coordinator) UpdatePolicyStatus() (UpdatePolicyStatus, error) {
	state, err := m.readUpdatePolicyState()
	if err != nil {
		return UpdatePolicyStatus{}, err
	}
	recalculateScheduleForTimeZone(&state, m.Config.Image, time.Local)
	pendingIdentity := ""
	if state.PendingManifest != nil {
		hash, hashErr := ManifestHash(*state.PendingManifest)
		if hashErr != nil {
			return UpdatePolicyStatus{}, hashErr
		}
		pendingIdentity = shortUpdateIdentity(*state.PendingManifest, hash)
	}
	return UpdatePolicyStatus{
		Frequency:             m.Config.Image.UpdateFrequency,
		UpdateTime:            m.Config.Image.UpdateTime,
		LastSuccessfulCheckAt: state.LastSuccessfulCheckAt,
		NextEligibleAt:        state.NextEligibleAt,
		NextRetryAt:           state.NextRetryAt,
		Pending:               state.PendingManifest != nil,
		PendingIdentity:       pendingIdentity,
		DeferredReason:        state.DeferredReason,
		LastError:             state.LastError,
	}, nil
}

// CheckRemoteUpdate performs only the cheap immutable remote observation. It
// never builds, imports, activates, or retires an artifact.
func (m *Coordinator) CheckRemoteUpdate(ctx context.Context, now time.Time) (RemoteUpdateCheck, error) {
	state, err := m.readUpdatePolicyState()
	if err != nil {
		return RemoteUpdateCheck{}, err
	}
	if recalculateScheduleForTimeZone(&state, m.Config.Image, now.Location()) {
		if err := m.writeUpdatePolicyState(state); err != nil {
			return RemoteUpdateCheck{}, err
		}
	}
	if !updateCheckDue(state, m.Config.Image, now) {
		return RemoteUpdateCheck{
			CurrentManifest: state.LastResolvedManifest,
			PendingManifest: state.PendingManifest,
			NextEligibleAt:  state.NextEligibleAt,
			NextRetryAt:     state.NextRetryAt,
		}, nil
	}
	var (
		localManifest Manifest
		manifest      Manifest
		source        *ResolvedDockerSource
	)
	if m.Config.Provider.Type == "docker-sandboxes" {
		localManifest, err = m.dockerSandboxesLocalManifest(ctx)
		if err == nil {
			var resolved ResolvedDockerSource
			manifest, resolved, err = m.dockerSandboxesDesiredManifest(ctx)
			source = &resolved
		}
	} else {
		localManifest, err = m.desiredLocalImageManifest(ctx)
		if err == nil {
			manifest, err = m.resolveRemoteImageManifest(ctx, localManifest)
		}
	}
	if err != nil {
		scheduleUpdateFailure(&state, now, err)
		_ = m.writeUpdatePolicyState(state)
		return RemoteUpdateCheck{Due: true, NextRetryAt: state.NextRetryAt}, err
	}
	localHash, err := ManifestHash(localManifest)
	if err != nil {
		return RemoteUpdateCheck{}, err
	}
	if state.LocalInputHash != localHash {
		return RemoteUpdateCheck{}, fmt.Errorf("local image inputs changed while the pool was running; restart EPAR to apply the new configuration safely")
	}
	resolvedHash, err := ManifestHash(manifest)
	if err != nil {
		return RemoteUpdateCheck{}, err
	}
	currentHash := ""
	if state.LastResolvedManifest != nil {
		currentHash, err = ManifestHash(*state.LastResolvedManifest)
		if err != nil {
			return RemoteUpdateCheck{}, err
		}
	}
	if currentHash == resolvedHash {
		if err := scheduleNextSuccess(&state, m.Config.Image, now); err != nil {
			return RemoteUpdateCheck{}, err
		}
		state.PendingManifest = nil
		state.PendingSource = nil
		if err := m.writeUpdatePolicyState(state); err != nil {
			return RemoteUpdateCheck{}, err
		}
		return RemoteUpdateCheck{
			Due:             true,
			CurrentManifest: state.LastResolvedManifest,
			NextEligibleAt:  state.NextEligibleAt,
		}, nil
	}
	state.LastAttemptAt = now.UTC()
	state.PendingManifest = &manifest
	state.PendingSource = source
	state.DeferredReason = "waiting for the common pool maintenance drain"
	state.LastError = ""
	if err := m.writeUpdatePolicyState(state); err != nil {
		return RemoteUpdateCheck{}, err
	}
	return RemoteUpdateCheck{
		Due:             true,
		Changed:         true,
		CurrentManifest: state.LastResolvedManifest,
		PendingManifest: &manifest,
	}, nil
}

// ApplyPendingUpdate performs the build and activation selected by
// CheckRemoteUpdate after the common pool lifecycle has drained all runners.
func (m *Coordinator) ApplyPendingUpdate(ctx context.Context, now time.Time) error {
	state, err := m.readUpdatePolicyState()
	if err != nil {
		return err
	}
	if state.PendingManifest == nil {
		return nil
	}
	manifest := *state.PendingManifest
	if m.Config.Provider.Type == "docker-sandboxes" {
		if state.PendingSource == nil {
			return fmt.Errorf("pending Docker Sandboxes update is missing its immutable source observation")
		}
		err = m.ensureDockerSandboxesTemplateResolved(ctx, false, manifest, *state.PendingSource)
	} else {
		err = m.buildResolvedImage(ctx, manifest)
		if err == nil {
			hash, hashErr := ManifestHash(manifest)
			if hashErr != nil {
				err = hashErr
			} else if recordErr := m.recordCurrentArtifact(ctx, hash); recordErr != nil {
				err = recordErr
			}
		}
	}
	if err != nil {
		scheduleUpdateFailure(&state, now, err)
		state.DeferredReason = "scheduled build failed; the previous verified generation remains active"
		_ = m.writeUpdatePolicyState(state)
		return err
	}
	state.LastResolvedManifest = &manifest
	state.LastResolvedSource = state.PendingSource
	state.PendingManifest = nil
	state.PendingSource = nil
	if err := scheduleNextSuccess(&state, m.Config.Image, now); err != nil {
		return err
	}
	if err := m.writeUpdatePolicyState(state); err != nil {
		return err
	}
	return m.cleanupSupersededCatalog(ctx)
}

func (m *Coordinator) DeferPendingUpdate(reason string) error {
	state, err := m.readUpdatePolicyState()
	if err != nil {
		return err
	}
	if state.PendingManifest == nil {
		return nil
	}
	state.DeferredReason = reason
	return m.writeUpdatePolicyState(state)
}

func NextImageUpdateAt(last time.Time, frequency, wallClock string, location *time.Location) (time.Time, error) {
	if frequency == config.ImageUpdateFrequencyManual {
		return time.Time{}, nil
	}
	if location == nil {
		location = time.Local
	}
	clock, err := time.Parse("15:04", wallClock)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse image update time: %w", err)
	}
	localLast := last.In(location)
	year, month, day := localLast.Date()
	switch frequency {
	case config.ImageUpdateFrequencyDaily:
		day++
	case config.ImageUpdateFrequencyWeekly:
		day += 7
	case config.ImageUpdateFrequencyBiweekly:
		day += 14
	case config.ImageUpdateFrequencyMonthly:
		year, month, day = addCalendarMonthClamped(year, month, day)
	default:
		return time.Time{}, fmt.Errorf("unsupported image update frequency %q", frequency)
	}
	next := time.Date(year, month, day, clock.Hour(), clock.Minute(), 0, 0, location)
	return normalizeLocalWallClock(next, clock.Hour(), clock.Minute(), location), nil
}

func addCalendarMonthClamped(year int, month time.Month, day int) (int, time.Month, int) {
	targetMonth := month + 1
	targetYear := year
	if targetMonth > time.December {
		targetMonth = time.January
		targetYear++
	}
	lastDay := time.Date(targetYear, targetMonth+1, 0, 12, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return targetYear, targetMonth, day
}

// normalizeLocalWallClock handles DST gaps by selecting the first valid local
// instant after the requested wall-clock time. Repeated times use Go's earlier
// occurrence, which is deterministic and still runs only once because state is
// persisted immediately after the check.
func normalizeLocalWallClock(candidate time.Time, hour, minute int, location *time.Location) time.Time {
	local := candidate.In(location)
	if local.Hour() == hour && local.Minute() == minute {
		return candidate
	}
	for i := 0; i < 180; i++ {
		candidate = candidate.Add(time.Minute)
		local = candidate.In(location)
		if local.Hour() > hour || (local.Hour() == hour && local.Minute() >= minute) {
			return candidate
		}
	}
	return candidate
}

func updateCheckDue(state UpdatePolicyState, image config.ImageConfig, now time.Time) bool {
	if image.UpdateFrequency == config.ImageUpdateFrequencyManual {
		return false
	}
	if state.LastResolvedManifest != nil && !manifestHasMutableRemoteInputs(*state.LastResolvedManifest) {
		return false
	}
	if !state.NextRetryAt.IsZero() && now.Before(state.NextRetryAt) {
		return false
	}
	if state.LastSuccessfulCheckAt.IsZero() || state.NextEligibleAt.IsZero() {
		return true
	}
	return !now.Before(state.NextEligibleAt)
}

func pendingUpdateReady(state UpdatePolicyState, now time.Time) bool {
	if state.PendingManifest == nil {
		return false
	}
	return state.NextRetryAt.IsZero() || !now.Before(state.NextRetryAt)
}

func scheduleNextSuccess(state *UpdatePolicyState, image config.ImageConfig, now time.Time) error {
	location := now.Location()
	if location == nil || location == time.UTC {
		location = time.Local
	}
	next, err := NextImageUpdateAt(now, image.UpdateFrequency, image.UpdateTime, location)
	if err != nil {
		return err
	}
	if state.LastResolvedManifest != nil && !manifestHasMutableRemoteInputs(*state.LastResolvedManifest) {
		next = time.Time{}
	}
	state.LastAttemptAt = now.UTC()
	state.LastSuccessfulCheckAt = now.UTC()
	state.NextEligibleAt = next.UTC()
	state.NextRetryAt = time.Time{}
	state.ConsecutiveFailures = 0
	state.TimeZone = location.String()
	state.PolicyFrequency = image.UpdateFrequency
	state.PolicyTime = image.UpdateTime
	state.LastError = ""
	state.DeferredReason = ""
	return nil
}

func manifestHasMutableRemoteInputs(manifest Manifest) bool {
	if normalizedRunnerSelector(manifest.RunnerSelector) == "latest" {
		return true
	}
	if manifest.SourceType == config.ImageSourceDockerImage && !strings.Contains(strings.ToLower(manifest.SourceImage), "@sha256:") {
		return true
	}
	return false
}

func recalculateScheduleForTimeZone(state *UpdatePolicyState, image config.ImageConfig, location *time.Location) bool {
	if location == nil {
		location = time.Local
	}
	policyChanged := state.PolicyFrequency != image.UpdateFrequency || state.PolicyTime != image.UpdateTime
	if image.UpdateFrequency == config.ImageUpdateFrequencyManual {
		if !policyChanged && state.NextEligibleAt.IsZero() && state.TimeZone == location.String() {
			return false
		}
		state.PolicyFrequency = image.UpdateFrequency
		state.PolicyTime = image.UpdateTime
		state.TimeZone = location.String()
		state.NextEligibleAt = time.Time{}
		return true
	}
	if state.LastSuccessfulCheckAt.IsZero() {
		if !policyChanged && state.TimeZone == location.String() {
			return false
		}
		state.PolicyFrequency = image.UpdateFrequency
		state.PolicyTime = image.UpdateTime
		state.TimeZone = location.String()
		return true
	}
	if !policyChanged && state.TimeZone == location.String() {
		return false
	}
	next, err := NextImageUpdateAt(state.LastSuccessfulCheckAt, image.UpdateFrequency, image.UpdateTime, location)
	if err != nil {
		return false
	}
	state.TimeZone = location.String()
	state.PolicyFrequency = image.UpdateFrequency
	state.PolicyTime = image.UpdateTime
	state.NextEligibleAt = next.UTC()
	return true
}

func shortUpdateIdentity(manifest Manifest, hash string) string {
	parts := []string{"manifest=" + shortDigest(hash)}
	if manifest.SourcePlatformDigest != "" {
		parts = append(parts, "source="+shortDigest(manifest.SourcePlatformDigest))
	} else if manifest.SourceDigest != "" {
		parts = append(parts, "source="+shortDigest(manifest.SourceDigest))
	}
	if manifest.RunnerVersion != "" {
		parts = append(parts, "runner="+manifest.RunnerVersion)
	}
	return strings.Join(parts, " ")
}

func shortDigest(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "sha256:"))
	if len(value) > 16 {
		value = value[:16]
	}
	return value
}

func scheduleUpdateFailure(state *UpdatePolicyState, now time.Time, failure error) {
	state.LastAttemptAt = now.UTC()
	state.ConsecutiveFailures++
	delay := time.Hour
	for i := 1; i < state.ConsecutiveFailures && delay < 24*time.Hour; i++ {
		delay *= 2
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	state.NextRetryAt = now.Add(delay).UTC()
	state.LastError = failure.Error()
}

func formatUpdateTime(value time.Time) string {
	if value.IsZero() {
		return "manual"
	}
	return value.In(time.Local).Format("2006-01-02 15:04 MST")
}
