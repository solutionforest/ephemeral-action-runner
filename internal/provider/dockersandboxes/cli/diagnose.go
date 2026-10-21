// Package cli contains command compatibility shared by provider admission and
// platform promotion preflight.
package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
)

// Diagnose reads the same versioned JSON report on either side of the v0.48
// flag rename. Only an explicit unsupported flag permits the legacy spelling;
// operational failures, cancellation, and invalid reports remain failures.
func Diagnose(ctx context.Context, run func(context.Context, []string) ([]byte, error)) ([]byte, error) {
	output, err := run(ctx, []string{"diagnose", "--format", "json"})
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || len(bytes.TrimSpace(output)) != 0 {
		return output, err
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "unknown flag: --format") {
			return run(ctx, []string{"diagnose", "--output", "json"})
		}
	}
	return output, err
}
