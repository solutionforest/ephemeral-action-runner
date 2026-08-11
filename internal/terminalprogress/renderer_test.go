package terminalprogress

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestFitNeverUsesFinalTerminalColumn(t *testing.T) {
	candidates := []string{
		"Docker Sandboxes prebuilt Full archive: downloading/materializing; 18.4 GiB written; 12.5 MiB/s archive-write average; elapsed 24m30s",
		"Prebuilt Full: downloading; 18.4 GiB; 12.5 MiB/s; 24m30s",
		"Full 18.4 GiB 24m30s",
	}
	for width := 1; width <= 240; width++ {
		line := Fit(candidates, width)
		if got := displayColumns(line); got > width-1 {
			t.Fatalf("width %d produced %d columns: %q", width, got, line)
		}
		if strings.ContainsAny(line, "\r\n\t") {
			t.Fatalf("width %d produced a multiline frame: %q", width, line)
		}
	}
}

func TestFitSelectsRichestCandidateThatFits(t *testing.T) {
	candidates := []string{"a detailed progress line", "compact value", "tiny"}
	if got := Fit(candidates, 80); got != candidates[0] {
		t.Fatalf("wide fit = %q", got)
	}
	if got := Fit(candidates, 14); got != candidates[1] {
		t.Fatalf("medium fit = %q", got)
	}
	if got := Fit(candidates, 6); got != candidates[2] {
		t.Fatalf("small fit = %q", got)
	}
}

func TestRendererRetainsLastValidWidthAndFinishesOnce(t *testing.T) {
	var output bytes.Buffer
	widths := []int{20, 0, 12}
	index := 0
	renderer := New(&output, func() int {
		if index >= len(widths) {
			return widths[len(widths)-1]
		}
		width := widths[index]
		index++
		return width
	})
	if !renderer.Available() {
		t.Fatal("renderer did not accept initial terminal width")
	}
	if !renderer.Write("12345678901234567890") {
		t.Fatal("renderer rejected transient width failure after initialization")
	}
	if !renderer.Write("abcdefghijklmnop") {
		t.Fatal("renderer rejected later valid width")
	}
	if !renderer.Finish() || renderer.Finish() {
		t.Fatal("renderer finish lifecycle is not idempotent")
	}
	frames := strings.Split(output.String(), controlPrefix)
	if len(frames) != 4 {
		t.Fatalf("rendered output = %q", output.String())
	}
	if displayColumns(frames[1]) > 19 || displayColumns(frames[2]) > 11 || frames[3] != "\n" {
		t.Fatalf("rendered frames were not bounded: %#v", frames)
	}
}

func TestRendererWithoutInitialWidthDoesNotWriteCursorControls(t *testing.T) {
	var output bytes.Buffer
	renderer := New(&output, func() int { return 0 })
	if renderer.Available() || renderer.Write("progress") || renderer.Finish() {
		t.Fatal("unknown-width renderer became interactive")
	}
	if output.Len() != 0 {
		t.Fatalf("unknown-width renderer wrote %q", output.String())
	}
}

func TestRendererSerializesConcurrentFrames(t *testing.T) {
	var output bytes.Buffer
	renderer := New(&output, func() int { return 80 })
	if !renderer.Available() {
		t.Fatal("renderer unavailable")
	}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			renderer.Write("concurrent progress")
		}()
	}
	group.Wait()
	if got := strings.Count(output.String(), controlPrefix); got != 20 {
		t.Fatalf("frame count = %d, want 20", got)
	}
}
