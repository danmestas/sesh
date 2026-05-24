package jsonrpc

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode_Valid(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"abc"}}`)
	req, jerr := Decode(body)
	if jerr != nil {
		t.Fatalf("Decode returned error: %+v", jerr)
	}
	if req.Method != "GetTask" {
		t.Fatalf("method = %q, want GetTask", req.Method)
	}
	if string(req.ID) != "1" {
		t.Fatalf("id raw = %q, want 1", req.ID)
	}
	var p struct{ ID string }
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if p.ID != "abc" {
		t.Fatalf("params.id = %q, want abc", p.ID)
	}
}

func TestDecode_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *Error
	}{
		{"malformed json", `not json`, ErrParse},
		{"empty body", ``, ErrParse},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"x"}`, ErrInvalidReq},
		{"missing method", `{"jsonrpc":"2.0","id":1}`, ErrInvalidReq},
		{"empty method", `{"jsonrpc":"2.0","id":1,"method":""}`, ErrInvalidReq},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, jerr := Decode([]byte(tc.body))
			if jerr == nil {
				t.Fatalf("expected error, got nil")
			}
			if jerr.Code != tc.want.Code || jerr.Name != tc.want.Name {
				t.Fatalf("got code=%d name=%q, want code=%d name=%q",
					jerr.Code, jerr.Name, tc.want.Code, tc.want.Name)
			}
		})
	}
}

func TestWriteResponse_SuccessShape(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, json.RawMessage(`42`), map[string]string{"ok": "yes"})
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", got["jsonrpc"])
	}
	if v, ok := got["id"].(float64); !ok || v != 42 {
		t.Errorf("id = %v (%T)", got["id"], got["id"])
	}
	if got["error"] != nil {
		t.Errorf("error must be omitted on success, got %v", got["error"])
	}
}

func TestWriteError_ErrorShape(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, json.RawMessage(`"abc"`), ErrTaskNotFound)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var got Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got.Error == nil {
		t.Fatal("error field missing")
	}
	if got.Error.Code != -32001 {
		t.Errorf("code = %d, want -32001", got.Error.Code)
	}
	if got.Error.Name != "TaskNotFoundError" {
		t.Errorf("name = %q, want TaskNotFoundError", got.Error.Name)
	}
	if string(got.ID) != `"abc"` {
		t.Errorf("id = %s, want \"abc\"", got.ID)
	}
}

func TestWriteError_NilIDBecomesNull(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, nil, ErrParse)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"id":null`)) {
		t.Errorf("expected id:null in body, got %s", w.Body.String())
	}
}

func TestError_WithData(t *testing.T) {
	e := ErrInternal.WithData(map[string]any{"kind": "task-leased", "retryable": true})
	if e == ErrInternal {
		t.Fatal("WithData should return a copy")
	}
	if e.Code != ErrInternal.Code || e.Name != ErrInternal.Name {
		t.Errorf("WithData mutated base fields: %+v", e)
	}
	if !strings.Contains(string(e.Data), "task-leased") {
		t.Errorf("data missing payload: %s", e.Data)
	}
}

func TestError_WithDataNil(t *testing.T) {
	e := ErrInternal.WithData(nil)
	if e != ErrInternal {
		t.Errorf("WithData(nil) should return the same pointer")
	}
}
