// Package storage defines provider-neutral, fail-closed storage inventory,
// capacity, retention-planning, and exact-execution contracts.
//
// The package does not delete host resources and does not implement Docker,
// Docker Sandboxes, or filesystem cleanup. Adapters inventory exact artifacts,
// Preview deterministically classifies them, and an optional ExactExecutor
// integration can apply only the exact removal entries bound into an approved
// plan hash.
package storage
