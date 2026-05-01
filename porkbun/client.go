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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL    = "https://api.porkbun.com/api/json/v3"
	defaultUserAgent  = "external-dns-porkbun-webhook"
	defaultRateMinGap = 1100 * time.Millisecond // > 1 req/sec, with safety margin
)

// Client is a Porkbun DNS API client.
type Client struct {
	apiKey       string
	secretAPIKey string
	baseURL      string
	userAgent    string
	httpClient   *http.Client

	// Rate limiting: Porkbun enforces ~1 req/sec per key. Serialize calls.
	rateMu    sync.Mutex
	lastCall  time.Time
	minGap    time.Duration

	// Retries
	maxRetries  int
	maxBackoff  time.Duration
}

// Option configures the client.
type Option func(*Client)

// WithBaseURL overrides the API base URL (useful for tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient overrides the HTTP client.
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
			Timeout: 30 * time.Second,
		},
		minGap:     defaultRateMinGap,
		maxRetries: 4,
		maxBackoff: 8 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
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
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

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
	Name    string `json:"name,omitempty"`    // subdomain (without root); empty = root
	Type    string `json:"type"`              // A, AAAA, CNAME, TXT, MX, etc.
	Content string `json:"content"`
	TTL     string `json:"ttl,omitempty"`     // seconds; default 600 minimum
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

func checkStatus(r baseResponse) error {
	if r.Status != "SUCCESS" {
		msg := r.Message
		if msg == "" {
			msg = "(no message)"
		}
		return fmt.Errorf("porkbun api error: %s", msg)
	}
	return nil
}

// do performs a request with retry, backoff, and rate limiting.
func (c *Client) do(ctx context.Context, path string, body any, out any) error {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.computeBackoff(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		c.waitRate(ctx)
		err := c.doOnce(ctx, path, body, out)
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
func (c *Client) waitRate(ctx context.Context) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	now := time.Now()
	if !c.lastCall.IsZero() {
		elapsed := now.Sub(c.lastCall)
		if elapsed < c.minGap {
			wait := c.minGap - elapsed
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
	c.lastCall = time.Now()
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

func (c *Client) doOnce(ctx context.Context, path string, body any, out any) error {
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &transientError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &transientError{fmt.Errorf("porkbun http %d: %s", resp.StatusCode, string(body))}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("porkbun http %d: %s", resp.StatusCode, string(body))
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

type transientError struct{ err error }

func (e *transientError) Error() string { return "transient: " + e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	if err == nil {
		return false
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
