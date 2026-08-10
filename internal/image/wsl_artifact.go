package image

import (
	"errors"
	"fmt"
	"os"
)

const wslPreviousArtifactSuffix = ".epar-previous"

func activateWSLArtifact(candidatePath, outputPath string) (err error) {
	candidateSidecar := wslImageManifestSidecarPath(candidatePath)
	outputSidecar := wslImageManifestSidecarPath(outputPath)
	previousPath := outputPath + wslPreviousArtifactSuffix
	previousSidecar := outputSidecar + wslPreviousArtifactSuffix

	if err := requireRegularFile(candidatePath); err != nil {
		return fmt.Errorf("candidate archive: %w", err)
	}
	if err := requireRegularFile(candidateSidecar); err != nil {
		return fmt.Errorf("candidate manifest: %w", err)
	}
	if err := recoverWSLArtifactSwap(outputPath); err != nil {
		return err
	}

	hadOutput, err := regularFileExists(outputPath)
	if err != nil {
		return err
	}
	hadSidecar, err := regularFileExists(outputSidecar)
	if err != nil {
		return err
	}
	if hadOutput != hadSidecar {
		return fmt.Errorf("existing WSL artifact is incomplete: archive=%t manifest=%t", hadOutput, hadSidecar)
	}

	rollbackNeeded := false
	rollback := func() {
		_ = removeRegularFileIfPresent(outputPath)
		_ = removeRegularFileIfPresent(outputSidecar)
		if hadOutput {
			_ = os.Rename(previousPath, outputPath)
		}
		if hadSidecar {
			_ = os.Rename(previousSidecar, outputSidecar)
		}
	}
	defer func() {
		if err != nil && rollbackNeeded {
			rollback()
		}
	}()

	if hadOutput {
		if err = os.Rename(outputPath, previousPath); err != nil {
			return err
		}
		if err = os.Rename(outputSidecar, previousSidecar); err != nil {
			_ = os.Rename(previousPath, outputPath)
			return err
		}
	}
	rollbackNeeded = true
	if err = os.Rename(candidatePath, outputPath); err != nil {
		return err
	}
	if err = os.Rename(candidateSidecar, outputSidecar); err != nil {
		return err
	}
	if err = requireRegularFile(outputPath); err != nil {
		return err
	}
	if _, err = readStoredImageManifest(outputSidecar); err != nil {
		return err
	}
	rollbackNeeded = false
	if err = removeRegularFileIfPresent(previousPath); err != nil {
		return err
	}
	if err = removeRegularFileIfPresent(previousSidecar); err != nil {
		return err
	}
	return nil
}

func recoverWSLArtifactSwap(outputPath string) error {
	outputSidecar := wslImageManifestSidecarPath(outputPath)
	previousPath := outputPath + wslPreviousArtifactSuffix
	previousSidecar := outputSidecar + wslPreviousArtifactSuffix

	hasPrevious, err := regularFileExists(previousPath)
	if err != nil {
		return err
	}
	hasPreviousSidecar, err := regularFileExists(previousSidecar)
	if err != nil {
		return err
	}
	if !hasPrevious && !hasPreviousSidecar {
		return nil
	}
	hasOutput, err := regularFileExists(outputPath)
	if err != nil {
		return err
	}
	hasOutputSidecar, err := regularFileExists(outputSidecar)
	if err != nil {
		return err
	}
	if hasOutput && hasOutputSidecar {
		if _, readErr := readStoredImageManifest(outputSidecar); readErr == nil {
			if err := removeRegularFileIfPresent(previousPath); err != nil {
				return err
			}
			return removeRegularFileIfPresent(previousSidecar)
		}
	}
	if hasPrevious && !hasPreviousSidecar {
		if !hasOutput && hasOutputSidecar {
			return os.Rename(previousPath, outputPath)
		}
		return fmt.Errorf("incomplete WSL activation recovery evidence: previous archive without its manifest")
	}
	if !hasPrevious && hasPreviousSidecar {
		return fmt.Errorf("incomplete WSL activation recovery evidence: previous manifest without its archive")
	}

	if err := removeRegularFileIfPresent(outputPath); err != nil {
		return err
	}
	if err := removeRegularFileIfPresent(outputSidecar); err != nil {
		return err
	}
	if err := os.Rename(previousPath, outputPath); err != nil {
		return err
	}
	if err := os.Rename(previousSidecar, outputSidecar); err != nil {
		_ = os.Rename(outputPath, previousPath)
		return err
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}

func removeRegularFileIfPresent(path string) error {
	exists, err := regularFileExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return os.Remove(path)
}
