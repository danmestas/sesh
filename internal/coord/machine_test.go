package coord_test

import (
	"testing"

	"github.com/danmestas/sesh/internal/coord"
)

func TestMachineLocal_Value(t *testing.T) {
	if coord.MachineLocal != "_local" {
		t.Errorf("MachineLocal = %q; want \"_local\"", coord.MachineLocal)
	}
}

func TestMachine_EnvOverride(t *testing.T) {
	t.Setenv("SESH_MACHINE", "my-test-host")
	if got := coord.Machine(); got != "my-test-host" {
		t.Errorf("Machine() = %q; want %q", got, "my-test-host")
	}
}

func TestMachine_FallbackNonEmpty(t *testing.T) {
	// Setting to "" makes os.Getenv return "" — override is inactive.
	t.Setenv("SESH_MACHINE", "")
	got := coord.Machine()
	if got == "" {
		t.Error("Machine() returned empty string without SESH_MACHINE override")
	}
}
