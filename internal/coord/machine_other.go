//go:build !darwin && !linux

package coord

// machineID returns empty string on unsupported platforms.
// Machine() falls back to MachineLocal in this case.
func machineID() string { return "" }
