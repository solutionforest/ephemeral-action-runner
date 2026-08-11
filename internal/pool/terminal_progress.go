package pool

import (
	"io"

	"github.com/solutionforest/ephemeral-action-runner/internal/terminalprogress"
)

func newInteractiveProgressRenderer(writer io.Writer) *terminalprogress.Renderer {
	renderer := terminalprogress.New(writer, progressTerminalWidth)
	if !renderer.Available() {
		return nil
	}
	return renderer
}
