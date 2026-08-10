// Package state provides a provider-neutral, durable ownership ledger for pool
// instances. It records intents before side effects and only permits exact-name
// records to progress through the lifecycle. Provider-specific details belong
// in Receipt, an explicitly versioned opaque JSON object.
//
// Unknown provider inventory is stored as a Discovery. Discoveries are
// quarantine/report-only: they cannot be converted into an owned Record or
// deleted through this package.
//
// The primary transition table is:
//
//	reserved -> creating -> created -> validating -> standby -> registering
//	         -> ready -> busy -> draining -> fencing -> fenced
//	         -> remote-reconciling -> remote-absent -> local-removing
//	         -> local-absent -> tombstoned
//
// Quarantine is terminal for normal allocation and registration. It can only
// proceed through fencing and exact cleanup. Cleanup failures become
// cleanup-pending and resume only at fencing.
package state
