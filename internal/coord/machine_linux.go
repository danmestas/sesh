//go:build linux

package coord

import (
	"os"
	"strings"
)

// machineID returns the first 8 lowercase hex chars of /etc/machine-id.
// Returns empty string if the file is missing or too short.
func machineID() string {
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(b))
	if len(raw) < 8 {
		return ""
	}
	return strings.ToLower(raw[:8])
}
