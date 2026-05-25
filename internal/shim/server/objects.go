package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/objects"

	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/methods"
)

// knownScopeKinds mirrors sesh-ops/objects.knownScopeKinds (unexported
// upstream). The handler validates {scopeKind} against this set before
// composing the bucket name; calling scope.Bucket here would reject
// session-scope ids on download because sanitization is one-way: a
// session id written as "myproj.alpha" lands as "myproj_alpha" in the
// bucket name, and the obj:// URI's ParseURI returns the sanitized form
// — scope.Bucket then refuses it for not containing ".".
//
// TODO(sesh-ops): switch to objects.OpenByBucket once that API lands;
// in-line bucket-name composition is a contained workaround.
var knownScopeKinds = map[string]bool{
	"hub":      true,
	"project":  true,
	"session":  true,
	"workflow": true,
	"agent":    true,
}

// objectReadScope is the OAuth-style scope a Principal must carry to
// download bytes from the per-scope ObjectStore. Mirrors the
// `agent.read` gate listTasks uses — coarse, per-task ACLs deferred
// per plan §3.
const objectReadScope = "agent.read"

// handleObjectGet streams the bytes addressed by the URL-templated path
// /obj/{scopeKind}/{scopeID}/{taskID}/{artifactID} to the caller.
//
// Auth: Bearer scope agent.read. Mux mounts this behind
// auth.Middleware(cfg.Auth), so the request reaches here only if the
// validator accepted the token. Absent principal ⇒ 401; principal
// without scope ⇒ 403.
//
// Body: io.Copy from jetstream.ObjectStore.Get reader; headers
// (Content-Type, Content-Length, ETag, Content-Disposition,
// Cache-Control) are set from GetInfo BEFORE the copy starts so a
// streamed response carries Content-Length and integrity headers.
// A client disconnect mid-stream surfaces as an io.Copy error after
// headers; we log it but cannot retroactively update the HTTP status.
func (s *server) handleObjectGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.FromContext(ctx)
	if !ok {
		// Reaching here without a Principal would be a middleware bug
		// (mux mounts this behind auth.Middleware). Defensive 401.
		w.Header().Set("WWW-Authenticate", `Bearer realm="sesh-shim"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !methods.HasScope(principal, objectReadScope) {
		http.Error(w, "missing scope "+objectReadScope, http.StatusForbidden)
		return
	}

	kind := r.PathValue("scopeKind")
	id := r.PathValue("scopeID")
	taskID := r.PathValue("taskID")
	artifactID := r.PathValue("artifactID")
	if kind == "" || id == "" || taskID == "" || artifactID == "" {
		http.Error(w, "missing path segment", http.StatusBadRequest)
		return
	}

	if !knownScopeKinds[kind] {
		http.Error(w, "invalid scope kind", http.StatusBadRequest)
		return
	}

	// Compose the bucket name directly. ParseURI returns the sanitized
	// scope-id (dots/dashes already mapped to underscores), so the bucket
	// name we want is exactly "sesh_objects_<kind>" or
	// "sesh_objects_<kind>_<id>" (hub is the only id-less kind). We open
	// the store by name to skip scope.Bucket's session "must contain ."
	// validation, which the sanitized id can never satisfy.
	bucket := "sesh_objects_" + kind
	if id != "" {
		bucket = bucket + "_" + id
	}
	store, err := s.cfg.JS.ObjectStore(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.log.Error("obj.get: open store", "bucket", bucket, "err", err)
		http.Error(w, "object store unavailable", http.StatusInternalServerError)
		return
	}

	key := objects.Key(taskID, artifactID)
	info, err := store.GetInfo(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrObjectNotFound) || errors.Is(err, jetstream.ErrBucketNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.log.Error("obj.get: get info", "kind", kind, "id", id, "key", key, "err", err)
		http.Error(w, "object info unavailable", http.StatusInternalServerError)
		return
	}

	res, err := store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrObjectNotFound) || errors.Is(err, jetstream.ErrBucketNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.log.Error("obj.get: get reader", "kind", kind, "id", id, "key", key, "err", err)
		http.Error(w, "object unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = res.Close() }()

	// Headers MUST be set before the first body write so streaming
	// clients (curl --max-filesize, browsers) see them up front.
	ct := headerValue(info, "Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatUint(info.Size, 10))
	if info.Digest != "" {
		// Strong validator wrap: ETag values are quoted strings per
		// RFC 7232 §2.3; the digest is content-addressed so it's a
		// strong validator (no weak `W/` prefix).
		w.Header().Set("ETag", `"`+info.Digest+`"`)
	}
	filename := info.Name
	if hv := headerValue(info, "Filename"); hv != "" {
		filename = hv
	}
	if filename != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}
	// Artifact IDs are content-addressed (ULID/hash); the bytes behind
	// a given (task,artifact) tuple are immutable. Cache aggressively.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, res); err != nil {
		// Headers already flushed; can't change status. Log for ops
		// visibility — most causes are client disconnect mid-stream.
		s.log.Warn("obj.get: copy interrupted", "kind", kind, "id", id, "key", key, "err", err)
	}
}

// headerValue extracts a header from an ObjectInfo by name (case-
// sensitive — nats.Header is a net/textproto-style map but adapters
// today write canonical casing). Returns "" when absent.
func headerValue(info *jetstream.ObjectInfo, name string) string {
	if info == nil || info.Headers == nil {
		return ""
	}
	vs := info.Headers[name]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}
