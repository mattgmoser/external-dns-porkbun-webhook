// Package webhook implements the HTTP API that external-dns expects from a
// webhook provider.
//
// The protocol (Content-Type: application/external.dns.webhook+json;version=1):
//
//	GET  /                  -> 200, DomainFilter JSON     (negotiate)
//	GET  /records           -> 200, [Endpoint, ...]       (current state)
//	POST /records           -> 204                         (apply changes)
//	POST /adjustendpoints   -> 200, [Endpoint, ...]       (canonicalise)
//	GET  /healthz           -> 200 if alive
//	GET  /readyz            -> 200 if cred-checked + DNS reachable
//	GET  /metrics           -> Prometheus
//
// The DomainFilter response shape comes from endpoint.DomainFilter's own
// MarshalJSON (currently {"include":[...],"exclude":[...]}); we don't dictate
// it here, we just emit whatever the type produces.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
	providerimpl "github.com/mattgmoser/external-dns-porkbun-webhook/provider"
)

// MediaType is the content type external-dns expects.
const MediaType = "application/external.dns.webhook+json;version=1"

const maxRequestBodyBytes = 4 << 20

// Provider is the contract this webhook serves. The provider package's *Provider
// implements it.
type Provider interface {
	Records(ctx context.Context) ([]*endpoint.Endpoint, error)
	ApplyChanges(ctx context.Context, changes *plan.Changes) error
	AdjustEndpoints([]*endpoint.Endpoint) ([]*endpoint.Endpoint, error)
	GetDomainFilter() *endpoint.DomainFilter
}

// Config is the webhook server config.
type Config struct {
	Provider      Provider
	Addr          string // "127.0.0.1:8888" (safe sidecar default)
	OpsAddr       string // ":8080" (healthz, readyz, metrics)
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration // zero disables the deadline; request cancellation still propagates
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
	metrics   *Metrics
}

// New constructs a server.
func New(cfg Config) *Server {
	if cfg.Provider == nil {
		panic("provider required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8888"
	}
	if cfg.OpsAddr == "" {
		cfg.OpsAddr = ":8080"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
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
		Addr:              cfg.Addr,
		Handler:           apiMux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}

	opsMux := http.NewServeMux()
	opsMux.HandleFunc("/healthz", s.handleHealthz)
	opsMux.HandleFunc("/readyz", s.handleReadyz)
	opsMux.Handle("/metrics", s.metrics.Handler())
	opsMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprintln(w, "external-dns-porkbun-webhook")
		fmt.Fprintln(w, "  /healthz   - liveness")
		fmt.Fprintln(w, "  /readyz    - readiness (creds + reachability)")
		fmt.Fprintln(w, "  /metrics   - Prometheus")
	})
	s.opsServer = &http.Server{
		Addr:              cfg.OpsAddr,
		Handler:           opsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	return s
}

// Run starts both servers and blocks until ctx is cancelled. Performs an
// initial readiness check against the configured zone before flipping ready=true.
func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	go s.checkReadiness(runCtx)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		runErr = err
	}
	cancel()
	return errors.Join(runErr, s.shutdown())
}

func (s *Server) checkReadiness(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	first := true
	check := func() {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := s.cfg.Provider.Records(ctx2)
		now := err == nil
		old := s.ready.Swap(now)
		if first || old != now {
			log.WithField("ready", now).WithError(err).Info("readiness changed")
			if s.cfg.OnReadyChange != nil {
				s.cfg.OnReadyChange(now)
			}
		}
		s.metrics.SetReady(now)
		first = false
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
	// "/" is a catch-all in net/http; reject anything that isn't actually root.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	df := s.cfg.Provider.GetDomainFilter()
	w.Header().Set("Content-Type", MediaType)
	w.Header().Set("Vary", "Accept")
	if err := json.NewEncoder(w).Encode(df); err != nil {
		log.WithError(err).Warn("encode domain filter")
	}
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
		writeProviderError(w, err)
		return
	}
	s.metrics.ObserveEndpoints(len(eps))
	w.Header().Set("Content-Type", MediaType)
	if err := json.NewEncoder(w).Encode(eps); err != nil {
		log.WithError(err).Warn("encode records")
	}
}

func (s *Server) applyChanges(w http.ResponseWriter, r *http.Request) {
	if !hasWebhookContentType(r) {
		http.Error(w, "expected webhook media type", http.StatusUnsupportedMediaType)
		return
	}
	var changes plan.Changes
	if err := decodeRequestBody(w, r, &changes); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.metrics.ObserveChanges(len(changes.Create), len(changes.UpdateNew), len(changes.Delete))
	if err := s.cfg.Provider.ApplyChanges(r.Context(), &changes); err != nil {
		log.WithError(err).Error("ApplyChanges failed")
		s.metrics.applyErrors.Add(1)
		writeProviderError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hasWebhookContentType(r) {
		http.Error(w, "expected webhook media type", http.StatusUnsupportedMediaType)
		return
	}
	var eps []*endpoint.Endpoint
	if err := decodeRequestBody(w, r, &eps); err != nil {
		writeDecodeError(w, err)
		return
	}
	out, err := s.cfg.Provider.AdjustEndpoints(eps)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	w.Header().Set("Content-Type", MediaType)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.WithError(err).Warn("encode adjustendpoints")
	}
}

func hasWebhookContentType(r *http.Request) bool {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/external.dns.webhook+json" && params["version"] == "1"
}

func decodeRequestBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("body must contain one JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
}

func writeProviderError(w http.ResponseWriter, err error) {
	status := providerErrorStatus(err)
	message := http.StatusText(status)
	// Validation failures describe only the request ExternalDNS supplied and
	// are useful to operators. Backend errors stay in logs so a legacy exposed
	// deployment does not disclose Porkbun response details to arbitrary Pods.
	var validationErr *providerimpl.ValidationError
	if errors.As(err, &validationErr) {
		message = validationErr.Error()
	}
	http.Error(w, message, status)
}

func providerErrorStatus(err error) int {
	var validationErr *providerimpl.ValidationError
	if errors.As(err, &validationErr) {
		return http.StatusBadRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}

	var apiErr *porkbun.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Retryable || apiErr.HTTPStatus == http.StatusTooManyRequests || apiErr.HTTPStatus >= http.StatusInternalServerError {
			return http.StatusServiceUnavailable
		}
		// A permanent Porkbun rejection is a failure of the webhook's upstream,
		// not of ExternalDNS's authentication to this sidecar.
		return http.StatusBadGateway
	}

	var networkErr net.Error
	if errors.As(err, &networkErr) || errors.Is(err, context.Canceled) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
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
	code        int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
