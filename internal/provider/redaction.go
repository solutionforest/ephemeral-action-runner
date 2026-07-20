package provider

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)([[:alnum:]_.-]*(?:token|password|secret|private(?:[_-]?key)?)[[:alnum:]_.-]*=)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^[:space:]]+)`)

// RedactText removes exact sensitive values and conservative KEY=value secret
// assignments from text before it is returned, logged, or persisted.
func RedactText(text string, sensitiveValues ...string) string {
	values := nonemptySensitiveValues(sensitiveValues)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, redactedValue)
	}
	return sensitiveAssignmentPattern.ReplaceAllString(text, `${1}`+redactedValue)
}

func HasSensitiveValues(values []string) bool {
	return len(nonemptySensitiveValues(values)) > 0
}

func RedactExecResult(result ExecResult, sensitiveValues ...string) ExecResult {
	result.Stdout = RedactText(result.Stdout, sensitiveValues...)
	result.Stderr = RedactText(result.Stderr, sensitiveValues...)
	return result
}

func RedactError(err error, sensitiveValues ...string) error {
	if err == nil {
		return nil
	}
	redacted := RedactText(err.Error(), sensitiveValues...)
	if redacted == err.Error() {
		return err
	}
	return redactedError{message: redacted, cause: err}
}

type redactedError struct {
	message string
	cause   error
}

func (err redactedError) Error() string { return err.message }

func (err redactedError) Unwrap() error { return err.cause }

// BufferSensitiveSinks fully buffers executions carrying exact sensitive
// values. Ordinary executions retain line-level streaming while conservative
// KEY=value secret assignments are redacted before reaching either sink.
func BufferSensitiveSinks(sensitiveValues []string, stdout, stderr io.Writer) (io.Writer, io.Writer, func() error) {
	if !HasSensitiveValues(sensitiveValues) {
		stdoutRedactor := newLineRedactingWriter(stdout)
		stderrRedactor := newLineRedactingWriter(stderr)
		return stdoutRedactor, stderrRedactor, func() error {
			return errors.Join(stdoutRedactor.Flush(), stderrRedactor.Flush())
		}
	}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	flush := func() error {
		return errors.Join(
			writeRedacted(&stdoutBuffer, stdout, sensitiveValues),
			writeRedacted(&stderrBuffer, stderr, sensitiveValues),
		)
	}
	return &stdoutBuffer, &stderrBuffer, flush
}

type lineRedactingWriter struct {
	destination io.Writer
	buffer      bytes.Buffer
}

func newLineRedactingWriter(destination io.Writer) *lineRedactingWriter {
	return &lineRedactingWriter{destination: destination}
}

func (writer *lineRedactingWriter) Write(data []byte) (int, error) {
	written := len(data)
	if writer.destination == nil {
		return written, nil
	}
	if _, err := writer.buffer.Write(data); err != nil {
		return 0, err
	}
	for {
		delimiter := bytes.IndexAny(writer.buffer.Bytes(), "\r\n")
		if delimiter < 0 {
			break
		}
		line := append([]byte(nil), writer.buffer.Next(delimiter+1)...)
		if err := writer.writeRedacted(string(line)); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (writer *lineRedactingWriter) Flush() error {
	if writer.buffer.Len() == 0 {
		return nil
	}
	remaining := writer.buffer.String()
	writer.buffer.Reset()
	return writer.writeRedacted(remaining)
}

func (writer *lineRedactingWriter) writeRedacted(text string) error {
	if writer.destination == nil {
		return nil
	}
	redacted := RedactText(text)
	written, err := io.WriteString(writer.destination, redacted)
	if err == nil && written != len(redacted) {
		return io.ErrShortWrite
	}
	return err
}

func FinishSensitiveExecution(result ExecResult, runErr, sinkErr error, sensitiveValues []string) (ExecResult, error) {
	result = RedactExecResult(result, sensitiveValues...)
	return result, errors.Join(RedactError(runErr, sensitiveValues...), sinkErr)
}

func writeRedacted(source *bytes.Buffer, destination io.Writer, sensitiveValues []string) error {
	if destination == nil || source.Len() == 0 {
		return nil
	}
	redacted := RedactText(source.String(), sensitiveValues...)
	written, err := io.WriteString(destination, redacted)
	if err == nil && written != len(redacted) {
		return io.ErrShortWrite
	}
	return err
}

func nonemptySensitiveValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
