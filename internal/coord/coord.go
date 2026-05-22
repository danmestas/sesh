// Package coord provides stable host identity for sesh coordination
// subjects layered on top of the Synadia `agents.*` namespace.
//
// The machine identifier from Machine() occupies the third position
// (after `agents.<verb>`) of every coordination subject:
//
//	agents.<verb>.<machine>.<project>.<session>[.<role>[.<worker_id>]]
//
// Token count selects the addressing tier: 5 = session orch, 6 = role
// pool (queue group on role), 7 = direct address by instance_id. See
// docs/synadia-agents-on-sesh.md § 8.1 for the full contract.
//
// Resolution order for Machine():
//  1. $SESH_MACHINE environment variable — explicit operator override.
//  2. OS-specific stable host identity (IOPlatformUUID on darwin,
//     /etc/machine-id on linux).
//  3. MachineLocal — safe sentinel for single-host deployments.
//
// project-id derivation + pinning live in cli/paths.go (a separate
// concern; this package owns only the host-identity resolver).
package coord

import "os"

// MachineLocal is the sentinel machine identifier for single-host deployments.
// Subscriptions using _local only match traffic originated on this host.
const MachineLocal = "_local"

// Machine returns the stable host identity for use in coordination subjects.
// See package doc for resolution order.
func Machine() string {
	if v := os.Getenv("SESH_MACHINE"); v != "" {
		return v
	}
	if id := machineID(); id != "" {
		return id
	}
	return MachineLocal
}
