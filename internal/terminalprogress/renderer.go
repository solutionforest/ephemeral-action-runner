package terminalprogress

import (
	"io"
	"strings"
	"sync"
	"unicode"
)

const (
	controlPrefix = "\r\x1b[2K"
	ellipsis      = "..."
)

// Renderer redraws a single progress row without using the terminal's final
// column. Avoiding that column prevents Windows consoles from soft-wrapping a
// progress update before the next carriage return can replace it.
type Renderer struct {
	writer    io.Writer
	width     func() int
	mu        sync.Mutex
	lastWidth int
	rendered  bool
}

// New returns a renderer whose width source is sampled on every update. The
// last valid width is retained across transient terminal-size query failures.
func New(writer io.Writer, width func() int) *Renderer {
	return &Renderer{writer: writer, width: width}
}

// Available reports whether a usable terminal width has been observed. A
// caller should use ordinary newline logging when this returns false.
func (renderer *Renderer) Available() bool {
	if renderer == nil {
		return false
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	return renderer.refreshWidthLocked() > 1 && renderer.writer != nil
}

// Write selects the richest candidate that fits on one physical row, clears
// the current row, and writes the update as one operation. Candidates should
// be ordered from richest to most compact.
func (renderer *Renderer) Write(candidates ...string) bool {
	if renderer == nil {
		return false
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	width := renderer.refreshWidthLocked()
	if width <= 1 || renderer.writer == nil {
		return false
	}
	_, _ = io.WriteString(renderer.writer, controlPrefix+Fit(candidates, width))
	renderer.rendered = true
	return true
}

// Finish clears a rendered progress row and advances to a clean line. It is a
// no-op when this renderer never wrote an interactive frame.
func (renderer *Renderer) Finish() bool {
	if renderer == nil {
		return false
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.rendered || renderer.writer == nil {
		return false
	}
	_, _ = io.WriteString(renderer.writer, controlPrefix+"\n")
	renderer.rendered = false
	return true
}

func (renderer *Renderer) refreshWidthLocked() int {
	if renderer.width != nil {
		if width := renderer.width(); width > 0 {
			renderer.lastWidth = width
		}
	}
	return renderer.lastWidth
}

// Fit returns a single-line candidate bounded to terminalWidth-1 display
// columns. EPAR progress text is ASCII; non-ASCII runes are conservatively
// counted as two columns so future diagnostic text cannot underestimate its
// terminal footprint.
func Fit(candidates []string, terminalWidth int) string {
	if len(candidates) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		cleaned = append(cleaned, singleLine(candidate))
	}
	if terminalWidth <= 0 {
		return cleaned[0]
	}
	maxColumns := terminalWidth - 1
	if maxColumns <= 0 {
		return ""
	}
	for _, candidate := range cleaned {
		if displayColumns(candidate) <= maxColumns {
			return candidate
		}
	}
	return middleElide(cleaned[len(cleaned)-1], maxColumns)
}

func singleLine(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func displayColumns(value string) int {
	columns := 0
	for _, r := range value {
		if r <= unicode.MaxASCII {
			columns++
		} else {
			columns += 2
		}
	}
	return columns
}

func middleElide(value string, maxColumns int) string {
	if maxColumns <= 0 {
		return ""
	}
	if displayColumns(value) <= maxColumns {
		return value
	}
	if maxColumns <= len(ellipsis) {
		return takePrefix(value, maxColumns)
	}
	available := maxColumns - len(ellipsis)
	prefixColumns := (available + 1) / 2
	suffixColumns := available - prefixColumns
	return strings.TrimSpace(takePrefix(value, prefixColumns)) + ellipsis + strings.TrimSpace(takeSuffix(value, suffixColumns))
}

func takePrefix(value string, maxColumns int) string {
	var builder strings.Builder
	columns := 0
	for _, r := range value {
		width := runeColumns(r)
		if columns+width > maxColumns {
			break
		}
		builder.WriteRune(r)
		columns += width
	}
	return builder.String()
}

func takeSuffix(value string, maxColumns int) string {
	runes := []rune(value)
	columns := 0
	start := len(runes)
	for start > 0 {
		width := runeColumns(runes[start-1])
		if columns+width > maxColumns {
			break
		}
		start--
		columns += width
	}
	return string(runes[start:])
}

func runeColumns(r rune) int {
	if r <= unicode.MaxASCII {
		return 1
	}
	return 2
}
