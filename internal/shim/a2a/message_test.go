package a2a

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/sesh-ops/messages"
)

func TestFromWireMessage_DecodesAndTranslatesRole(t *testing.T) {
	in := []byte(`{
		"messageId":"M1",
		"taskId":"T1",
		"contextId":"C1",
		"role":"ROLE_USER",
		"parts":[{"text":"hello"}]
	}`)
	got, err := FromWireMessage(in)
	if err != nil {
		t.Fatalf("FromWireMessage: %v", err)
	}
	if got.ID != "M1" || got.TaskID != "T1" || got.ContextID != "C1" {
		t.Errorf("ids mismatch: %+v", got)
	}
	if got.Role != messages.MessageRoleUser {
		t.Errorf("role = %q, want %q", got.Role, messages.MessageRoleUser)
	}
	if len(got.Parts) != 1 || got.Parts[0].Text != "hello" {
		t.Errorf("parts: %+v", got.Parts)
	}
}

func TestFromWireMessage_AgentRole(t *testing.T) {
	in := []byte(`{"messageId":"M","taskId":"T","role":"ROLE_AGENT","parts":[{"text":"x"}]}`)
	got, err := FromWireMessage(in)
	if err != nil {
		t.Fatalf("FromWireMessage: %v", err)
	}
	if got.Role != messages.MessageRoleAgent {
		t.Errorf("role = %q, want %q", got.Role, messages.MessageRoleAgent)
	}
}

func TestFromWireMessage_UnknownRoleClearsToEmpty(t *testing.T) {
	in := []byte(`{"messageId":"M","taskId":"T","role":"ROLE_BOGUS","parts":[{"text":"x"}]}`)
	got, err := FromWireMessage(in)
	if err != nil {
		t.Fatalf("FromWireMessage: %v", err)
	}
	if got.Role != "" {
		t.Errorf("role = %q, want empty", got.Role)
	}
}

func TestFromWireMessage_Malformed(t *testing.T) {
	if _, err := FromWireMessage(nil); err == nil {
		t.Error("nil input: want error")
	}
	if _, err := FromWireMessage([]byte("{not json")); err == nil {
		t.Error("bad json: want error")
	}
}

func TestToWireMessage_RoleAndShape(t *testing.T) {
	m := &messages.Message{
		ID:        "M1",
		TaskID:    "T1",
		ContextID: "C1",
		Role:      messages.MessageRoleAgent,
		Parts:     []messages.Part{{Text: "hi"}},
	}
	raw, err := ToWireMessage(m)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	var out struct {
		ID        string `json:"messageId"`
		TaskID    string `json:"taskId"`
		ContextID string `json:"contextId"`
		Role      string `json:"role"`
		Parts     []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Role != "ROLE_AGENT" {
		t.Errorf("role = %q, want ROLE_AGENT", out.Role)
	}
	if out.ID != "M1" || out.TaskID != "T1" || out.ContextID != "C1" {
		t.Errorf("ids: %+v", out)
	}
	if len(out.Parts) != 1 || out.Parts[0].Text != "hi" {
		t.Errorf("parts: %+v", out.Parts)
	}
}

func TestToWireMessage_NilErrors(t *testing.T) {
	if _, err := ToWireMessage(nil); err == nil {
		t.Error("nil: want error")
	}
}

func TestRoundTrip_WireToStorageToWire(t *testing.T) {
	in := []byte(`{"messageId":"M1","taskId":"T1","role":"ROLE_USER","parts":[{"text":"hi"}],"v":0,"createdAt":"0001-01-01T00:00:00Z"}`)
	stored, err := FromWireMessage(in)
	if err != nil {
		t.Fatalf("FromWireMessage: %v", err)
	}
	out, err := ToWireMessage(stored)
	if err != nil {
		t.Fatalf("ToWireMessage: %v", err)
	}
	if !strings.Contains(string(out), `"role":"ROLE_USER"`) {
		t.Errorf("round-trip lost role: %s", out)
	}
}

// TestToMeshMessage_RoleIsLowercase guards sesh#137: the v0.4 mesh
// wire projection must emit lowercase "user"/"agent" (not the a2a-go
// SCREAMING_SNAKE "ROLE_USER"/"ROLE_AGENT") so the sesh-channels
// adapter SDK envelope validator accepts the published payload.
func TestToMeshMessage_RoleIsLowercase(t *testing.T) {
	cases := []struct {
		role messages.MessageRole
		want string
	}{
		{messages.MessageRoleUser, "user"},
		{messages.MessageRoleAgent, "agent"},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			m := &messages.Message{
				ID:    "M1",
				Role:  tc.role,
				Parts: []messages.Part{{Text: "x"}},
			}
			raw, err := ToMeshMessage(m)
			if err != nil {
				t.Fatalf("ToMeshMessage: %v", err)
			}
			if !strings.Contains(string(raw), `"role":"`+tc.want+`"`) {
				t.Errorf("payload missing lowercase role %q: %s", tc.want, raw)
			}
			if strings.Contains(string(raw), "ROLE_") {
				t.Errorf("payload still contains SCREAMING_SNAKE form: %s", raw)
			}
		})
	}
}

func TestToMeshMessage_NilErrors(t *testing.T) {
	if _, err := ToMeshMessage(nil); err == nil {
		t.Error("nil: want error")
	}
}

func TestFromWireMessage_DoesNotMutateRoleOnUserAndAgentOnly(t *testing.T) {
	for wire, want := range map[string]messages.MessageRole{
		"ROLE_USER":  messages.MessageRoleUser,
		"ROLE_AGENT": messages.MessageRoleAgent,
	} {
		in := []byte(`{"messageId":"M","taskId":"T","role":"` + wire + `","parts":[{"text":"x"}]}`)
		got, err := FromWireMessage(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Role != want {
			t.Errorf("wire %q -> stored %q, want %q", wire, got.Role, want)
		}
	}
}
