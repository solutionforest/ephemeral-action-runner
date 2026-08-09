package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/solutionforest/ephemeral-action-runner/internal/provider"
	storagecatalog "github.com/solutionforest/ephemeral-action-runner/internal/storage/catalog"
)

const dockerSandboxesActivationJournalSchema = 1

const (
	dockerSandboxesActivationPrepared         = "prepared"
	dockerSandboxesActivationImported         = "imported"
	dockerSandboxesActivationVerified         = "verified"
	dockerSandboxesActivationAdmissionBlocked = "admission-blocked"
	dockerSandboxesActivationActivated        = "activated"
	dockerSandboxesActivationReadBack         = "read-back"
	dockerSandboxesActivationReceiptPublished = "receipt-published"
	dockerSandboxesActivationCommitted        = "committed"
	dockerSandboxesActivationRolledBack       = "rolled-back"
)

type dockerSandboxesActivationJournal struct {
	SchemaVersion  int                     `json:"schemaVersion"`
	ConfigID       string                  `json:"configId"`
	Phase          string                  `json:"phase"`
	Candidate      dockerSandboxesReceipt  `json:"candidate"`
	Previous       *dockerSandboxesReceipt `json:"previous,omitempty"`
	ArchivePath    string                  `json:"archivePath,omitempty"`
	BuildWorkspace bool                    `json:"buildWorkspace,omitempty"`
	Imported       bool                    `json:"imported,omitempty"`
	StartedAt      time.Time               `json:"startedAt"`
	UpdatedAt      time.Time               `json:"updatedAt"`
	LastError      string                  `json:"lastError,omitempty"`
}

func (m *Coordinator) dockerSandboxesActivationJournalPath() (string, error) {
	receiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(receiptPath), "activation-journal.json"), nil
}

func (m *Coordinator) prepareDockerSandboxesActivationJournal(candidate dockerSandboxesReceipt, archivePath string, buildWorkspace bool) (dockerSandboxesActivationJournal, error) {
	journalPath, err := m.dockerSandboxesActivationJournalPath()
	if err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	if _, err := os.Lstat(journalPath); err == nil {
		return dockerSandboxesActivationJournal{}, fmt.Errorf("Docker Sandboxes activation journal already exists; reconcile it before starting another activation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return dockerSandboxesActivationJournal{}, err
	}
	configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	receiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	var previous *dockerSandboxesReceipt
	if receipt, readErr := readDockerSandboxesReceiptPath(receiptPath); readErr == nil {
		if evidenceErr := validateDockerSandboxesReceiptEvidence(receiptPath, receipt); evidenceErr != nil {
			return dockerSandboxesActivationJournal{}, fmt.Errorf("validate previous Docker Sandboxes receipt evidence before activation: %w", evidenceErr)
		}
		previous = &receipt
	} else if !errors.Is(readErr, os.ErrNotExist) {
		m.warnf("previous Docker Sandboxes receipt is not usable as rollback evidence: %v\n", readErr)
	}
	now := m.now().UTC()
	journal := dockerSandboxesActivationJournal{
		SchemaVersion:  dockerSandboxesActivationJournalSchema,
		ConfigID:       configID,
		Phase:          dockerSandboxesActivationPrepared,
		Candidate:      candidate,
		Previous:       previous,
		ArchivePath:    archivePath,
		BuildWorkspace: buildWorkspace,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := writeJSONFile(journalPath, journal); err != nil {
		return dockerSandboxesActivationJournal{}, fmt.Errorf("persist prepared Docker Sandboxes activation journal: %w", err)
	}
	if err := m.injectDockerSandboxesActivationFault(journal.Phase); err != nil {
		return journal, err
	}
	return journal, nil
}

func (m *Coordinator) updateDockerSandboxesActivationJournal(journal *dockerSandboxesActivationJournal, phase string, cause error) error {
	journal.Phase = phase
	journal.UpdatedAt = m.now().UTC()
	journal.LastError = ""
	if cause != nil {
		journal.LastError = cause.Error()
	}
	path, err := m.dockerSandboxesActivationJournalPath()
	if err != nil {
		return err
	}
	if err := writeJSONFile(path, *journal); err != nil {
		return fmt.Errorf("persist Docker Sandboxes activation phase %s: %w", phase, err)
	}
	return m.injectDockerSandboxesActivationFault(phase)
}

func (m *Coordinator) injectDockerSandboxesActivationFault(phase string) error {
	if m.DockerSandboxesActivationFault == nil {
		return nil
	}
	if err := m.DockerSandboxesActivationFault(phase); err != nil {
		return fmt.Errorf("injected Docker Sandboxes activation fault after %s: %w", phase, err)
	}
	return nil
}

func (m *Coordinator) removeDockerSandboxesActivationJournal() error {
	path, err := m.dockerSandboxesActivationJournalPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Coordinator) readDockerSandboxesActivationJournal() (dockerSandboxesActivationJournal, error) {
	path, err := m.dockerSandboxesActivationJournalPath()
	if err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	var journal dockerSandboxesActivationJournal
	if err := readJSONFile(path, &journal); err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	configID, err := storagecatalog.ConfigID(m.ProjectRoot, m.effectiveConfigPath())
	if err != nil {
		return dockerSandboxesActivationJournal{}, err
	}
	if journal.SchemaVersion != dockerSandboxesActivationJournalSchema || journal.ConfigID != configID || journal.Phase == "" || journal.Candidate.SchemaVersion != dockerSandboxesReceiptSchema || journal.Candidate.ManifestHash == "" || journal.Candidate.Artifact.Reference == "" || journal.Candidate.ActivatedAt.IsZero() || journal.StartedAt.IsZero() || journal.UpdatedAt.IsZero() {
		return dockerSandboxesActivationJournal{}, fmt.Errorf("invalid Docker Sandboxes activation journal")
	}
	return journal, nil
}

func sameDockerSandboxesReceipt(left, right dockerSandboxesReceipt) bool {
	return left.SchemaVersion == right.SchemaVersion && left.Distribution == right.Distribution && left.ManifestHash == right.ManifestHash && left.Artifact == right.Artifact && left.MetadataSHA256 == right.MetadataSHA256 && left.ArchiveSHA256 == right.ArchiveSHA256 && left.ArchiveBytes == right.ArchiveBytes && left.ActivatedAt.Equal(right.ActivatedAt)
}

func activateCommittedDockerSandboxesTemplate(runtime provider.TemplateArtifactRuntime, artifact provider.TemplateArtifact) error {
	controller, ok := runtime.(provider.TemplateArtifactActivationController)
	if !ok {
		return fmt.Errorf("docker-sandboxes provider is missing transactional template activation integration")
	}
	return controller.WithTemplateActivation(func() error {
		if err := runtime.ActivateTemplate(artifact); err != nil {
			return err
		}
		active, found := controller.ActiveTemplate()
		if !found || active != artifact {
			return fmt.Errorf("Docker Sandboxes committed-template activation readback failed")
		}
		return nil
	})
}

func (m *Coordinator) activateDockerSandboxesCandidateLocked(ctx context.Context, candidate dockerSandboxesReceipt, archivePath string, buildWorkspace bool, runtime provider.TemplateArtifactRuntime) (time.Time, error) {
	return m.activateDockerSandboxesCandidateWithPreflightLocked(ctx, candidate, archivePath, buildWorkspace, false, runtime)
}

func (m *Coordinator) activateDockerSandboxesCandidateWithPreflightLocked(ctx context.Context, candidate dockerSandboxesReceipt, archivePath string, buildWorkspace, alreadyVerified bool, runtime provider.TemplateArtifactRuntime) (time.Time, error) {
	controller, ok := runtime.(provider.TemplateArtifactActivationController)
	if !ok {
		return time.Time{}, fmt.Errorf("docker-sandboxes provider is missing transactional template activation integration")
	}
	receiptPath, err := m.dockerSandboxesReceiptPath()
	if err != nil {
		return time.Time{}, err
	}
	if err := validateDockerSandboxesReceiptEvidence(receiptPath, candidate); err != nil {
		return time.Time{}, fmt.Errorf("validate candidate Docker Sandboxes receipt evidence: %w", err)
	}
	journal, err := m.prepareDockerSandboxesActivationJournal(candidate, archivePath, buildWorkspace)
	if err != nil {
		return time.Time{}, err
	}
	if err := func() error {
		if alreadyVerified {
			return nil
		}
		return runtime.VerifyImportedTemplate(ctx, candidate.Artifact)
	}(); err != nil {
		if !errors.Is(err, provider.ErrTemplateNotFound) {
			return time.Time{}, err
		}
		if archivePath == "" {
			return time.Time{}, fmt.Errorf("candidate Docker Sandboxes template is missing and no verified archive is available for exact import")
		}
		if err := m.verifyDockerSandboxesActivationArchive(archivePath, candidate); err != nil {
			return time.Time{}, err
		}
		importPlan, planErr := m.dockerSandboxesImportStoragePlan(candidate.Source, candidate.ArchiveBytes)
		if planErr != nil {
			return time.Time{}, fmt.Errorf("plan Docker Sandboxes template-cache import storage: %w", planErr)
		}
		if err := m.preflightStorage(importPlan.OperationPlan); err != nil {
			return time.Time{}, err
		}
		label := fmt.Sprintf("Docker Sandboxes template-cache import for %s", candidate.Artifact.Reference)
		if err := m.runProgressOperation(label, nil, func() error { return runtime.ImportTemplate(ctx, archivePath) }); err != nil {
			return time.Time{}, err
		}
		journal.Imported = true
	}
	if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationImported, nil); err != nil {
		return time.Time{}, err
	}
	if err := runtime.VerifyImportedTemplate(ctx, candidate.Artifact); err != nil {
		return time.Time{}, fmt.Errorf("verify imported Docker Sandboxes runner template: %w", err)
	}
	if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationVerified, nil); err != nil {
		return time.Time{}, err
	}
	activated := false
	err = controller.WithTemplateActivation(func() error {
		if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationAdmissionBlocked, nil); err != nil {
			return err
		}
		if err := runtime.ActivateTemplate(candidate.Artifact); err != nil {
			return err
		}
		activated = true
		if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationActivated, nil); err != nil {
			return err
		}
		active, found := controller.ActiveTemplate()
		if !found || active != candidate.Artifact {
			return fmt.Errorf("Docker Sandboxes active-template readback does not match the candidate")
		}
		if err := runtime.VerifyImportedTemplate(ctx, candidate.Artifact); err != nil {
			return fmt.Errorf("read back activated Docker Sandboxes template: %w", err)
		}
		if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationReadBack, nil); err != nil {
			return err
		}
		if err := writeJSONFile(receiptPath, candidate); err != nil {
			return fmt.Errorf("publish Docker Sandboxes active receipt: %w", err)
		}
		if err := m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationReceiptPublished, nil); err != nil {
			return err
		}
		if err := m.recordCurrentSandboxArtifactLocked(candidate.Artifact, candidate.ManifestHash, candidate.ActivatedAt); err != nil {
			return fmt.Errorf("record current Docker Sandboxes template ownership: %w", err)
		}
		return m.updateDockerSandboxesActivationJournal(&journal, dockerSandboxesActivationCommitted, nil)
	})
	if err != nil {
		if activated {
			rollbackErr := controller.WithTemplateActivation(func() error {
				return m.rollbackDockerSandboxesActivationLocked(ctx, controller, runtime, &journal, err)
			})
			return time.Time{}, errors.Join(err, rollbackErr)
		}
		return time.Time{}, err
	}
	if err := m.removeDockerSandboxesActivationJournal(); err != nil {
		return time.Time{}, fmt.Errorf("complete Docker Sandboxes activation journal: %w", err)
	}
	return candidate.ActivatedAt, nil
}

func (m *Coordinator) verifyDockerSandboxesActivationArchive(path string, receipt dockerSandboxesReceipt) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect verified Docker Sandboxes archive: %w", err)
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != receipt.ArchiveBytes {
		return fmt.Errorf("verified Docker Sandboxes archive size changed before import")
	}
	digest, size, err := hashFile(path)
	if err != nil {
		return err
	}
	if digest != receipt.ArchiveSHA256 || size != receipt.ArchiveBytes {
		return fmt.Errorf("verified Docker Sandboxes archive digest changed before import")
	}
	return nil
}

func (m *Coordinator) rollbackDockerSandboxesActivationLocked(ctx context.Context, controller provider.TemplateArtifactActivationController, runtime provider.TemplateArtifactRuntime, journal *dockerSandboxesActivationJournal, cause error) error {
	var rollbackErr error
	candidateNeedsCustody := journal.Imported
	if journal.Previous == nil || journal.Previous.Artifact != journal.Candidate.Artifact {
		if !candidateNeedsCustody {
			if err := runtime.VerifyImportedTemplate(ctx, journal.Candidate.Artifact); err == nil {
				candidateNeedsCustody = true
			} else if !errors.Is(err, provider.ErrTemplateNotFound) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("determine candidate Docker Sandboxes template custody during rollback: %w", err))
			}
		}
	} else {
		candidateNeedsCustody = false
	}
	if journal.Previous != nil {
		if err := runtime.VerifyImportedTemplate(ctx, journal.Previous.Artifact); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify previous Docker Sandboxes template for rollback: %w", err))
		} else if err := runtime.ActivateTemplate(journal.Previous.Artifact); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore previous Docker Sandboxes template: %w", err))
		} else if active, found := controller.ActiveTemplate(); !found || active != journal.Previous.Artifact {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("previous Docker Sandboxes template rollback readback failed"))
		}
	} else if active, found := controller.ActiveTemplate(); found {
		if active != journal.Candidate.Artifact {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("active Docker Sandboxes template changed during rollback"))
		} else if err := controller.ClearActiveTemplate(journal.Candidate.Artifact); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	if rollbackErr == nil {
		receiptPath, err := m.dockerSandboxesReceiptPath()
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		} else if journal.Previous != nil {
			if err := writeJSONFile(receiptPath, *journal.Previous); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore previous Docker Sandboxes receipt: %w", err))
			} else if err := m.recordCurrentSandboxArtifactLocked(journal.Previous.Artifact, journal.Previous.ManifestHash, journal.Previous.ActivatedAt); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore previous Docker Sandboxes catalog ownership: %w", err))
			}
		} else {
			if err := os.Remove(receiptPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove uncommitted Docker Sandboxes receipt: %w", err))
			} else if err := m.releaseCatalogRole("provider-artifact", m.now().UTC()); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("release uncommitted Docker Sandboxes catalog ownership: %w", err))
			}
		}
		if rollbackErr == nil && candidateNeedsCustody {
			if err := m.recordSupersededSandboxArtifactLocked(journal.Candidate.Artifact, journal.Candidate.ManifestHash, m.now().UTC()); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("record rolled-back Docker Sandboxes candidate custody: %w", err))
			}
		}
	}
	if rollbackErr != nil {
		_ = m.updateDockerSandboxesActivationJournal(journal, journal.Phase, errors.Join(cause, rollbackErr))
		return rollbackErr
	}
	if err := m.updateDockerSandboxesActivationJournal(journal, dockerSandboxesActivationRolledBack, cause); err != nil {
		return err
	}
	return m.removeDockerSandboxesActivationJournal()
}

// recoverDockerSandboxesActivation reconciles one interrupted activation
// before current-receipt or update-policy decisions. An atomically published
// candidate receipt is authoritative and is committed idempotently; otherwise
// the previous receipt is restored.
func (m *Coordinator) recoverDockerSandboxesActivation(ctx context.Context, runtime provider.TemplateArtifactRuntime) error {
	journal, err := m.readDockerSandboxesActivationJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Docker Sandboxes activation journal: %w", err)
	}
	controller, ok := runtime.(provider.TemplateArtifactActivationController)
	if !ok {
		return fmt.Errorf("docker-sandboxes provider is missing transactional template activation integration")
	}
	return m.withSandboxBackendLock(ctx, func() error {
		return controller.WithTemplateActivation(func() error {
			receiptPath, pathErr := m.dockerSandboxesReceiptPath()
			if pathErr != nil {
				return pathErr
			}
			activeReceipt, receiptErr := readDockerSandboxesReceiptPath(receiptPath)
			if receiptErr == nil && sameDockerSandboxesReceipt(activeReceipt, journal.Candidate) {
				if err := runtime.VerifyImportedTemplate(ctx, journal.Candidate.Artifact); err != nil {
					if !errors.Is(err, provider.ErrTemplateNotFound) || journal.ArchivePath == "" {
						return fmt.Errorf("recover committed Docker Sandboxes template: %w", err)
					}
					if err := m.verifyDockerSandboxesActivationArchive(journal.ArchivePath, journal.Candidate); err != nil {
						return err
					}
					if err := runtime.ImportTemplate(ctx, journal.ArchivePath); err != nil {
						return err
					}
				}
				if err := runtime.VerifyImportedTemplate(ctx, journal.Candidate.Artifact); err != nil {
					return err
				}
				if err := runtime.ActivateTemplate(journal.Candidate.Artifact); err != nil {
					return err
				}
				if active, found := controller.ActiveTemplate(); !found || active != journal.Candidate.Artifact {
					return fmt.Errorf("recovered Docker Sandboxes template activation readback failed")
				}
				if err := m.recordCurrentSandboxArtifactLocked(journal.Candidate.Artifact, journal.Candidate.ManifestHash, journal.Candidate.ActivatedAt); err != nil {
					return err
				}
				return m.removeDockerSandboxesActivationJournal()
			}
			return m.rollbackDockerSandboxesActivationLocked(ctx, controller, runtime, &journal, errors.New("interrupted activation did not publish its candidate receipt"))
		})
	})
}
