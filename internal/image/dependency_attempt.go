package image

import (
	"context"
	"time"
)

const (
	imageMetadataAttemptTimeout = 30 * time.Second
	verifiedAssetAttemptTimeout = 30 * time.Minute
	runnerImagesAttemptTimeout  = 15 * time.Minute
	dockerPullAttemptTimeout    = 2 * time.Hour
)

func boundedImageAttempt(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}
