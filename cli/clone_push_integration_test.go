package cli_test

import (
	"context"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSessionFossilURLIsLiveHTTPEndpoint verifies sesh's contribution to
// the supported clone-push worker pattern: every running session's
// state JSON must expose a `fossil_url` pointing at a live HTTP server
// that workers can `fossil clone` from.
//
// Substrate-level propagation (clone → commit → push → peer receives)
// is verified upstream by EdgeSync's TestCrossLeaf_HTTPPush_PropagatesCommit.
// Sesh's contract here is narrower: expose the URL and keep the listener
// reachable. This test asserts that contract.
func TestSessionFossilURLIsLiveHTTPEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bin := buildSesh(t)
	home := t.TempDir()
	project := t.TempDir()
	setupGitWorktree(t, project)

	s, stderr := startSesh(t, bin, home, project, "alpha")
	defer killAndWait(t, s, stderr)
	state := waitForURLs(t, filepath.Join(project, ".sesh", "sessions", "alpha.json"), 15*time.Second)

	if state.FossilURL == "" {
		t.Fatalf("session state missing fossil_url; got %+v", state)
	}

	parsed, err := url.Parse(state.FossilURL)
	if err != nil {
		t.Fatalf("parse fossil_url %q: %v", state.FossilURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		t.Errorf("fossil_url scheme = %q, want http/https", parsed.Scheme)
	}
	if parsed.Host == "" {
		t.Errorf("fossil_url has empty host: %q", state.FossilURL)
	}

	dial(t, state.FossilURL, "fossil_url")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, state.FossilURL, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", state.FossilURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", state.FossilURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		t.Errorf("GET %s returned %d (want < 500 — workers' `fossil clone` would fail)",
			state.FossilURL, resp.StatusCode)
	}
}
