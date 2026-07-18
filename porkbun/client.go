// Package porkbun is a thin client for the Porkbun JSON DNS API.
//
// Auth: every request body must include {"apikey":..., "secretapikey":...}.
// All endpoints are POST. Responses always include {"status":"SUCCESS"|"ERROR", "message":"..."}.
//
// The client transparently retries on 5xx and transient network errors with
// exponential backoff, and respects Porkbun's documented per-key rate limits
// (1 req/sec) by serializing concurrent calls.
package porkbun

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL         = "https://api.porkbun.com/api/json/v3"
	defaultUserAgent       = "external-dns-porkbun-webhook"
	defaultRateMinGap      = 1100 * time.Millisecond // > 1 req/sec, with safety margin
	defaultHTTPTimeout     = 10 * time.Second
	defaultMaxRetries      = 2
	maxServerRetryDelay    = 10 * time.Second
	maxErrorResponseBytes  = 64 << 10
	idempotencyKeyByteSize = 16
)

// Client is a Porkbun DNS API client.
type Client struct {
	apiKey       string
	secretAPIKey string
	baseURL      string
	userAgent    string
	httpClient   *http.Client

	// Rate limiting: Porkbun enforces ~1 req/sec per key. Serialize calls.
	rateMu   sync.Mutex
	lastCall time.Time
	minGap   time.Duration

	// Retries
	maxRetries int
	maxBackoff time.Duration
}

// Option configures the client.
type Option func(*Client)

// WithBaseURL overrides the API base URL (useful for tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient overrides the HTTP client. Redirects are still rejected so a
// credential-bearing request body cannot be forwarded to another endpoint.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.userAgent = ua } }

// WithMinGap sets the minimum gap between requests (rate limiting).
func WithMinGap(d time.Duration) Option { return func(c *Client) { c.minGap = d } }

// WithMaxRetries sets the max number of retry attempts on transient errors.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New returns a Porkbun client. Pass options to override defaults.
func New(apiKey, secretAPIKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:       apiKey,
		secretAPIKey: secretAPIKey,
		baseURL:      defaultBaseURL,
		userAgent:    defaultUserAgent,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
		minGap:     defaultRateMinGap,
		maxRetries: defaultMaxRetries,
		maxBackoff: 8 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// Clone rather than mutate a caller-provided client. Porkbun requests carry
	// credentials in their POST bodies, so following a 307/308 redirect could
	// disclose them to a different host.
	redirectSafeClient := *c.httpClient
	redirectSafeClient.CheckRedirect = rejectRedirect
	c.httpClient = &redirectSafeClient
	return c
}

// Record is a Porkbun DNS record.
type Record struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     string `json:"ttl"`
	Prio    string `json:"prio,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

// TTLInt returns the TTL as an int (Porkbun returns strings).
func (r Record) TTLInt() int {
	v, _ := strconv.Atoi(r.TTL)
	return v
}

type baseRequest struct {
	APIKey       string `json:"apikey"`
	SecretAPIKey string `json:"secretapikey"`
}

type baseResponse struct {
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

func (r *baseResponse) responseBase() *baseResponse { return r }

type retrieveResponse struct {
	baseResponse
	Records []Record `json:"records"`
}

// Ping calls /ping to validate credentials. Useful at startup.
func (c *Client) Ping(ctx context.Context) error {
	var resp baseResponse
	if err := c.do(ctx, "/ping", baseRequest{c.apiKey, c.secretAPIKey}, &resp); err != nil {
		return err
	}
	return checkStatus(resp)
}

// Retrieve returns all DNS records for a domain.
func (c *Client) Retrieve(ctx context.Context, domain string) ([]Record, error) {
	var resp retrieveResponse
	if err := c.do(ctx, "/dns/retrieve/"+domain, baseRequest{c.apiKey, c.secretAPIKey}, &resp); err != nil {
		return nil, err
	}
	if err := checkStatus(resp.baseResponse); err != nil {
		return nil, err
	}
	return resp.Records, nil
}

// RecordInput is the body of a create/edit-record call.
type RecordInput struct {
	Name    string `json:"name,omitempty"` // subdomain (without root); empty = root
	Type    string `json:"type"`           // A, AAAA, CNAME, TXT, MX, etc.
	Content string `json:"content"`
	TTL     string `json:"ttl,omitempty"` // seconds; default 600 minimum
	Prio    string `json:"prio,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

type recordRequest struct {
	baseRequest
	RecordInput
}

type createResponse struct {
	baseResponse
	ID json.Number `json:"id"`
}

// Create creates a DNS record. Returns the new record's ID.
func (c *Client) Create(ctx context.Context, domain string, in RecordInput) (string, error) {
	req := recordRequest{
		baseRequest: baseRequest{c.apiKey, c.secretAPIKey},
		RecordInput: in,
	}
	var resp createResponse
	if err := c.do(ctx, "/dns/create/"+domain, req, &resp); err != nil {
		return "", err
	}
	if err := checkStatus(resp.baseResponse); err != nil {
		return "", err
	}
	return resp.ID.String(), nil
}

// Edit replaces a record by ID.
func (c *Client) Edit(ctx context.Context, domain, id string, in RecordInput) error {
	req := recordRequest{
		baseRequest: baseRequest{c.apiKey, c.secretAPIKey},
		RecordInput: in,
	}
	var resp baseResponse
	if err := c.do(ctx, "/dns/edit/"+domain+"/"+id, req, &resp); err != nil {
		return err
	}
	return checkStatus(resp)
}

// Delete removes a record by ID.
func (c *Client) Delete(ctx context.Context, domain, id string) error {
	var resp baseResponse
	if err := c.do(ctx, "/dns/delete/"+domain+"/"+id, baseRequest{c.apiKey, c.secretAPIKey}, &resp); err != nil {
		return err
	}
	return checkStatus(resp)
}

// ----- internals -----

// APIError describes an error returned by the Porkbun API. HTTPStatus is zero
// when no HTTP response was available. RetryAfter is populated from a valid
// Retry-After or X-RateLimit-Reset response header.
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
	RequestID  string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return "porkbun api error"
	}

	message := e.Message
	if message == "" {
		message = "(no message)"
	}

	details := make([]string, 0, 2)
	// Keep the long-standing error text for application errors returned with
	// HTTP 200 while still exposing the status through HTTPStatus.
	if e.HTTPStatus >= http.StatusBadRequest {
		details = append(details, fmt.Sprintf("http %d", e.HTTPStatus))
	}
	if e.Code != "" {
		details = append(details, "code "+e.Code)
	}

	prefix := "porkbun api error"
	if len(details) != 0 {
		prefix += " (" + strings.Join(details, ", ") + ")"
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (request id %s)", prefix, message, e.RequestID)
	}
	return prefix + ": " + message
}

func checkStatus(r baseResponse) error {
	if r.Status != "SUCCESS" {
		return apiErrorFromResponse(0, r, nil)
	}
	return nil
}

// do performs a request with retry, backoff, and rate limiting.
func (c *Client) do(ctx context.Context, path string, body any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		return fmt.Errorf("generating idempotency key: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay, retry := c.retryDelay(attempt, lastErr)
			if !retry {
				return lastErr
			}
			if err := waitContext(ctx, delay); err != nil {
				return err
			}
		}
		if err := c.waitRate(ctx); err != nil {
			return err
		}
		err := c.doOnce(ctx, path, body, out, idempotencyKey)
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

// waitRate blocks until at least minGap has passed since the previous call.
// If ctx is cancelled while waiting it returns ctx.Err without updating
// lastCall, so the next caller's pacing isn't shifted by an aborted wait.
func (c *Client) waitRate(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		c.rateMu.Lock()
		if err := ctx.Err(); err != nil {
			c.rateMu.Unlock()
			return err
		}
		now := time.Now()
		wait := c.minGap - now.Sub(c.lastCall)
		if c.lastCall.IsZero() || wait <= 0 {
			c.lastCall = now
			c.rateMu.Unlock()
			return nil
		}
		c.rateMu.Unlock()

		// Do not hold rateMu during the wait. Other callers can then observe
		// their own context cancellation; after waking, each caller rechecks
		// lastCall so only one can claim the next request slot.
		if err := waitContext(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) computeBackoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * time.Second
	if base > c.maxBackoff {
		base = c.maxBackoff
	}
	// jitter ±25 %
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter - base/4
}

func (c *Client) retryDelay(attempt int, lastErr error) (time.Duration, bool) {
	delay := c.computeBackoff(attempt)
	var apiErr *APIError
	if !errors.As(lastErr, &apiErr) || apiErr.RetryAfter <= delay {
		return delay, true
	}
	// Waiting longer than this without a caller-supplied deadline could tie up
	// a webhook request indefinitely. Do not retry early when the server asks
	// for a longer pause; return the typed error to the caller instead.
	if apiErr.RetryAfter > maxServerRetryDelay {
		return 0, false
	}
	return apiErr.RetryAfter, true
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newIdempotencyKey() (string, error) {
	random := make([]byte, idempotencyKeyByteSize)
	if _, err := cryptorand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", random), nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func (c *Client) doOnce(ctx context.Context, path string, body any, out any, idempotencyKey string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &transientError{err}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBytes))
		return apiErrorFromHTTP(resp.StatusCode, resp.Header, responseBody)
	}
	responseTarget, commitResponse, err := freshResponseTarget(out)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(responseTarget); err != nil {
		return transientDecodeError(ctx, err)
	}
	// A second decode must reach a clean EOF. This catches a transport read
	// failure after the first JSON value and rejects unexpected trailing data.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return transientDecodeError(ctx, err)
	}
	if response, ok := responseTarget.(interface{ responseBase() *baseResponse }); ok {
		base := response.responseBase()
		if base.Status != "SUCCESS" {
			return apiErrorFromResponse(resp.StatusCode, *base, resp.Header)
		}
	}
	commitResponse()
	return nil
}

func freshResponseTarget(out any) (any, func(), error) {
	value := reflect.ValueOf(out)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return nil, nil, errors.New("response target must be a non-nil pointer")
	}
	fresh := reflect.New(value.Elem().Type())
	return fresh.Interface(), func() {
		value.Elem().Set(fresh.Elem())
	}, nil
}

func transientDecodeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &transientError{fmt.Errorf("decoding response: %w", err)}
}

func apiErrorFromHTTP(status int, header http.Header, body []byte) *APIError {
	type errorEnvelope struct {
		Message        string          `json:"message"`
		Code           json.RawMessage `json:"code"`
		RequestID      string          `json:"requestId"`
		RequestIDUpper string          `json:"requestID"`
		RequestIDSnake string          `json:"request_id"`
	}

	var envelope errorEnvelope
	structured := json.Unmarshal(body, &envelope) == nil
	message := envelope.Message
	if message == "" {
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" && !structured {
			message = trimmed
		} else {
			message = http.StatusText(status)
		}
	}
	requestID := firstNonEmpty(envelope.RequestID, envelope.RequestIDUpper, envelope.RequestIDSnake, requestIDFromHeader(header))
	code := rawJSONText(envelope.Code)
	retryAfter, _ := retryAfterFromHeader(header, time.Now())
	return &APIError{
		HTTPStatus: status,
		Code:       code,
		Message:    message,
		RequestID:  requestID,
		Retryable:  retryableAPIError(status, code, message),
		RetryAfter: retryAfter,
	}
}

func apiErrorFromResponse(status int, response baseResponse, header http.Header) *APIError {
	retryAfter, _ := retryAfterFromHeader(header, time.Now())
	requestID := response.RequestID
	if requestID == "" {
		requestID = requestIDFromHeader(header)
	}
	return &APIError{
		HTTPStatus: status,
		Code:       response.Code,
		Message:    response.Message,
		RequestID:  requestID,
		Retryable:  retryableAPIError(status, response.Code, response.Message),
		RetryAfter: retryAfter,
	}
}

func retryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func retryableAPIError(status int, code, message string) bool {
	if strings.EqualFold(strings.TrimSpace(code), "IDEMPOTENCY_KEY_MISMATCH") {
		return false
	}
	if status == http.StatusConflict {
		return strings.EqualFold(strings.TrimSpace(code), "IDEMPOTENCY_KEY_IN_USE")
	}
	return retryableHTTPStatus(status) || retryablePorkbunError(code, message)
}

func retryablePorkbunError(code, message string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "RATE_LIMIT_EXCEEDED" || code == "TOO_MANY_REQUESTS" {
		return true
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests")
}

func requestIDFromHeader(header http.Header) string {
	return firstNonEmpty(header.Get("X-Request-ID"), header.Get("Request-ID"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func rawJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}

func retryAfterFromHeader(header http.Header, now time.Time) (time.Duration, bool) {
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			return durationFromSeconds(seconds)
		}
		if retryAt, err := http.ParseTime(value); err == nil {
			if !retryAt.After(now) {
				return 0, true
			}
			return retryAt.Sub(now), true
		}
	}

	if value := strings.TrimSpace(header.Get("X-RateLimit-Reset")); value != "" {
		reset, err := strconv.ParseInt(value, 10, 64)
		if err != nil || reset < 0 {
			return 0, false
		}
		// Rate-limit reset headers conventionally contain a Unix timestamp. Also
		// accept small values as relative seconds for compatible proxies/mocks.
		if reset < 1_000_000_000 {
			return durationFromSeconds(reset)
		}
		if reset > 100_000_000_000 {
			reset /= 1000 // tolerate Unix milliseconds
		}
		resetAt := time.Unix(reset, 0)
		if !resetAt.After(now) {
			return 0, true
		}
		return resetAt.Sub(now), true
	}

	return 0, false
}

func durationFromSeconds(seconds int64) (time.Duration, bool) {
	const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))
	if seconds > maxDurationSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

type transientError struct{ err error }

func (e *transientError) Error() string { return "transient: " + e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable
	}
	var te *transientError
	if errors.As(err, &te) {
		return true
	}
	// network-level errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
