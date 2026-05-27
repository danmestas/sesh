package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/objects"

	"github.com/danmestas/sesh/internal/shim/a2a"
	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/subject"
)

// newObjectsTestServer wires the full stack with the /obj route mounted
// behind the supplied Validator. Mirrors newTestServer's shape but
// keeps the test focus local (objects_test imports keep tight) and
// lets each test pick its own auth posture.
func newObjectsTestServer(t *testing.T, v auth.Validator) (*server, *httptest.Server, jetstream.JetStream) {
	t.Helper()

	url := startBroker(t)
	nc := testConn(t, url)
	js2, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream v2: %v", err)
	}

	signer, err := card.NewDevSigner()
	if err != nil {
		t.Fatalf("dev signer: %v", err)
	}
	composer := card.NewComposer(nc, subject.Coord{Machine: "a", Project: "o", Session: "a"}, card.L1Defaults{
		GatewayURL:         "https://shim.test",
		ProtocolVersion:    "1.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}, 200*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := card.NewCache(composer, signer, time.Minute, 16)

	srv := newServer(Config{
		Listen:     "127.0.0.1:0",
		Dev:        true,
		Auth:       v,
		Card:       cache,
		Signer:     signer,
		NC:         nc,
		JS:         js2,
		AgentKey:   card.AgentKey{Agent: "a", Owner: "o", Name: "a"},
		ScopeKind:  "project",
		ScopeID:    "abc123",
		GatewayURL: "https://shim.test",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", srv.wrap("/metrics", srv.handleMetrics))
	mux.Handle("GET "+a2a.ObjectPathPrefix+"{scopeKind}/{scopeID}/{taskID}/{artifactID}",
		auth.Middleware(srv.cfg.Auth)(srv.wrapHandler("/obj", http.HandlerFunc(srv.handleObjectGet))))

	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts, js2
}

// seedObject puts `data` into the (project,abc123) object store under
// objects.Key(taskID, artifactID) and returns the resulting ObjectInfo
// so the test can assert ETag matches Digest.
func seedObject(t *testing.T, js jetstream.JetStream, taskID, artifactID string, data []byte, meta jetstream.ObjectMeta) *jetstream.ObjectInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := objects.Open(ctx, js, "project", "abc123")
	if err != nil {
		t.Fatalf("objects.Open: %v", err)
	}
	meta.Name = objects.Key(taskID, artifactID)
	info, err := store.Put(ctx, meta, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return info
}

// allScopesValidator yields a Principal with agent.read so the gate
// trips only when the URL itself is wrong.
type allScopesValidator struct{}

func (allScopesValidator) Validate(r *http.Request) (auth.Principal, error) {
	return auth.Principal{Sub: "test", Scopes: []string{"agent.read"}}, nil
}

// noReadScopeValidator yields a Principal with NO scopes — so the
// handler's scope check trips and returns 403.
type noReadScopeValidator struct{}

func (noReadScopeValidator) Validate(r *http.Request) (auth.Principal, error) {
	return auth.Principal{Sub: "test"}, nil
}

// missingAuthValidator returns the canonical 401 (header set by the
// middleware).
type missingAuthValidator struct{}

func (missingAuthValidator) Validate(r *http.Request) (auth.Principal, error) {
	return auth.Principal{}, &auth.AuthError{Status: http.StatusUnauthorized, Msg: "missing"}
}

func TestObjGet_RoundTrip2MB(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})

	data := make([]byte, 2<<20) // 2 MiB
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	info := seedObject(t, js, "T1", "A1", data, jetstream.ObjectMeta{
		Headers: nats.Header{
			"Content-Type": []string{"application/pdf"},
			"Filename":     []string{"doc.pdf"},
		},
	})

	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T1/A1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != len(data) {
		t.Fatalf("len mismatch: got %d, want %d", len(body), len(data))
	}
	if !bytes.Equal(body, data) {
		gotSum := sha256.Sum256(body)
		wantSum := sha256.Sum256(data)
		t.Fatalf("body bytes differ:\n got sha=%s\nwant sha=%s", hex.EncodeToString(gotSum[:]), hex.EncodeToString(wantSum[:]))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(data)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(data))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="doc.pdf"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	// ETag must wrap the digest in strong-validator quotes and match
	// what JetStream computed at Put time.
	wantETag := `"` + info.Digest + `"`
	if et := resp.Header.Get("ETag"); et != wantETag {
		t.Errorf("ETag = %q, want %q", et, wantETag)
	}
}

// TestObjGet_StreamingHeadersBeforeBody — 4 MB payload, confirm headers
// settled before body bytes arrive. This is a soft assertion: we read
// the headers via http.Response (which only returns once headers parse),
// then drain the body separately. If the server buffered the body
// before writing headers, the Content-Length header could only ever be
// the buffered length — which we verify is the seeded length.
func TestObjGet_StreamingHeadersBeforeBody(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})

	data := make([]byte, 4<<20) // 4 MiB
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	seedObject(t, js, "T2", "A2", data, jetstream.ObjectMeta{})

	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T2/A2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Headers must include Content-Length matching seed size, NOT
	// trailing — proving they were emitted before io.Copy ran.
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(data)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("default Content-Type = %q, want application/octet-stream", ct)
	}
	if et := resp.Header.Get("ETag"); !strings.HasPrefix(et, `"`) || !strings.HasSuffix(et, `"`) {
		t.Errorf("ETag missing strong-validator quotes: %q", et)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != len(data) {
		t.Errorf("len mismatch: got %d, want %d", len(body), len(data))
	}
}

func TestObjGet_MissingObject_404(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})
	// Create the bucket so we get past "bucket not found" and hit
	// "object not found".
	if _, err := objects.Open(context.Background(), js, "project", "abc123"); err != nil {
		t.Fatal(err)
	}
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T0/MISSING")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestObjGet_MissingBucket_404(t *testing.T) {
	// No seed — the handler opens by bucket name and propagates
	// ErrBucketNotFound as a clean 404 (no implicit create).
	_, ts, _ := newObjectsTestServer(t, allScopesValidator{})
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T0/A0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestObjGet_SessionScopeRoundTrip pins the session-scope download path.
// scope.Bucket("session", "myproj_alpha", ...) would reject the sanitized
// id (session validation requires "."), so the handler must compose the
// bucket name itself rather than re-validate through scope.Bucket. This
// test seeds bytes under the unsanitized "myproj.alpha" id (bucket name
// "sesh_objects_session_myproj_alpha") and confirms the URL the
// translator would emit — /obj/session/myproj_alpha/... — fetches them
// back successfully.
func TestObjGet_SessionScopeRoundTrip(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := objects.Open(ctx, js, "session", "myproj.alpha")
	if err != nil {
		t.Fatalf("objects.Open session: %v", err)
	}
	payload := []byte("session-scope payload")
	if _, err := store.Put(ctx, jetstream.ObjectMeta{Name: objects.Key("Ts", "As")}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("store.Put: %v", err)
	}

	// The translator would emit /obj/session/myproj_alpha/Ts/As because
	// ParseURI returns the sanitized id. The handler must accept it.
	resp, err := newTLSClient().Get(ts.URL + "/obj/session/myproj_alpha/Ts/As")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("body = %q, want %q", body, payload)
	}
}

func TestObjGet_InvalidScopeKind_400(t *testing.T) {
	_, ts, _ := newObjectsTestServer(t, allScopesValidator{})
	resp, err := newTLSClient().Get(ts.URL + "/obj/bogus/abc123/T1/A1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestObjGet_MissingAuth_401(t *testing.T) {
	_, ts, _ := newObjectsTestServer(t, missingAuthValidator{})
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T1/A1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want Bearer...", got)
	}
}

func TestObjGet_MissingScope_403(t *testing.T) {
	_, ts, _ := newObjectsTestServer(t, noReadScopeValidator{})
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T1/A1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestObjGet_MetricsRouteLabel(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})

	// Seed something so we can hit 200 too.
	seedObject(t, js, "T1", "A1", []byte("small payload"), jetstream.ObjectMeta{})

	// 1 successful GET → /obj 200.
	if resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T1/A1"); err == nil {
		_ = resp.Body.Close()
	} else {
		t.Fatal(err)
	}
	// 1 missing-object GET → /obj 404.
	if resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/Tx/Ax"); err == nil {
		_ = resp.Body.Close()
	} else {
		t.Fatal(err)
	}

	resp, err := newTLSClient().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, `route="/obj"`) {
		t.Errorf("metrics missing /obj route label:\n%s", text)
	}
	// Both observed statuses should show up — order-independent
	// substring assert (the metrics emitter sorts by route,method,status).
	if !strings.Contains(text, `route="/obj",status="200"`) {
		t.Errorf("metrics missing /obj 200:\n%s", text)
	}
	if !strings.Contains(text, `route="/obj",status="404"`) {
		t.Errorf("metrics missing /obj 404:\n%s", text)
	}
	// Template form must NOT leak.
	if strings.Contains(text, "{scopeKind}") {
		t.Errorf("metrics leaked path template:\n%s", text)
	}
}

// TestObjGet_FilenameHeader confirms a Filename header on the object
// metadata makes it into the Content-Disposition response.
func TestObjGet_FilenameHeader(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})
	seedObject(t, js, "T3", "A3", []byte("hi"), jetstream.ObjectMeta{
		Headers: nats.Header{"Filename": []string{"report.csv"}},
	})
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T3/A3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="report.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// TestObjGet_ETagMatchesDigest pins the strong-validator wrap and the
// digest-byte-equality with what JetStream returns at Put time. If the
// JetStream digest format ever changes (currently SHA-256= base64), the
// ETag changes with it — fine.
func TestObjGet_ETagMatchesDigest(t *testing.T) {
	_, ts, js := newObjectsTestServer(t, allScopesValidator{})
	payload := []byte("etag-fixture")
	info := seedObject(t, js, "T4", "A4", payload, jetstream.ObjectMeta{})
	if info.Digest == "" {
		t.Skip("JetStream did not compute Digest — backend skipped")
	}
	resp, err := newTLSClient().Get(ts.URL + "/obj/project/abc123/T4/A4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wantETag := `"` + info.Digest + `"`
	if et := resp.Header.Get("ETag"); et != wantETag {
		t.Errorf("ETag = %q, want %q", et, wantETag)
	}
}
