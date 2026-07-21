package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transcript exposes independent command stdout and stderr writers while
// preserving one raw, coalesced on-disk stream.
type Transcript struct {
	Stdout io.Writer
	Stderr io.Writer
	// File writes only to the raw rotating file and never emits a console
	// envelope. It is io.Discard when the file sink is disabled.
	File io.Writer

	options  Options
	metadata TranscriptMetadata
	file     *rotationHandle
	mu       sync.Mutex
	buffers  map[string][]byte
	closed   bool
	closeErr error
	onClose  func(*Transcript)
}

type transcriptStream struct {
	transcript *Transcript
	name       string
}

type transcriptEnvelope struct {
	Timestamp  string            `json:"timestamp"`
	Instance   string            `json:"instance,omitempty"`
	Component  string            `json:"component,omitempty"`
	Stream     string            `json:"stream"`
	Message    string            `json:"message"`
	SessionID  string            `json:"sessionId,omitempty"`
	Category   Category          `json:"category,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

func newTranscript(options Options, root, path string, metadata TranscriptMetadata) (*Transcript, error, error) {
	if metadata.SessionID == "" {
		sessionID, err := randomToken()
		if err != nil {
			return nil, nil, fmt.Errorf("create transcript session token: %w", err)
		}
		metadata.SessionID = sessionID
	}
	transcript := &Transcript{options: options, metadata: cloneMetadata(metadata), buffers: map[string][]byte{"stdout": nil, "stderr": nil}, File: io.Discard}
	if options.TranscriptSinks.Has(SinkFile) {
		file, err := openRotating(root, path, options.Rotation, metadata, options.Now())
		if err != nil {
			err = fmt.Errorf("open transcript: %w", err)
			if !options.TranscriptSinks.Has(SinkConsole) {
				return nil, nil, err
			}
			transcript.options.TranscriptSinks &^= SinkFile
			transcript.Stdout = &transcriptStream{transcript: transcript, name: "stdout"}
			transcript.Stderr = &transcriptStream{transcript: transcript, name: "stderr"}
			return transcript, err, nil
		}
		transcript.file = file
		transcript.File = file
	}
	transcript.Stdout = &transcriptStream{transcript: transcript, name: "stdout"}
	transcript.Stderr = &transcriptStream{transcript: transcript, name: "stderr"}
	return transcript, nil, nil
}

func (stream *transcriptStream) Write(data []byte) (int, error) {
	if stream == nil || stream.transcript == nil {
		return 0, errors.New("nil transcript stream")
	}
	return stream.transcript.write(stream.name, data)
}

func (transcript *Transcript) write(stream string, data []byte) (int, error) {
	transcript.mu.Lock()
	defer transcript.mu.Unlock()
	if transcript.closed {
		return 0, errors.New("write to closed transcript")
	}
	written := len(data)
	var writeErrors []error
	if transcript.file != nil {
		var err error
		written, err = transcript.file.Write(data)
		if err != nil {
			writeErrors = append(writeErrors, err)
		}
		if written != len(data) {
			writeErrors = append(writeErrors, io.ErrShortWrite)
		}
	}
	if !transcript.options.TranscriptSinks.Has(SinkConsole) {
		return written, errors.Join(writeErrors...)
	}
	transcript.buffers[stream] = append(transcript.buffers[stream], data...)
	for {
		newline := bytes.IndexByte(transcript.buffers[stream], '\n')
		if newline < 0 {
			break
		}
		line := append([]byte(nil), transcript.buffers[stream][:newline]...)
		transcript.buffers[stream] = transcript.buffers[stream][newline+1:]
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if err := transcript.writeEnvelope(stream, string(line)); err != nil {
			writeErrors = append(writeErrors, err)
		}
	}
	if len(writeErrors) > 0 {
		return written, errors.Join(writeErrors...)
	}
	return len(data), nil
}

func (transcript *Transcript) writeEnvelope(stream, message string) error {
	envelope := transcriptEnvelope{
		Timestamp:  transcript.options.Now().UTC().Format(time.RFC3339Nano),
		Instance:   transcript.metadata.Instance,
		Component:  transcript.metadata.Component,
		Stream:     stream,
		Message:    message,
		SessionID:  transcript.metadata.SessionID,
		Category:   transcript.metadata.Category,
		Provider:   transcript.metadata.Provider,
		Attributes: transcript.metadata.Attributes,
	}
	var data []byte
	var err error
	if transcript.options.TranscriptConsoleFormat == FormatJSON {
		data, err = json.Marshal(envelope)
		if err != nil {
			return fmt.Errorf("encode transcript console envelope: %w", err)
		}
		data = append(data, '\n')
	} else {
		data = formatTextEnvelope(envelope, transcript.options.TranscriptConsoleTextFormat)
	}
	target := transcript.options.Stdout
	if stream == "stderr" {
		target = transcript.options.Stderr
	}
	_, err = target.Write(data)
	return err
}

func formatTextEnvelope(envelope transcriptEnvelope, template string) []byte {
	keys := make([]string, 0, len(envelope.Attributes))
	for key := range envelope.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attributes := make([]string, 0, len(keys))
	for _, key := range keys {
		attributes = append(attributes, key+"="+quoteHumanString(envelope.Attributes[key]))
	}
	renderedAttributes := strings.Join(attributes, " ")
	if renderedAttributes != "" {
		renderedAttributes = " " + renderedAttributes
	}
	line := strings.NewReplacer(
		"{time}", envelope.Timestamp,
		"{instance}", singleLine(envelope.Instance),
		"{component}", singleLine(envelope.Component),
		"{stream}", singleLine(envelope.Stream),
		"{message}", singleLine(envelope.Message),
		"{session}", singleLine(envelope.SessionID),
		"{category}", singleLine(string(envelope.Category)),
		"{provider}", singleLine(envelope.Provider),
		"{attributes}", renderedAttributes,
	).Replace(template)
	return []byte(line + "\n")
}

// Close emits unterminated console fragments, closes the raw file, and
// releases active-path state. It is idempotent.
func (transcript *Transcript) Close() error {
	if transcript == nil {
		return nil
	}
	transcript.mu.Lock()
	if transcript.closed {
		err := transcript.closeErr
		transcript.mu.Unlock()
		return err
	}
	transcript.closed = true
	var closeErrors []error
	if transcript.options.TranscriptSinks.Has(SinkConsole) {
		for _, stream := range []string{"stdout", "stderr"} {
			if len(transcript.buffers[stream]) == 0 {
				continue
			}
			line := bytes.TrimSuffix(transcript.buffers[stream], []byte{'\r'})
			if err := transcript.writeEnvelope(stream, string(line)); err != nil {
				closeErrors = append(closeErrors, err)
			}
			transcript.buffers[stream] = nil
		}
	}
	if transcript.file != nil {
		if err := transcript.file.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	transcript.closeErr = errors.Join(closeErrors...)
	onClose := transcript.onClose
	err := transcript.closeErr
	transcript.mu.Unlock()
	if onClose != nil {
		onClose(transcript)
	}
	return err
}
