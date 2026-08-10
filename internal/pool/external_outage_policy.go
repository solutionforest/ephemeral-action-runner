package pool

import (
	"fmt"

	"github.com/solutionforest/ephemeral-action-runner/internal/dependency"
)

type ExternalOutageRetryPolicy = dependency.Policy

func ParseExternalOutageRetryPolicy(value string) (ExternalOutageRetryPolicy, error) {
	policy, err := dependency.ParsePolicy(value)
	if err != nil {
		return ExternalOutageRetryPolicy{}, fmt.Errorf("invalid --external-outage-retry: %w", err)
	}
	return policy, nil
}
