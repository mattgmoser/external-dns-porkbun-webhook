package webhook

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
	providerimpl "github.com/mattgmoser/external-dns-porkbun-webhook/provider"
)

type fakeProvider struct {
	mu         sync.Mutex
	records    []*endpoint.Endpoint
	recordsErr error
	changes    *plan.Changes
	applyErr   error
	adjustErr  error
	filter     *endpoint.DomainFilter
}

func (p *fakeProvider) Records(context.Context) ([]*endpoint.Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.records, p.recordsErr
}

func (p *fakeProvider) ApplyChanges(_ context.Context, changes *plan.Changes) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changes = changes
	return p.applyErr
}

func (p *fakeProvider) AdjustEndpoints(eps []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	if p.adjustErr != nil {
		return nil, p.adjustErr
	}
	for _, ep := range eps {
		if ep.RecordTTL > 0 && ep.RecordTTL < 600 {
			ep.RecordTTL = 600
		}
	}
	return eps, nil
}

func TestProviderErrorsUseMeaningfulHTTPStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		hide string
	}{
		{name: "invalid change set", err: &providerimpl.ValidationError{Err: errors.New("outside zone")}, want: http.StatusBadRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{name: "Porkbun rate limit", err: &porkbun.APIError{HTTPStatus: http.StatusTooManyRequests, Message: "backend detail", Retryable: true}, want: http.StatusServiceUnavailable, hide: "backend detail"},
		{name: "Porkbun permanent rejection", err: &porkbun.APIError{HTTPStatus: http.StatusForbidden}, want: http.StatusBadGateway},
		{name: "internal error", err: errors.New("unexpected"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeProvider{applyErr: tt.err}
			s := newHandlerServer(p)
			rr := serveAPI(s, http.MethodPost, "/records", MediaType, []byte(`{}`))
			if rr.Code != tt.want {
				t.Fatalf("status=%d, want=%d body=%s", rr.Code, tt.want, rr.Body.String())
			}
			if tt.hide != "" && strings.Contains(rr.Body.String(), tt.hide) {
				t.Fatalf("response body disclosed backend detail: %s", rr.Body.String())
			}
		})
	}
}

func (p *fakeProvider) GetDomainFilter() *endpoint.DomainFilter { return p.filter }

func newHandlerServer(p *fakeProvider) *Server {
	if p.filter == nil {
		p.filter = endpoint.NewDomainFilter([]string{"example.com"})
	}
	return New(Config{Provider: p})
}

func serveAPI(s *Server, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	s.apiServer.Handler.ServeHTTP(rr, req)
	return rr
}

func serveOps(s *Server, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	s.opsServer.Handler.ServeHTTP(rr, req)
	return rr
}

func TestWebhookProtocolHandlers(t *testing.T) {
	p := &fakeProvider{records: []*endpoint.Endpoint{{
		DNSName: "app.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}, RecordTTL: 600,
	}}}
	s := newHandlerServer(p)

	t.Run("negotiate", func(t *testing.T) {
		rr := serveAPI(s, http.MethodGet, "/", "", nil)
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != MediaType {
			t.Fatalf("status=%d content-type=%q body=%s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "example.com") {
			t.Fatalf("domain filter missing from %s", rr.Body.String())
		}
	})

	t.Run("records", func(t *testing.T) {
		rr := serveAPI(s, http.MethodGet, "/records", "", nil)
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != MediaType {
			t.Fatalf("status=%d content-type=%q body=%s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "app.example.com") {
			t.Fatalf("endpoint missing from %s", rr.Body.String())
		}
	})

	t.Run("apply", func(t *testing.T) {
		body := []byte(`{"create":[{"dnsName":"new.example.com","targets":["192.0.2.2"],"recordType":"A","recordTTL":600}]}`)
		rr := serveAPI(s, http.MethodPost, "/records", MediaType, body)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.changes == nil || len(p.changes.Create) != 1 || p.changes.Create[0].DNSName != "new.example.com" {
			t.Fatalf("changes = %#v", p.changes)
		}
	})

	t.Run("adjust", func(t *testing.T) {
		body := []byte(`[{"dnsName":"new.example.com","targets":["192.0.2.2"],"recordType":"A","recordTTL":60}]`)
		rr := serveAPI(s, http.MethodPost, "/adjustendpoints", MediaType, body)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"recordTTL":600`) {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestWebhookRejectsInvalidRequests(t *testing.T) {
	s := newHandlerServer(&fakeProvider{})

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        []byte
		want        int
	}{
		{name: "unknown path", method: http.MethodGet, path: "/nope", want: http.StatusNotFound},
		{name: "root method", method: http.MethodPost, path: "/", want: http.StatusMethodNotAllowed},
		{name: "records method", method: http.MethodPut, path: "/records", want: http.StatusMethodNotAllowed},
		{name: "missing media type", method: http.MethodPost, path: "/records", body: []byte(`{}`), want: http.StatusUnsupportedMediaType},
		{name: "wrong version", method: http.MethodPost, path: "/records", contentType: "application/external.dns.webhook+json;version=2", body: []byte(`{}`), want: http.StatusUnsupportedMediaType},
		{name: "malformed json", method: http.MethodPost, path: "/records", contentType: MediaType, body: []byte(`{`), want: http.StatusBadRequest},
		{name: "trailing json", method: http.MethodPost, path: "/records", contentType: MediaType, body: []byte(`{} {}`), want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := serveAPI(s, tt.method, tt.path, tt.contentType, tt.body)
			if rr.Code != tt.want {
				t.Fatalf("status=%d, want=%d body=%s", rr.Code, tt.want, rr.Body.String())
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	s := newHandlerServer(&fakeProvider{})
	body := []byte(`{"padding":"` + strings.Repeat("x", maxRequestBodyBytes) + `"}`)
	rr := serveAPI(s, http.MethodPost, "/records", MediaType, body)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReadinessChecksZoneRecords(t *testing.T) {
	readyChange := make(chan bool, 1)
	p := &fakeProvider{records: []*endpoint.Endpoint{}}
	s := New(Config{
		Provider:      p,
		OnReadyChange: func(ready bool) { readyChange <- ready },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.checkReadiness(ctx)
		close(done)
	}()

	select {
	case ready := <-readyChange:
		if !ready {
			t.Fatal("initial readiness change was false")
		}
	case <-time.After(time.Second):
		t.Fatal("readiness check did not complete")
	}

	p.mu.Lock()
	p.recordsErr = errors.New("zone unavailable")
	p.mu.Unlock()
	// Run an immediate check rather than waiting for the production ticker.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go s.checkReadiness(ctx2)
	select {
	case ready := <-readyChange:
		if ready {
			t.Fatal("readiness stayed true after Records failed")
		}
	case <-time.After(time.Second):
		t.Fatal("failed readiness check did not complete")
	}
	cancel2()
	cancel()
	<-done
}

func TestServerDefaultsAreSidecarSafe(t *testing.T) {
	s := newHandlerServer(&fakeProvider{})
	if s.apiServer.Addr != "127.0.0.1:8888" {
		t.Fatalf("webhook address = %q", s.apiServer.Addr)
	}
	if s.apiServer.WriteTimeout != 0 {
		t.Fatalf("write timeout = %s, want disabled", s.apiServer.WriteTimeout)
	}
}

func TestOpsRootRejectsUnknownPathsAndMethods(t *testing.T) {
	s := newHandlerServer(&fakeProvider{})
	if rr := serveOps(s, http.MethodGet, "/not-a-probe"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown ops path status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := serveOps(s, http.MethodPost, "/"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("ops root POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := serveOps(s, http.MethodGet, "/"); rr.Code != http.StatusOK {
		t.Fatalf("ops root GET status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunStopsCleanlyWhenContextIsCanceled(t *testing.T) {
	apiAddr := freeLoopbackAddr(t)
	opsAddr := freeLoopbackAddr(t)
	s := New(Config{Provider: &fakeProvider{}, Addr: apiAddr, OpsAddr: opsAddr})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		default:
		}
	})

	waitForHTTP(t, "http://"+apiAddr+"/")
	waitForHTTP(t, "http://"+opsAddr+"/healthz")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunBindFailureStopsSiblingServer(t *testing.T) {
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocked.Close() })
	opsAddr := freeLoopbackAddr(t)
	s := New(Config{Provider: &fakeProvider{}, Addr: blocked.Addr().String(), OpsAddr: opsAddr})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "webhook server") {
			t.Fatalf("Run bind error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after listener bind failure")
	}

	conn, err := net.DialTimeout("tcp", opsAddr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("ops sibling is still accepting connections after webhook bind failure")
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", url)
}
