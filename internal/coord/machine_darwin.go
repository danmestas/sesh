//go:build darwin

package coord

import (
	"os/exec"
	"regexp"
	"strings"
)

// uuidRE matches the IOPlatformUUID line in ioreg output.
// Example line: "IOPlatformUUID" = "AABBCCDD-1234-5678-ABCD-000000000001"
var uuidRE = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([0-9A-Fa-f-]+)"`)

// machineID returns the first 8 lowercase hex chars of the IOPlatformUUID.
// Returns empty string if ioreg is unavailable or the UUID cannot be parsed.
func machineID() string {
	out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	m := uuidRE.FindSubmatch(out)
	if m == nil {
		return ""
	}
	// UUID looks like "AABBCCDD-1234-..."; strip dashes and take first 8 hex.
	raw := strings.ReplaceAll(string(m[1]), "-", "")
	if len(raw) < 8 {
		return ""
	}
	return strings.ToLower(raw[:8])
}
