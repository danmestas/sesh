// Package coord provides stable host identity and subject-construction
// primitives for the sesh coordination subject hierarchy (sesh.*).
//
// The machine identifier is a single resolved token used in the second
// position of every sesh.* subject:
//
//	sesh.<verb>.<machine>.<scope>.<scope-id>.<target>.<role>
//
// Resolution order for Machine():
//  1. $SESH_MACHINE environment variable — explicit operator override.
//  2. OS-specific stable host identity (IOPlatformUUID on darwin,
//     /etc/machine-id on linux).
//  3. MachineLocal — safe sentinel for single-host deployments.
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
