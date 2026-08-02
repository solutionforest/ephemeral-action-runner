// Package inventory collects a deterministic, read-only inventory of known
// EPAR filesystem storage roots.
//
// Collection never invokes provider CLIs and never removes or rewrites host
// resources. Exact ownership is claimed only for artifacts that pass their
// complete recognition rules; ambiguous entries remain unknown and report-only.
package inventory
