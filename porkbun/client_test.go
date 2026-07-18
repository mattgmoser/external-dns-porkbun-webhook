package porkbun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	c.maxBackoff = time.Millisecond
	t.Cleanup(srv.Close)
	return c, srv
}

func TestDefaultRequestBudget(t *testing.T) {
	c := New("k", "s")
	if c.httpClient.Timeout != 10*time.Second {
		t.Errorf("default HTTP timeout = %v, want 10s", c.httpClient.Timeout)
	}
	if c.maxRetries != 2 {
		t.Errorf("default max retries = %d, want 2", c.maxRetries)
	}
}

func TestPing(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.HTTPStatus != http.StatusOK || apiErr.Message != "bad creds" || apiErr.Retryable {
		t.Errorf("unexpected APIError: %+v", apiErr)
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
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
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

func TestIdempotencyKeyStableAcrossRetry(t *testing.T) {
	var keysMu sync.Mutex
	var keys []string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		keysMu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		call := len(keys)
		keysMu.Unlock()
		if call == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "ERROR",
				"message": "try again",
			})
			return
		}
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	})

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("received %d keys, want 2", len(keys))
	}
	if keys[0] == "" || len(keys[0]) != idempotencyKeyByteSize*2 {
		t.Fatalf("invalid idempotency key %q", keys[0])
	}
	if keys[0] != keys[1] {
		t.Errorf("idempotency key changed across retry: %q != %q", keys[0], keys[1])
	}
}

func TestIdempotencyKeyDiffersAcrossCalls(t *testing.T) {
	var keys []string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	})

	for range 2 {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("Ping() error = %v", err)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("received %d keys, want 2", len(keys))
	}
	if keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Errorf("logical calls did not receive distinct keys: %q, %q", keys[0], keys[1])
	}
}

func TestIdempotencyConflictRetryPolicy(t *testing.T) {
	t.Run("key in use is retried", func(t *testing.T) {
		var calls atomic.Int32
		var keysMu sync.Mutex
		var keys []string
		c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			keysMu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			keysMu.Unlock()
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{
					"status":  "ERROR",
					"code":    "IDEMPOTENCY_KEY_IN_USE",
					"message": "request is still in progress",
				})
				return
			}
			json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
		})

		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("Ping() error = %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("server received %d calls, want 2", calls.Load())
		}
		keysMu.Lock()
		defer keysMu.Unlock()
		if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
			t.Errorf("retry did not reuse idempotency key: %v", keys)
		}
	})

	t.Run("key mismatch is never retried", func(t *testing.T) {
		var calls atomic.Int32
		c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "ERROR",
				"code":    "IDEMPOTENCY_KEY_MISMATCH",
				"message": "key was reused with another request",
			})
		})

		err := c.Ping(context.Background())
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *APIError, got %T: %v", err, err)
		}
		if apiErr.Code != "IDEMPOTENCY_KEY_MISMATCH" || apiErr.Retryable {
			t.Errorf("unexpected APIError: %+v", apiErr)
		}
		if calls.Load() != 1 {
			t.Errorf("mismatch response received %d calls, want 1", calls.Load())
		}
	})
}

func TestTruncated2xxResponseRetriesWithSameIdempotencyKey(t *testing.T) {
	var calls atomic.Int32
	var keysMu sync.Mutex
	var keys []string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		keysMu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		keysMu.Unlock()
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"status":"SUCCESS","records":[{"id":"stale","name":"stale.example.com"}`)
			return
		}
		json.NewEncoder(w).Encode(retrieveResponse{
			baseResponse: baseResponse{Status: "SUCCESS"},
			Records: []Record{
				{ID: "fresh", Name: "fresh.example.com", Type: "A", Content: "192.0.2.1", TTL: "300"},
			},
		})
	})

	records, err := c.Retrieve(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(records) != 1 || records[0].ID != "fresh" {
		t.Fatalf("Retrieve() records = %+v, want only fresh replay response", records)
	}
	if calls.Load() != 2 {
		t.Fatalf("server received %d calls, want 2", calls.Load())
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("decode retry did not reuse idempotency key: %v", keys)
	}
}

func TestInterrupted2xxResponseDoesNotCommitAttemptState(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	c := New("k", "s",
		WithMinGap(0),
		WithMaxRetries(2),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var body io.ReadCloser
			if calls.Add(1) == 1 {
				body = &interruptingBody{
					Reader: strings.NewReader(`{"status":"SUCCESS","records":[{"id":"stale"}]}`),
					err:    io.ErrUnexpectedEOF,
				}
			} else {
				// Omitting records ensures state decoded from the interrupted attempt
				// would be visible if it had been committed prematurely.
				body = io.NopCloser(strings.NewReader(`{"status":"SUCCESS"}`))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    r,
			}, nil
		})}),
	)
	c.maxBackoff = time.Millisecond

	records, err := c.Retrieve(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("Retrieve() records = %+v, want no stale attempt state", records)
	}
	if calls.Load() != 2 {
		t.Fatalf("transport received %d calls, want 2", calls.Load())
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("read retry did not reuse idempotency key: %v", keys)
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
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// Should NOT have retried a 400.
	if calls.Load() != 1 {
		t.Errorf("400 response received %d calls, want 1", calls.Load())
	}
}

func TestStructuredHTTPError(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "ERROR",
			"code":      "INVALID_RECORD",
			"message":   "record is invalid",
			"requestId": "req-123",
		})
	})

	err := c.Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusBadRequest || apiErr.Code != "INVALID_RECORD" ||
		apiErr.Message != "record is invalid" || apiErr.RequestID != "req-123" || apiErr.Retryable {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
	if calls.Load() != 1 {
		t.Errorf("non-retryable response received %d calls, want 1", calls.Load())
	}
}

func TestStructuredRetryableHTTPError(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "header-request-id")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ERROR",
			"code":    "RATE_LIMIT_EXCEEDED",
			"message": "slow down",
		})
	})
	c.maxRetries = 0

	err := c.Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusTooManyRequests || apiErr.Code != "RATE_LIMIT_EXCEEDED" ||
		apiErr.Message != "slow down" || apiErr.RequestID != "header-request-id" ||
		!apiErr.Retryable || apiErr.RetryAfter != 2*time.Second {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestStructuredServerError(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status":     "ERROR",
			"code":       "UPSTREAM_UNAVAILABLE",
			"message":    "temporarily unavailable",
			"request_id": "req-503",
		})
	})
	c.maxRetries = 0

	err := c.Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatus != http.StatusServiceUnavailable || apiErr.Code != "UPSTREAM_UNAVAILABLE" ||
		apiErr.Message != "temporarily unavailable" || apiErr.RequestID != "req-503" || !apiErr.Retryable {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestRateWaitPropagatesContextCancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	}))
	defer srv.Close()
	c := New("k", "s", WithBaseURL(srv.URL), WithMinGap(time.Hour), WithMaxRetries(0))

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := c.Ping(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Ping() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("cancellation propagation took %v", elapsed)
	}
	if calls.Load() != 1 {
		t.Errorf("server received %d calls, want 1", calls.Load())
	}
}

func TestContendedRateWaitIsContextCancellable(t *testing.T) {
	c := New("k", "s", WithMinGap(time.Hour))
	c.lastCall = time.Now()

	blockerCtx, cancelBlocker := context.WithCancel(context.Background())
	blockerDone := make(chan error, 1)
	started := make(chan struct{})
	c.rateMu.Lock()
	go func() {
		close(started)
		blockerDone <- c.waitRate(blockerCtx)
	}()
	<-started
	c.rateMu.Unlock()
	// Give the first waiter time to enter the long pacing wait. A fallback
	// cancellation prevents this regression test from hanging with the old
	// implementation, which held rateMu for that entire wait.
	time.Sleep(10 * time.Millisecond)
	fallback := time.AfterFunc(250*time.Millisecond, cancelBlocker)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	startedAt := time.Now()
	err := c.waitRate(ctx)
	elapsed := time.Since(startedAt)
	cancel()
	_ = fallback.Stop()
	cancelBlocker()
	<-blockerDone

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitRate() error = %v, want context deadline exceeded", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("contended cancellation took %v", elapsed)
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		json.NewEncoder(w).Encode(baseResponse{Status: "SUCCESS"})
	}))
	defer destination.Close()

	var originCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		http.Redirect(w, r, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	// Even a supplied client whose policy permits redirects must be hardened.
	c := New("secret-key", "secret-api-key",
		WithBaseURL(origin.URL),
		WithMinGap(0),
		WithMaxRetries(2),
		WithHTTPClient(&http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return nil }}),
	)
	err := c.Ping(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusTemporaryRedirect || apiErr.Retryable {
		t.Fatalf("Ping() error = %v, want non-retryable redirect APIError", err)
	}
	if originCalls.Load() != 1 {
		t.Errorf("origin received %d calls, want 1", originCalls.Load())
	}
	if destinationCalls.Load() != 0 {
		t.Errorf("redirect destination received %d credential-bearing calls, want 0", destinationCalls.Load())
	}
}

func TestRetryAfterWaitHonorsContext(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"status": "ERROR", "message": "slow down"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	err := c.Ping(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ping() error = %v, want context deadline exceeded", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server received %d calls, want 1 before Retry-After elapsed", calls.Load())
	}
}

func TestRetryAfterAboveSafetyLimitDoesNotRetryEarly(t *testing.T) {
	var calls atomic.Int32
	c, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"status": "ERROR", "message": "slow down"})
	})

	started := time.Now()
	err := c.Ping(context.Background())
	if time.Since(started) > time.Second {
		t.Fatal("Ping() waited for a server retry delay above the safety limit")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Minute {
		t.Fatalf("Ping() error = %v, want APIError with one-minute RetryAfter", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server received %d calls, want 1", calls.Load())
	}
}

func TestRetryAfterFromHeader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{name: "retry after seconds", header: http.Header{"Retry-After": []string{"3"}}, want: 3 * time.Second},
		{name: "retry after date", header: http.Header{"Retry-After": []string{now.Add(4 * time.Second).Format(http.TimeFormat)}}, want: 4 * time.Second},
		{name: "rate limit reset relative", header: http.Header{"X-Ratelimit-Reset": []string{"5"}}, want: 5 * time.Second},
		{name: "rate limit reset unix", header: http.Header{"X-Ratelimit-Reset": []string{strconv.FormatInt(now.Add(6*time.Second).Unix(), 10)}}, want: 6 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := retryAfterFromHeader(tt.header, now)
			if !ok || got != tt.want {
				t.Errorf("retryAfterFromHeader() = (%v, %v), want (%v, true)", got, ok, tt.want)
			}
		})
	}
}

func TestBodyClose(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader(`{"status":"SUCCESS"}`)}
	c := New("k", "s",
		WithMinGap(0),
		WithMaxRetries(0),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    r,
			}, nil
		})}),
	)

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("response body was not closed")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

type interruptingBody struct {
	*strings.Reader
	err error
}

func (b *interruptingBody) Read(buffer []byte) (int, error) {
	read, err := b.Reader.Read(buffer)
	if err == nil && b.Len() == 0 {
		return read, b.err
	}
	if errors.Is(err, io.EOF) {
		return read, b.err
	}
	return read, err
}

func (b *interruptingBody) Close() error { return nil }

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}
