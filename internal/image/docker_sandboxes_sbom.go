package image

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/solutionforest/ephemeral-action-runner/internal/storage"
)

const sbomMaximumJSONTokenBytes = 16 * storage.MiB

// Bound decoder token allocations and recursion before decoding. This pass
// neither normalizes nor drops bytes; unsupported inputs fail explicitly.
func checkSBOMJSONLexicalLimits(input io.Reader) error {
	reader := bufio.NewReader(input)
	var tokenBytes uint64
	var depth int
	var inString, escaped bool
	for {
		r, size, err := reader.ReadRune()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if r == utf8.RuneError && size == 1 {
			return fmt.Errorf("SBOM JSON contains invalid UTF-8")
		}
		if inString {
			tokenBytes += uint64(size)
			if tokenBytes > sbomMaximumJSONTokenBytes {
				return fmt.Errorf("SBOM JSON token exceeds %d bytes", sbomMaximumJSONTokenBytes)
			}
			if escaped {
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
				tokenBytes = 0
			}
			continue
		}
		switch r {
		case '"':
			inString = true
			tokenBytes = 0
		case '{', '[':
			depth++
			tokenBytes = 0
			if depth > 256 {
				return fmt.Errorf("SBOM JSON nesting exceeds 256 levels")
			}
		case '}', ']':
			depth--
			tokenBytes = 0
		case ',', ':', ' ', '\t', '\r', '\n':
			tokenBytes = 0
		default:
			tokenBytes += uint64(size)
			if tokenBytes > sbomMaximumJSONTokenBytes {
				return fmt.Errorf("SBOM JSON token exceeds %d bytes", sbomMaximumJSONTokenBytes)
			}
		}
	}
}

// copyDockerSandboxesSBOMStatement preserves the locked scanner's complete
// in-toto/SPDX output byte-for-byte. Template metadata binds its hash to the
// verified archive; EPAR does not invent or rewrite the scanner's subjects.
func copyDockerSandboxesSBOMStatement(source, destination string, maximumBytes uint64) error {
	if maximumBytes == 0 || maximumBytes > storage.GiB {
		return fmt.Errorf("invalid Docker Sandboxes SBOM size limit %d", maximumBytes)
	}
	before, err := storage.SnapshotFilesystemTarget(source)
	if err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || uint64(info.Size()) > maximumBytes {
		return fmt.Errorf("SBOM statement has invalid size %d", info.Size())
	}
	output, err := os.CreateTemp(filepath.Dir(destination), ".sbom-statement-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	n, err := io.Copy(output, io.LimitReader(input, info.Size()+1))
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("SBOM statement changed size during export")
	}
	after, err := storage.SnapshotFilesystemTarget(source)
	if err != nil {
		return err
	}
	if before.Identity != after.Identity || before.Fingerprint != after.Fingerprint {
		return fmt.Errorf("SBOM statement changed during export")
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := validateInTotoSPDX(output.Name()); err != nil {
		return fmt.Errorf("validate complete scanner SPDX statement: %w", err)
	}
	if err := os.Chmod(output.Name(), 0o644); err != nil {
		return err
	}
	return replaceAtomicFile(output.Name(), destination)
}
