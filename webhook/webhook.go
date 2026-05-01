// Package webhook implements the HTTP API that external-dns expects from a
// webhook provider.
//
// The protocol (Content-Type: application/external.dns.webhook+json;version=1):
//
//	GET  /                  → 200, {"filters": [...]}    (negotiate + return DomainFilter)
//	GET  /records           → 200, [Endpoint, ...]       (current state)
//	POST /records           → 204                         (apply changes)
//	POST /adjustendpoints   → 200, [Endpoint, ...]       (canonicalise)
//	GET  /healthz           → 200 if alive
//	GET  /readyz            → 200 if cred-checked + DNS reachable
//	GET  /metrics           → Prometheus
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// MediaType is the content type external-dns expects.
const MediaType = "application/external.dns.webhook+json;version=1"

// Provider is the contract this webhook serves. The provider package's *Provider
// implements it.
type Provider interface {
	Records(ctx context.Context) ([]*endpoint.Endpoint, error)
	ApplyChanges(ctx context.Context, changes *plan.Changes) error
	AdjustEndpoints([]*endpoint.Endpoint) ([]*endpoint.Endpoint, error)
	GetDomainFilter() *endpoint.DomainFilter
	Ping(ctx context.Context) error
}

// Config is the webhook server config.
type Config struct {
	Provider      Provider
	Addr          string // ":8888" (external-dns default)
	OpsAddr       string // ":8080" (healthz, readyz, metrics)
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	Metrics       *Metrics // optional; created internally if nil
	OnReadyChange func(bool)
}

// Server runs the two HTTP servers (webhook + ops). Use New to construct.
type Server struct {
	cfg       Config
	apiServer *http.Server
	opsServer *http.Server
	ready     atomic.Bool
	wg        sync.WaitGroup
	mux       *http.ServeMux
	metrics   *Metrics
}

// New constructs a server.
func New(cfg Config) *Server {
	if cfg.Provider == nil {
		panic("provider required")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8888"
	}
	if cfg.OpsAddr == "" {
		cfg.OpsAddr = ":8080"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 60 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 90 * time.Second
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewMetrics()
	}
	s := &Server{cfg: cfg, metrics: cfg.Metrics}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/", s.withMetrics("root", s.handleRoot))
	apiMux.HandleFunc("/records", s.withMetrics("records", s.handleRecords))
	apiMux.HandleFunc("/adjustendpoints", s.withMetrics("adjustendpoints", s.handleAdjust))
	s.apiServer = &http.Server{
		Addr:         cfg.Addr,
		Handler:      apiMux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	opsMux := http.NewServeMux()
	opsMux.HandleFunc("/healthz", s.handleHealthz)
	opsMux.HandleFunc("/readyz", s.handleReadyz)
	opsMux.Handle("/metrics", s.metrics.Handler())
	opsMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "external-dns-porkbun-webhook")
		fmt.Fprintln(w, "  /healthz   - liveness")
		fmt.Fprintln(w, "  /readyz    - readiness (creds + reachability)")
		fmt.Fprintln(w, "  /metrics   - Prometheus")
	})
	s.opsServer = &http.Server{
		Addr:         cfg.OpsAddr,
		Handler:      opsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	return s
}

// Run starts both servers and blocks until ctx is cancelled. Performs an
// initial readiness check (Ping + Records) before flipping ready=true.
func (s *Server) Run(ctx context.Context) error {
	s.wg.Add(2)
	errCh := make(chan error, 2)

	go func() {
		defer s.wg.Done()
		log.WithField("addr", s.apiServer.Addr).Info("webhook listening")
		if err := s.apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("webhook server: %w", err)
		}
	}()
	go func() {
		defer s.wg.Done()
		log.WithField("addr", s.opsServer.Addr).Info("ops listening")
		if err := s.opsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("ops server: %w", err)
		}
	}()

	go s.checkReadiness(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	return s.shutdown()
}

func (s *Server) checkReadiness(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	check := func() {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := s.cfg.Provider.Ping(ctx2)
		now := err == nil
		if old := s.ready.Swap(now); old != now {
			log.WithField("ready", now).WithError(err).Info("readiness changed")
			if s.cfg.OnReadyChange != nil {
				s.cfg.OnReadyChange(now)
			}
			s.metrics.SetReady(now)
		}
	}
	check() // initial
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func (s *Server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	apiErr := s.apiServer.Shutdown(ctx)
	opsErr := s.opsServer.Shutdown(ctx)
	s.wg.Wait()
	return errors.Join(apiErr, opsErr)
}

// ----- handlers -----

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	df := s.cfg.Provider.GetDomainFilter()
	w.Header().Set("Content-Type", MediaType)
	w.Header().Set("Vary", "Content-Type")
	json.NewEncoder(w).Encode(df)
}

func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getRecords(w, r)
	case http.MethodPost:
		s.applyChanges(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getRecords(w http.ResponseWriter, r *http.Request) {
	eps, err := s.cfg.Provider.Records(r.Context())
	if err != nil {
		log.WithError(err).Error("Records failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.ObserveEndpoints(len(eps))
	w.Header().Set("Content-Type", MediaType)
	json.NewEncoder(w).Encode(eps)
}

func (s *Server) applyChanges(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/external.dns.webhook+json") {
		http.Error(w, "expected webhook media type", http.StatusUnsupportedMediaType)
		return
	}
	var changes plan.Changes
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.metrics.ObserveChanges(len(changes.Create), len(changes.UpdateNew), len(changes.Delete))
	if err := s.cfg.Provider.ApplyChanges(r.Context(), &changes); err != nil {
		log.WithError(err).Error("ApplyChanges failed")
		s.metrics.applyErrors.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/external.dns.webhook+json") {
		http.Error(w, "expected webhook media type", http.StatusUnsupportedMediaType)
		return
	}
	var eps []*endpoint.Endpoint
	if err := json.NewDecoder(r.Body).Decode(&eps); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.cfg.Provider.AdjustEndpoints(eps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", MediaType)
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.ready.Load() {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintln(w, "not ready (credentials / reachability)")
}

// withMetrics wraps a handler to record per-route metrics.
func (s *Server) withMetrics(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, code: 200}
		h(rw, r)
		s.metrics.ObserveRequest(route, r.Method, rw.code, time.Since(start))
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
