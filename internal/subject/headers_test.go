package subject

import "testing"

// TestHeader_LiteralsArePinned guards against silent drift in the
// cross-stack header contract. Every literal here MUST match the value
// of the same-named export in sesh-channels/sdk/src/headers.ts
// byte-for-byte. Bumping any of these is a paired Go + TS PR.
func TestHeader_LiteralsArePinned(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"HeaderProtocolVersion", HeaderProtocolVersion, "Sesh-Protocol-Version"},
		{"HeaderCapabilities", HeaderCapabilities, "Sesh-Capabilities"},
		{"HeaderWire", HeaderWire, "Sesh-Wire"},
		{"CurrentProtocolVersion", CurrentProtocolVersion, "0.4"},
		{"DefaultCapabilities", DefaultCapabilities, "messages,artifacts,cards"},
		{"WireSynadia", WireSynadia, "synadia"},
		{"WireA2A", WireA2A, "a2a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %q, want %q (cross-stack drift — keep paired with sesh-channels/sdk/src/headers.ts)",
					tc.name, tc.got, tc.want)
			}
		})
	}
}
