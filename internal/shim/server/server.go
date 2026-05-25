package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/sesh-ops/scope"

	"github.com/danmestas/sesh/internal/shim/a2a"
	"github.com/danmestas/sesh/internal/shim/auth"
	"github.com/danmestas/sesh/internal/shim/card"
	"github.com/danmestas/sesh/internal/shim/jsonrpc"
	"github.com/danmestas/sesh/internal/shim/methods"
	"github.com/danmestas/sesh/internal/shim/push"
)

// Config wires the shim's HTTP server to its collaborators. All fields
// except ShutdownGrace are required; ShutdownGrace defaults to 5s. JS
// is the jetstream.JetStream v2 client; main.go obtains it from
// sesh-ops/conn.Connect.
type Config struct {
	Listen        string
	TLSCert       string
	TLSKey        string
	Dev           bool
	Auth          auth.Validator
	Card          *card.Cache
	Signer        *card.Signer
	NC            *nats.Conn
	JS            jetstream.JetStream
	AgentKey      card.AgentKey
	ScopeKind     string
	ScopeID       string
	Machine       string
	Logger        *slog.Logger
	ShutdownGrace time.Duration

	// GatewayURL is the public-facing absolute URL clients reach the
	// shim at. Used by the a2a.Translator (Slice 7) to rewrite obj://
	// Part URLs to dereferenceable HTTPS. Required outside Dev mode;
	// empty in Dev means obj:// URLs pass through unchanged (with a
	// one-shot INFO log).
	GatewayURL string

	// Push notification (Slice 6) wiring. Zero values disable the
	// feature: nil PushKey ⇒ the 4 CRUD methods return -32008 and
	// the delivery worker is NOT started.
	PushKey               []byte
	PushDevAllowLocalhost bool
	PushWorkerDisabled    bool
	PushMaxRetries        int
}

// server holds the mutable runtime state (counters) behind the http.Handler.
type server struct {
	cfg        Config
	log        *slog.Logger
	dispatcher *methods.Dispatcher
	translator *a2a.Translator

	mu            sync.Mutex
	httpReqs      map[httpKey]uint64
	jsonrpcErrs   map[jsonrpcErrKey]uint64
	cardCacheHits uint64
	cardCacheMiss uint64
}

type httpKey struct {
	method string
	route  string
	status int
}

type jsonrpcErrKey struct {
	code int
	name string
}

func newServer(cfg Config) *server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	translator := a2a.NewTranslator(cfg.GatewayURL, cfg.Logger)
	s := &server{
		cfg:         cfg,
		log:         cfg.Logger,
		translator:  translator,
		httpReqs:    map[httpKey]uint64{},
		jsonrpcErrs: map[jsonrpcErrKey]uint64{},
	}
	s.dispatcher = methods.NewDispatcher(methods.Deps{
		NC:                    cfg.NC,
		JS:                    cfg.JS,
		ScopeKind:             cfg.ScopeKind,
		ScopeID:               cfg.ScopeID,
		AgentKey:              cfg.AgentKey,
		Machine:               cfg.Machine,
		Log:                   cfg.Logger,
		Composer:              cfg.Card.Composer(),
		Signer:                cfg.Signer,
		PushKey:               cfg.PushKey,
		PushDevAllowLocalhost: cfg.PushDevAllowLocalhost,
		Translator:            translator,
	})
	return s
}

// New constructs the *http.Server with all routes mounted. Caller owns
// lifecycle; use Run for the conventional cancel-on-context pattern.
func New(cfg Config) (*http.Server, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	s := newServer(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", s.wrap("/.well-known/agent-card.json", s.handleAgentCard))
	mux.HandleFunc("GET /.well-known/jwks.json", s.wrap("/.well-known/jwks.json", s.handleJWKS))
	mux.HandleFunc("GET /healthz", s.wrap("/healthz", s.handleHealthz))
	mux.HandleFunc("GET /readyz", s.wrap("/readyz", s.handleReadyz))
	mux.HandleFunc("GET /metrics", s.wrap("/metrics", s.handleMetrics))

	a2aHandler := http.HandlerFunc(s.handleA2A)
	mux.Handle("POST /a2a", auth.Middleware(cfg.Auth)(s.wrapHandler("/a2a", a2aHandler)))

	// Slice 7: gateway-rooted object download. Mounted under the same
	// JWT/Bearer middleware as /a2a; the handler enforces the
	// `agent.read` scope. Constant metrics label `/obj` keeps the
	// cardinality bounded (NO templated `{scopeKind}` leak).
	mux.Handle("GET "+a2a.ObjectPathPrefix+"{scopeKind}/{scopeID}/{taskID}/{artifactID}",
		auth.Middleware(cfg.Auth)(s.wrapHandler("/obj", http.HandlerFunc(s.handleObjectGet))))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv, nil
}

// Run starts the HTTPS server and blocks until ctx is cancelled, then
// performs a graceful Shutdown bounded by cfg.ShutdownGrace. When
// cfg.PushKey is non-nil and cfg.PushWorkerDisabled is false, a
// push delivery worker is started alongside the HTTP server; on
// shutdown the worker is cancelled FIRST so its in-flight deliveries
// race the ShutdownGrace timer rather than the HTTP Shutdown.
func Run(ctx context.Context, cfg Config) error {
	srv, err := New(cfg)
	if err != nil {
		return err
	}
	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	useDevTLS := cfg.TLSCert == "" && cfg.TLSKey == "" && cfg.Dev
	if useDevTLS {
		cert, err := generateSelfSignedTLS()
		if err != nil {
			return fmt.Errorf("dev tls: %w", err)
		}
		srv.TLSConfig.Certificates = []tls.Certificate{cert}
	}

	// Push delivery worker (Slice 6). Started before the HTTP listener
	// so a webhook fires for any state-change that happens before the
	// listener is ready.
	var worker *push.Worker
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	if cfg.PushKey != nil && !cfg.PushWorkerDisabled {
		if err := ensureNotifyFailBucket(workerCtx, cfg); err != nil {
			return fmt.Errorf("notifyfail bucket: %w", err)
		}
		worker = push.NewWorker(push.WorkerConfig{
			NC:                cfg.NC,
			JS:                cfg.JS,
			ScopeKind:         cfg.ScopeKind,
			ScopeID:           cfg.ScopeID,
			PushKey:           cfg.PushKey,
			Log:               cfg.Logger,
			MaxRetries:        cfg.PushMaxRetries,
			DevAllowLocalhost: cfg.PushDevAllowLocalhost,
		})
		go func() {
			if err := worker.Run(workerCtx); err != nil && cfg.Logger != nil {
				cfg.Logger.Error("push worker: exited with error", "err", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if useDevTLS {
			serveErr = srv.ListenAndServeTLS("", "")
		} else {
			serveErr = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// Cancel the worker FIRST so its in-flight deliveries race
		// against ShutdownGrace rather than the HTTP Shutdown.
		cancelWorker()
		if worker != nil {
			workerDone := make(chan struct{})
			go func() {
				worker.Wait()
				close(workerDone)
			}()
			select {
			case <-workerDone:
			case <-time.After(grace):
			}
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		shutErr := srv.Shutdown(shutCtx)
		serveErr := <-errCh
		if shutErr != nil {
			return shutErr
		}
		return serveErr
	case err := <-errCh:
		return err
	}
}

// ensureNotifyFailBucket creates the per-scope notifyfail KV bucket
// with a 168h (7-day) TTL when absent. The TTL bounds the unbounded-
// growth risk noted in plan §11; operators inspecting the dead-letter
// log have a week to act before entries roll off.
func ensureNotifyFailBucket(ctx context.Context, cfg Config) error {
	bucket, err := scope.Bucket(cfg.ScopeKind, cfg.ScopeID, "notifyfail")
	if err != nil {
		return err
	}
	if _, err := cfg.JS.KeyValue(ctx, bucket); err == nil {
		return nil
	} else if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return err
	}
	_, err = cfg.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: bucket,
		TTL:    168 * time.Hour,
	})
	if err == nil {
		return nil
	}
	// Race: another process may have created it between our Open and Create.
	if _, err2 := cfg.JS.KeyValue(ctx, bucket); err2 == nil {
		return nil
	}
	return err
}

func validate(cfg Config) error {
	if cfg.Listen == "" {
		return errors.New("server.Config: Listen is required")
	}
	if cfg.Auth == nil {
		return errors.New("server.Config: Auth is required (use auth.NoopValidator{} for dev)")
	}
	if cfg.Card == nil {
		return errors.New("server.Config: Card cache is required")
	}
	if cfg.Signer == nil {
		return errors.New("server.Config: Signer is required")
	}
	if cfg.NC == nil {
		return errors.New("server.Config: NC is required")
	}
	if cfg.JS == nil {
		return errors.New("server.Config: JS is required")
	}
	hasCert := cfg.TLSCert != "" && cfg.TLSKey != ""
	noCert := cfg.TLSCert == "" && cfg.TLSKey == ""
	if !hasCert && !(noCert && cfg.Dev) {
		return errors.New("server.Config: must provide both TLSCert and TLSKey, or set Dev=true with neither")
	}
	if _, err := scope.Bucket(cfg.ScopeKind, cfg.ScopeID, "tasks"); err != nil {
		return fmt.Errorf("server.Config: invalid scope (kind=%q id=%q): %w", cfg.ScopeKind, cfg.ScopeID, err)
	}
	// Slice 7: outside --dev mode, GatewayURL is required so the
	// obj:// → HTTPS rewrite has a non-empty origin. --dev keeps the
	// pre-Slice-7 behaviour (pass-through with a one-shot INFO log)
	// to ease local manual smoke testing.
	if cfg.GatewayURL == "" && !cfg.Dev {
		return errors.New("server.Config: GatewayURL is required outside --dev mode (Slice 7)")
	}
	return nil
}

// wrap is the metrics-counting middleware shim for HandlerFunc routes.
func (s *server) wrap(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		s.incHTTP(r.Method, route, sw.status)
	}
}

// wrapHandler does the same for plain http.Handler (used after auth.Middleware).
func (s *server) wrapHandler(route string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		s.incHTTP(r.Method, route, sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (s *server) incHTTP(method, route string, status int) {
	s.mu.Lock()
	s.httpReqs[httpKey{method, route, status}]++
	s.mu.Unlock()
}

func (s *server) incJSONRPCErr(code int, name string) {
	s.mu.Lock()
	s.jsonrpcErrs[jsonrpcErrKey{code, name}]++
	s.mu.Unlock()
}

func (s *server) incCardCache(hit bool) {
	s.mu.Lock()
	if hit {
		s.cardCacheHits++
	} else {
		s.cardCacheMiss++
	}
	s.mu.Unlock()
}

// ---- handlers ----

func (s *server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	// Probe before compose to count hits vs misses. Counter is best-effort:
	// a concurrent request between probe and GetOrCompose can flip the
	// outcome, but that's acceptable for ops telemetry.
	s.incCardCache(s.cfg.Card.HasFresh(s.cfg.AgentKey))
	b, err := s.cfg.Card.GetOrCompose(r.Context(), s.cfg.AgentKey)
	if err != nil {
		s.log.Error("agent-card: compose", "err", err)
		http.Error(w, "compose failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	b, err := s.cfg.Signer.JWKS()
	if err != nil {
		s.log.Error("jwks: marshal", "err", err)
		http.Error(w, "jwks failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status":      "ok",
		"nats":        s.cfg.NC.Status().String(),
		"signing_key": s.cfg.Signer.Loaded(),
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	if s.cfg.NC.Status() != nats.CONNECTED {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready": status == http.StatusOK,
		"nats":  s.cfg.NC.Status().String(),
	})
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.writeMetrics(w)
}

func (s *server) writeMetrics(w io.Writer) {
	s.mu.Lock()
	httpSnap := make(map[httpKey]uint64, len(s.httpReqs))
	for k, v := range s.httpReqs {
		httpSnap[k] = v
	}
	rpcSnap := make(map[jsonrpcErrKey]uint64, len(s.jsonrpcErrs))
	for k, v := range s.jsonrpcErrs {
		rpcSnap[k] = v
	}
	hits, miss := s.cardCacheHits, s.cardCacheMiss
	s.mu.Unlock()

	fmt.Fprintln(w, "# HELP sesh_shim_http_requests_total HTTP requests by method, route, status.")
	fmt.Fprintln(w, "# TYPE sesh_shim_http_requests_total counter")
	httpKeys := make([]httpKey, 0, len(httpSnap))
	for k := range httpSnap {
		httpKeys = append(httpKeys, k)
	}
	sort.Slice(httpKeys, func(i, j int) bool {
		if httpKeys[i].route != httpKeys[j].route {
			return httpKeys[i].route < httpKeys[j].route
		}
		if httpKeys[i].method != httpKeys[j].method {
			return httpKeys[i].method < httpKeys[j].method
		}
		return httpKeys[i].status < httpKeys[j].status
	})
	for _, k := range httpKeys {
		fmt.Fprintf(w, "sesh_shim_http_requests_total{method=%q,route=%q,status=\"%d\"} %d\n",
			k.method, k.route, k.status, httpSnap[k])
	}

	fmt.Fprintln(w, "# HELP sesh_shim_jsonrpc_errors_total JSON-RPC errors by code and A2A name.")
	fmt.Fprintln(w, "# TYPE sesh_shim_jsonrpc_errors_total counter")
	rpcKeys := make([]jsonrpcErrKey, 0, len(rpcSnap))
	for k := range rpcSnap {
		rpcKeys = append(rpcKeys, k)
	}
	sort.Slice(rpcKeys, func(i, j int) bool {
		if rpcKeys[i].code != rpcKeys[j].code {
			return rpcKeys[i].code < rpcKeys[j].code
		}
		return rpcKeys[i].name < rpcKeys[j].name
	})
	for _, k := range rpcKeys {
		fmt.Fprintf(w, "sesh_shim_jsonrpc_errors_total{code=\"%d\",name=%q} %d\n",
			k.code, k.name, rpcSnap[k])
	}

	fmt.Fprintln(w, "# HELP sesh_shim_card_cache_hits_total AgentCard cache hits.")
	fmt.Fprintln(w, "# TYPE sesh_shim_card_cache_hits_total counter")
	fmt.Fprintf(w, "sesh_shim_card_cache_hits_total %d\n", hits)

	fmt.Fprintln(w, "# HELP sesh_shim_card_cache_misses_total AgentCard cache misses.")
	fmt.Fprintln(w, "# TYPE sesh_shim_card_cache_misses_total counter")
	fmt.Fprintf(w, "sesh_shim_card_cache_misses_total %d\n", miss)
}

// handleA2A is the JSON-RPC dispatch entry point. Reads body, decodes
// envelope, routes to one of the registered methods. All application
// errors return HTTP 200 with a JSON-RPC error object.
func (s *server) handleA2A(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		// Body-too-large is a transport-level error and bypasses the
		// JSON-RPC envelope (HTTP 413). All other read errors are
		// treated as malformed payload and surfaced via -32700.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		s.incJSONRPCErr(jsonrpc.ErrParse.Code, jsonrpc.ErrParse.Name)
		jsonrpc.WriteError(w, nil, jsonrpc.ErrParse)
		return
	}
	req, jerr := jsonrpc.Decode(body)
	if jerr != nil {
		s.incJSONRPCErr(jerr.Code, jerr.Name)
		jsonrpc.WriteError(w, nil, jerr)
		return
	}

	if s.dispatcher.IsStreaming(req.Method) {
		switch req.Method {
		case methods.MethodSendStreamingMessage:
			s.dispatcher.SendStreamingMessage(w, r, req.Params)
		case methods.MethodSubscribeToTask:
			s.dispatcher.SubscribeToTask(w, r, req.Params)
		}
		return
	}

	result, jerr := s.dispatcher.Dispatch(r.Context(), req.Method, req.Params)
	if jerr != nil {
		s.incJSONRPCErr(jerr.Code, jerr.Name)
		jsonrpc.WriteError(w, req.ID, jerr)
		return
	}
	jsonrpc.WriteResponse(w, req.ID, result)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
