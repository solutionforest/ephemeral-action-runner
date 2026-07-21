package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Runtime owns the manager logger and all transcripts opened through it.
type Runtime struct {
	options     Options
	root        string
	manager     *slog.Logger
	managerFile *rotationHandle
	mu          sync.Mutex
	closed      bool
	transcripts map[*Transcript]struct{}
	warnings    []error
}

// NewRuntime constructs a manager logger and, when configured, opens epar.log.
func NewRuntime(options Options) (*Runtime, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	root, err := canonicalPath(options.Directory)
	if err != nil {
		return nil, fmt.Errorf("canonicalize logging directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create logging directory: %w", err)
	}
	if err := ensureSafeDirectory(root, 0o755); err != nil {
		return nil, fmt.Errorf("validate logging directory: %w", err)
	}
	runtime := &Runtime{options: options, root: root, transcripts: make(map[*Transcript]struct{})}
	var managerFile *rotationHandle
	if options.ManagerSinks.Has(SinkFile) {
		managerFile, err = openRotating(root, filepath.Join(root, ManagerFilename), options.Rotation, TranscriptMetadata{SessionID: "manager", Category: CategoryManager, Component: "manager"}, options.Now())
		if err != nil {
			err = fmt.Errorf("open manager log: %w", err)
			if !options.ManagerSinks.Has(SinkConsole) {
				return nil, err
			}
			runtime.warnings = append(runtime.warnings, err)
			writeInitializationWarning(options.Stderr, err)
			options.ManagerSinks &^= SinkFile
			runtime.options.ManagerSinks &^= SinkFile
		} else {
			runtime.managerFile = managerFile
		}
	}
	runtime.manager = slog.New(managerHandler(options, managerFile))
	return runtime, nil
}

// Manager returns the structured manager logger.
func (runtime *Runtime) Manager() *slog.Logger { return runtime.manager }

// Logger is an alias for Manager for call sites that only use one logger.
func (runtime *Runtime) Logger() *slog.Logger { return runtime.manager }

// Directory returns the canonical logging root.
func (runtime *Runtime) Directory() string { return runtime.root }

// InitializationWarnings returns defensive sink fallbacks encountered by the
// runtime. The returned slice is a copy.
func (runtime *Runtime) InitializationWarnings() []error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]error(nil), runtime.warnings...)
}

// OpenTranscript opens a coalesced raw transcript and separate contextual
// stdout/stderr writers. The path must match the metadata category and one of
// the recognized shallow current-file patterns.
func (runtime *Runtime) OpenTranscript(metadata TranscriptMetadata, path string) (*Transcript, error) {
	if !metadata.Category.validTranscriptCategory() {
		return nil, fmt.Errorf("unsupported transcript category %q", metadata.Category)
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, err
	}
	recognized, ok := recognizePath(runtime.root, canonical)
	if !ok || recognized.category != metadata.Category || recognized.backup || recognized.current {
		return nil, fmt.Errorf("transcript path %s does not match category %q or a recognized current-file pattern", canonical, metadata.Category)
	}

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil, os.ErrClosed
	}
	transcript, warning, err := newTranscript(runtime.options, runtime.root, canonical, metadata)
	if err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	if warning != nil {
		runtime.warnings = append(runtime.warnings, warning)
		writeInitializationWarning(runtime.options.Stderr, warning)
	}
	transcript.onClose = runtime.removeTranscript
	runtime.transcripts[transcript] = struct{}{}
	runtime.mu.Unlock()
	return transcript, nil
}

func writeInitializationWarning(writer io.Writer, err error) {
	_, _ = fmt.Fprintf(writer, "warning: logging file sink disabled: %v\n", err)
}

func (runtime *Runtime) removeTranscript(transcript *Transcript) {
	runtime.mu.Lock()
	delete(runtime.transcripts, transcript)
	runtime.mu.Unlock()
}

// Close flushes partial transcript lines, closes every open transcript, and
// releases the manager log's active-path lock.
func (runtime *Runtime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil
	}
	runtime.closed = true
	transcripts := make([]*Transcript, 0, len(runtime.transcripts))
	for transcript := range runtime.transcripts {
		transcripts = append(transcripts, transcript)
	}
	runtime.mu.Unlock()

	errorsFound := make([]error, 0, len(transcripts)+1)
	for _, transcript := range transcripts {
		if err := transcript.Close(); err != nil {
			errorsFound = append(errorsFound, err)
		}
	}
	if runtime.managerFile != nil {
		if err := runtime.managerFile.Close(); err != nil {
			errorsFound = append(errorsFound, err)
		}
	}
	return errors.Join(errorsFound...)
}
