package porkbun

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := New("k", "s",
		WithBaseURL(srv.URL),
		WithMinGap(0), // disable rate limiting in tests
		WithMaxRetries(2),
	)
	t.Cleanup(srv.Close)
	return c, srv
}

func TestPing(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body baseRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.APIKey != "k" || body.SecretAPIKey != "s" {
			t.Errorf("creds not propagated: %+v", body)
		}
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPingError(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(baseResponse{Status: "ERROR", Message: "bad creds"})
	})
	err := c.Ping(context.Background())
	if err == nil || err.Error() != "porkbun api error: bad creds" {
		t.Errorf("expected api error, got %v", err)
	}
}

func TestRetrieve(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(retrieveResponse{
			baseResponse: baseResponse{Status: "SUCCESS"},
			Records: []Record{
				{ID: "1", Name: "foo.example.com", Type: "A", Content: "1.2.3.4", TTL: "300"},
				{ID: "2", Name: "foo.example.com", Type: "A", Content: "5.6.7.8", TTL: "300"},
			},
		})
	})
	got, err := c.Retrieve(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 records, got %d", len(got))
	}
	if got[0].TTLInt() != 300 {
		t.Errorf("ttl int = %d", got[0].TTLInt())
	}
}

func TestCreate(t *testing.T) {
	var captured RecordInput
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		var body recordRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured = body.RecordInput
		json.NewEncoder(w).Encode(createResponse{
			baseResponse: baseResponse{Status: "SUCCESS"},
			ID:           json.Number("12345"),
		})
	})
	id, err := c.Create(context.Background(), "example.com", RecordInput{Name: "foo", Type: "A", Content: "1.2.3.4", TTL: "600"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" {
		t.Errorf("id = %s", id)
	}
	if captured.Name != "foo" || captured.Type != "A" {
		t.Errorf("not captured: %+v", captured)
	}
}

func TestRetryOn500(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (2 fails + 1 success), got %d", calls.Load())
	}
}

func TestRetryGivesUp(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := calls.Load(); got != 3 { // 1 + maxRetries(2) attempts
		t.Errorf("expected 3 calls, got %d", got)
	}
}

func TestRateLimitGap(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	}))
	defer srv.Close()
	c := New("k", "s", WithBaseURL(srv.URL), WithMinGap(50*time.Millisecond), WithMaxRetries(0))
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	// 4 calls with 50ms gap between each (3 gaps) = ≥ 150ms minimum.
	if elapsed < 130*time.Millisecond {
		t.Errorf("expected rate-limit gaps to slow us down; elapsed=%v", elapsed)
	}
}

func TestNon4xxClientError(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// Should NOT have retried a 400.
}

// ensure response body is closed even on weird input
func TestBodyClose(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	})
	for i := 0; i < 50; i++ {
		_ = c.Ping(context.Background())
	}
}
