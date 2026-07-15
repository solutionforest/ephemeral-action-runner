// Package logging provides the manager logger, command transcript writers, and
// conservative log-retention primitives used by ephemeral-action-runner.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const (
	// ManagerFilename is the active manager log in the logging root.
	ManagerFilename = "epar.log"
	// LastErrorFilename is the stable latest-error report in the logging root.
	LastErrorFilename = "epar-last-error.log"
	defaultMaxSizeMiB = 100
	defaultMaxBackups = 3
)

// Format selects a structured or human-readable line representation.
type Format string

const (
	FormatText                         Format = "text"
	FormatJSON                         Format = "json"
	DefaultManagerConsoleTextFormat           = "{time} [{level}] {message}"
	DefaultTranscriptConsoleTextFormat        = "{time} {stream} {instance} {component} {message}{attributes}"
)

// Sinks is a bitset describing where a log stream is written.
type Sinks uint8

const (
	SinkNone    Sinks = 0
	SinkConsole Sinks = 1 << iota
	SinkFile
	SinkBoth = SinkConsole | SinkFile
)

// Has reports whether every requested sink is enabled.
func (sinks Sinks) Has(sink Sinks) bool { return sinks&sink == sink }

// Rotation configures Lumberjack v2. Retention age and aggregate-size policy
// are deliberately separate from per-file rotation.
type Rotation struct {
	MaxSizeMiB int
	MaxBackups int
	Compress   bool
}

func (rotation Rotation) withDefaults() Rotation {
	if rotation.MaxSizeMiB == 0 {
		rotation.MaxSizeMiB = defaultMaxSizeMiB
		if rotation.MaxBackups == 0 {
			rotation.MaxBackups = defaultMaxBackups
		}
	}
	return rotation
}

func (rotation Rotation) validate() error {
	if rotation.MaxSizeMiB < 0 {
		return errors.New("rotation max size must not be negative")
	}
	if rotation.MaxBackups < 0 {
		return errors.New("rotation max backups must not be negative")
	}
	return nil
}

// Options configures a reusable logging runtime. A nil output writer uses the
// corresponding process stream. Level defaults to INFO.
type Options struct {
	Directory                   string
	ManagerSinks                Sinks
	ManagerConsoleFormat        Format
	ManagerConsoleTextFormat    string
	ManagerFileFormat           Format
	TranscriptSinks             Sinks
	TranscriptConsoleFormat     Format
	TranscriptConsoleTextFormat string
	Rotation                    Rotation
	Level                       slog.Leveler
	Stdout                      io.Writer
	Stderr                      io.Writer
	Now                         func() time.Time
}

func (options Options) normalized() (Options, error) {
	if options.Directory == "" {
		return Options{}, errors.New("logging directory is empty")
	}
	if options.ManagerSinks&^SinkBoth != 0 || options.TranscriptSinks&^SinkBoth != 0 {
		return Options{}, errors.New("logging sinks contain unsupported bits")
	}
	if options.ManagerConsoleFormat == "" {
		options.ManagerConsoleFormat = FormatText
	}
	if options.ManagerFileFormat == "" {
		options.ManagerFileFormat = FormatJSON
	}
	if options.TranscriptConsoleFormat == "" {
		options.TranscriptConsoleFormat = FormatText
	}
	for label, format := range map[string]Format{
		"manager console":    options.ManagerConsoleFormat,
		"manager file":       options.ManagerFileFormat,
		"transcript console": options.TranscriptConsoleFormat,
	} {
		if format != FormatText && format != FormatJSON {
			return Options{}, fmt.Errorf("unsupported %s format %q", label, format)
		}
	}
	if err := validateTextTemplate("manager console", options.ManagerConsoleTextFormat, options.ManagerConsoleFormat, []string{"time", "level", "message", "attributes"}); err != nil {
		return Options{}, err
	}
	if err := validateTextTemplate("transcript console", options.TranscriptConsoleTextFormat, options.TranscriptConsoleFormat, []string{"time", "instance", "component", "stream", "message", "session", "category", "provider", "attributes"}); err != nil {
		return Options{}, err
	}
	if options.ManagerConsoleTextFormat == "" {
		options.ManagerConsoleTextFormat = DefaultManagerConsoleTextFormat
	}
	if options.TranscriptConsoleTextFormat == "" {
		options.TranscriptConsoleTextFormat = DefaultTranscriptConsoleTextFormat
	}
	if err := options.Rotation.validate(); err != nil {
		return Options{}, err
	}
	options.Rotation = options.Rotation.withDefaults()
	if options.Level == nil {
		options.Level = slog.LevelInfo
	}
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options, nil
}

func validateTextTemplate(label, template string, format Format, allowed []string) error {
	if template == "" {
		return nil
	}
	if format != FormatText {
		return fmt.Errorf("%s text format is supported only with text output", label)
	}
	if strings.ContainsAny(template, "\r\n") {
		return fmt.Errorf("%s text format must be a single line", label)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, placeholder := range allowed {
		allowedSet[placeholder] = struct{}{}
	}
	foundMessage := false
	remaining := template
	for {
		open := strings.IndexByte(remaining, '{')
		if open < 0 {
			if strings.ContainsRune(remaining, '}') {
				return fmt.Errorf("%s text format contains an unmatched closing brace", label)
			}
			break
		}
		if strings.ContainsRune(remaining[:open], '}') {
			return fmt.Errorf("%s text format contains an unmatched closing brace", label)
		}
		closeOffset := strings.IndexByte(remaining[open+1:], '}')
		if closeOffset < 0 {
			return fmt.Errorf("%s text format contains an unmatched opening brace", label)
		}
		placeholder := remaining[open+1 : open+1+closeOffset]
		if _, ok := allowedSet[placeholder]; !ok {
			return fmt.Errorf("%s text format contains unsupported placeholder {%s}", label, placeholder)
		}
		foundMessage = foundMessage || placeholder == "message"
		remaining = remaining[open+closeOffset+2:]
	}
	if !foundMessage {
		return fmt.Errorf("%s text format must contain {message}", label)
	}
	return nil
}

// Category is a shallow retention and path namespace.
type Category string

const (
	CategoryManager    Category = "manager"
	CategoryErrors     Category = "errors"
	CategoryInstances  Category = "instances"
	CategoryBuilds     Category = "builds"
	CategoryBenchmarks Category = "benchmarks"
)

func (category Category) validTranscriptCategory() bool {
	switch category {
	case CategoryErrors, CategoryInstances, CategoryBuilds, CategoryBenchmarks:
		return true
	default:
		return false
	}
}

// TranscriptMetadata supplies stable context for console envelopes and active
// session records. Attributes may contain additional low-cardinality context.
type TranscriptMetadata struct {
	SessionID  string            `json:"sessionId,omitempty"`
	Category   Category          `json:"category"`
	Instance   string            `json:"instance,omitempty"`
	Component  string            `json:"component,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}
