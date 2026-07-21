package logging

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

type fullWriteErrorOnceWriter struct {
	bytes.Buffer
	err error
}

func (writer *fullWriteErrorOnceWriter) Write(data []byte) (int, error) {
	written, _ := writer.Buffer.Write(data)
	err := writer.err
	writer.err = nil
	return written, err
}

func TestWindowsConsoleLineEndings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lone LF", in: "one\ntwo\n", want: "one\r\ntwo\r\n"},
		{name: "existing CRLF", in: "one\r\ntwo\r\n", want: "one\r\ntwo\r\n"},
		{name: "mixed", in: "one\r\ntwo\nthree", want: "one\r\ntwo\r\nthree"},
		{name: "no newline", in: "one", want: "one"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(windowsConsoleLineEndings([]byte(tt.in))); got != tt.want {
				t.Fatalf("windowsConsoleLineEndings(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWindowsConsoleLineEndingCapacity(t *testing.T) {
	tests := []struct {
		name           string
		dataLength     int
		insertionCount int
		want           int
		wantOK         bool
	}{
		{name: "exact capacity", dataLength: 8, insertionCount: 2, want: 10, wantOK: true},
		{name: "maximum capacity", dataLength: math.MaxInt - 1, insertionCount: 1, want: math.MaxInt, wantOK: true},
		{name: "overflow", dataLength: math.MaxInt, insertionCount: 1, want: math.MaxInt, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := windowsConsoleLineEndingCapacity(tt.dataLength, tt.insertionCount)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("windowsConsoleLineEndingCapacity(%d, %d) = (%d, %t), want (%d, %t)", tt.dataLength, tt.insertionCount, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWindowsConsoleLineEndingInsertions(t *testing.T) {
	tests := []struct {
		name       string
		data       string
		previousCR bool
		want       int
	}{
		{name: "lone LF", data: "one\ntwo\n", want: 2},
		{name: "existing CRLF", data: "one\r\ntwo\r\n", want: 0},
		{name: "mixed", data: "one\r\ntwo\nthree", want: 1},
		{name: "split CRLF", data: "\ntwo\n", previousCR: true, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowsConsoleLineEndingInsertions([]byte(tt.data), tt.previousCR); got != tt.want {
				t.Fatalf("windowsConsoleLineEndingInsertions(%q, %t) = %d, want %d", tt.data, tt.previousCR, got, tt.want)
			}
		})
	}
}

func TestWindowsConsoleWriterReportsOriginalByteCount(t *testing.T) {
	var output bytes.Buffer
	writer := &windowsConsoleWriter{writer: &output}
	input := []byte("one\ntwo\n")

	written, err := writer.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(input) {
		t.Fatalf("Write() bytes = %d, want %d", written, len(input))
	}
	if got, want := output.String(), "one\r\ntwo\r\n"; got != want {
		t.Fatalf("Write() output = %q, want %q", got, want)
	}
}

func TestWindowsConsoleWriterPreservesSplitCRLF(t *testing.T) {
	var output bytes.Buffer
	writer := &windowsConsoleWriter{writer: &output}

	for _, input := range []string{"one\r", "\ntwo\n"} {
		if _, err := writer.Write([]byte(input)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := output.String(), "one\r\ntwo\r\n"; got != want {
		t.Fatalf("split Write() output = %q, want %q", got, want)
	}
}

func TestWindowsConsoleWriterReportsFullWriteWithError(t *testing.T) {
	sentinel := errors.New("injected write error")
	output := &fullWriteErrorOnceWriter{err: sentinel}
	writer := &windowsConsoleWriter{writer: output}

	first := []byte("one\r")
	written, err := writer.Write(first)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Write() error = %v, want %v", err, sentinel)
	}
	if written != len(first) {
		t.Fatalf("Write() bytes = %d, want %d", written, len(first))
	}
	if _, err := writer.Write([]byte("\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "one\r\ntwo\r\n"; got != want {
		t.Fatalf("Write() output = %q, want %q", got, want)
	}
}

func TestConsoleWriterLeavesNonFileWriterUnchanged(t *testing.T) {
	var output bytes.Buffer
	if got := ConsoleWriter(&output); got != &output {
		t.Fatalf("ConsoleWriter() wrapped a non-terminal writer: %T", got)
	}
}
