// Package jsonrpc implements the JSON-RPC 2.0 envelope used by the
// A2A POST /a2a endpoint. Pre-defined Error values include both the
// standard JSON-RPC codes and the A2A-named errors used in Slice 1.
package jsonrpc

import (
	"encoding/json"
	"net/http"
)

// Request is the inbound JSON-RPC 2.0 envelope.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is the outbound envelope. Exactly one of Result or Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error follows the JSON-RPC 2.0 error object. Name carries the
// A2A-spec error name; stock A2A clients match on Name (not Code).
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Name    string          `json:"name,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// WithData returns a copy of e with Data set to the JSON encoding of v.
// If v fails to marshal, the original e is returned unmodified.
func (e *Error) WithData(v any) *Error {
	if v == nil {
		return e
	}
	b, err := json.Marshal(v)
	if err != nil {
		return e
	}
	cp := *e
	cp.Data = b
	return &cp
}

// Standard JSON-RPC codes plus the A2A-named errors needed for Slice 1.
var (
	ErrParse                     = &Error{Code: -32700, Message: "Parse error", Name: "Parse error"}
	ErrInvalidReq                = &Error{Code: -32600, Message: "Invalid Request", Name: "Invalid Request"}
	ErrMethodNotFound            = &Error{Code: -32601, Message: "Method not found", Name: "Method not found"}
	ErrInvalidParams             = &Error{Code: -32602, Message: "Invalid params", Name: "Invalid params"}
	ErrInternal                  = &Error{Code: -32603, Message: "Internal error", Name: "Internal error"}
	ErrTaskNotFound              = &Error{Code: -32001, Message: "task not found", Name: "TaskNotFoundError"}
	ErrTaskNotCancelable         = &Error{Code: -32002, Message: "task cannot be canceled", Name: "TaskNotCancelableError"}
	ErrExtendedCardNotConfigured = &Error{Code: -32007, Message: "extended agent card not configured", Name: "AuthenticatedExtendedCardNotConfiguredError"}
	ErrPushNotConfigured         = &Error{Code: -32008, Message: "push notifications not configured", Name: "PushNotificationNotSupportedError"}
)

// Decode parses a JSON-RPC 2.0 request body. Returns ErrParse on JSON
// syntax errors and ErrInvalidReq when jsonrpc != "2.0" or method is empty.
func Decode(body []byte) (*Request, *Error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, ErrParse
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return nil, ErrInvalidReq
	}
	return &req, nil
}

// WriteResponse writes a success JSON-RPC response. Always HTTP 200 —
// JSON-RPC carries its own success/failure signal.
func WriteResponse(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := Response{JSONRPC: "2.0", ID: idOrNull(id), Result: result}
	writeJSON(w, &resp)
}

// WriteError writes an error JSON-RPC response. Always HTTP 200 for
// application-level errors. Transport-level failures (auth missing,
// body too large) bypass this and use http.Error directly.
func WriteError(w http.ResponseWriter, id json.RawMessage, e *Error) {
	resp := Response{JSONRPC: "2.0", ID: idOrNull(id), Error: e}
	writeJSON(w, &resp)
}

func writeJSON(w http.ResponseWriter, resp *Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
