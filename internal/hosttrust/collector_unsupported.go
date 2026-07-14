//go:build !windows && !darwin && !linux

package hosttrust

import "context"

func collectNative(_ context.Context, _ []string) (Snapshot, error) {
	return Snapshot{}, ErrUnsupportedPlatform
}
