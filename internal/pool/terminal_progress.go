package pool

import (
	"fmt"
	"io"
)

const terminalProgressEllipsis = "..."

func writeInteractiveProgressLine(writer io.Writer, line string) {
	_, _ = fmt.Fprintf(writer, "\r\033[2K%s", boundedTerminalProgressLine(line, progressTerminalWidth()))
}

// boundedTerminalProgressLine reserves the last terminal cell so writing a
// progress update cannot trigger an automatic wrap. Current progress summaries
// are ASCII; rune-safe slicing also avoids producing invalid UTF-8.
func boundedTerminalProgressLine(line string, terminalWidth int) string {
	if terminalWidth <= 0 {
		return line
	}
	maxRunes := terminalWidth - 1
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	if maxRunes <= len(terminalProgressEllipsis) {
		return string(runes[:maxRunes])
	}
	available := maxRunes - len(terminalProgressEllipsis)
	prefixRunes := (available + 1) / 2
	suffixRunes := available - prefixRunes
	return string(runes[:prefixRunes]) + terminalProgressEllipsis + string(runes[len(runes)-suffixRunes:])
}
