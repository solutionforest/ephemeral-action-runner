package logging

import (
	"bytes"
	"io"
	"math"
	"runtime"
	"sync"

	"golang.org/x/term"
)

// ConsoleWriter preserves native Windows line endings when output is written
// directly to a terminal. Redirected output and non-Windows platforms are left
// unchanged.
func ConsoleWriter(writer io.Writer) io.Writer {
	if runtime.GOOS != "windows" {
		return writer
	}
	file, ok := writer.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return writer
	}
	return &windowsConsoleWriter{writer: writer}
}

type windowsConsoleWriter struct {
	writer     io.Writer
	mu         sync.Mutex
	previousCR bool
}

func (writer *windowsConsoleWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()

	normalized := windowsConsoleLineEndingsAfter(data, writer.previousCR)
	written, err := writer.writer.Write(normalized)
	if written == len(normalized) && len(data) > 0 {
		writer.previousCR = data[len(data)-1] == '\r'
	}
	if err != nil {
		if written == len(normalized) {
			return len(data), err
		}
		return 0, err
	}
	if written != len(normalized) {
		return 0, io.ErrShortWrite
	}
	return len(data), nil
}

func windowsConsoleLineEndings(data []byte) []byte {
	return windowsConsoleLineEndingsAfter(data, false)
}

func windowsConsoleLineEndingsAfter(data []byte, previousCR bool) []byte {
	if !bytes.Contains(data, []byte{'\n'}) {
		return data
	}
	insertionCount := windowsConsoleLineEndingInsertions(data, previousCR)
	capacity, ok := windowsConsoleLineEndingCapacity(len(data), insertionCount)
	if !ok {
		// The normalized slice cannot be represented by an int. Preserve the
		// original output instead of overflowing the allocation size.
		return data
	}
	normalized := make([]byte, 0, capacity)
	for index, value := range data {
		precededByCR := index > 0 && data[index-1] == '\r' || index == 0 && previousCR
		if value == '\n' && !precededByCR {
			normalized = append(normalized, '\r')
		}
		normalized = append(normalized, value)
	}
	return normalized
}

func windowsConsoleLineEndingCapacity(dataLength, insertionCount int) (int, bool) {
	if insertionCount > math.MaxInt-dataLength {
		return dataLength, false
	}
	return dataLength + insertionCount, true
}

func windowsConsoleLineEndingInsertions(data []byte, previousCR bool) int {
	insertionCount := 0
	for index, value := range data {
		precededByCR := index > 0 && data[index-1] == '\r' || index == 0 && previousCR
		if value == '\n' && !precededByCR {
			insertionCount++
		}
	}
	return insertionCount
}
