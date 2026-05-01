package webhook

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a small lock-light Prometheus metric set.
//
// We don't pull in the prometheus client_golang library to keep the binary
// small (the entire helm-shipped image is ~15 MB this way). The exposition
// format is text-based and trivial to render by hand.
type Metrics struct {
	mu sync.Mutex

	// counters
	requestsTotal map[routeKey]uint64
	applyErrors   atomic.Uint64

	// histograms (simple bucketed)
	requestDuration map[routeKey]*histogram

	// gauges
	endpointsCurrent atomic.Int64
	createsTotal     atomic.Uint64
	updatesTotal     atomic.Uint64
	deletesTotal     atomic.Uint64
	ready            atomic.Int32
}

type routeKey struct {
	route, method string
	code          int
}

// NewMetrics returns a fresh Metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		requestsTotal:   map[routeKey]uint64{},
		requestDuration: map[routeKey]*histogram{},
	}
}

// ObserveRequest records a webhook HTTP call.
func (m *Metrics) ObserveRequest(route, method string, code int, dur time.Duration) {
	k := routeKey{route, method, code}
	m.mu.Lock()
	m.requestsTotal[k]++
	h, ok := m.requestDuration[k]
	if !ok {
		h = newHistogram()
		m.requestDuration[k] = h
	}
	m.mu.Unlock()
	h.Observe(dur.Seconds())
}

// ObserveEndpoints records the count returned by Records().
func (m *Metrics) ObserveEndpoints(n int) { m.endpointsCurrent.Store(int64(n)) }

// ObserveChanges records change counts from ApplyChanges.
func (m *Metrics) ObserveChanges(create, update, del int) {
	m.createsTotal.Add(uint64(create))
	m.updatesTotal.Add(uint64(update))
	m.deletesTotal.Add(uint64(del))
}

// SetReady toggles the ready gauge.
func (m *Metrics) SetReady(ok bool) {
	if ok {
		m.ready.Store(1)
	} else {
		m.ready.Store(0)
	}
}

// Handler returns an http.Handler that exposes the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		mm := m.snapshot()

		w.Write([]byte("# HELP edns_porkbun_requests_total HTTP requests received by the webhook.\n"))
		w.Write([]byte("# TYPE edns_porkbun_requests_total counter\n"))
		for k, v := range mm.requests {
			w.Write([]byte("edns_porkbun_requests_total{route=\"" + k.route + "\",method=\"" + k.method + "\",code=\"" + strconv.Itoa(k.code) + "\"} " + strconv.FormatUint(v, 10) + "\n"))
		}

		w.Write([]byte("# HELP edns_porkbun_request_duration_seconds Webhook HTTP duration histogram.\n"))
		w.Write([]byte("# TYPE edns_porkbun_request_duration_seconds histogram\n"))
		for k, h := range mm.duration {
			labels := "route=\"" + k.route + "\",method=\"" + k.method + "\",code=\"" + strconv.Itoa(k.code) + "\""
			h.WriteTo(w, "edns_porkbun_request_duration_seconds", labels)
		}

		w.Write([]byte("# HELP edns_porkbun_endpoints Endpoints currently advertised.\n"))
		w.Write([]byte("# TYPE edns_porkbun_endpoints gauge\n"))
		w.Write([]byte("edns_porkbun_endpoints " + strconv.FormatInt(mm.endpoints, 10) + "\n"))

		w.Write([]byte("# HELP edns_porkbun_apply_errors_total ApplyChanges failures.\n"))
		w.Write([]byte("# TYPE edns_porkbun_apply_errors_total counter\n"))
		w.Write([]byte("edns_porkbun_apply_errors_total " + strconv.FormatUint(mm.applyErrors, 10) + "\n"))

		w.Write([]byte("# HELP edns_porkbun_changes_total Total endpoints requested per change kind.\n"))
		w.Write([]byte("# TYPE edns_porkbun_changes_total counter\n"))
		w.Write([]byte("edns_porkbun_changes_total{kind=\"create\"} " + strconv.FormatUint(mm.creates, 10) + "\n"))
		w.Write([]byte("edns_porkbun_changes_total{kind=\"update\"} " + strconv.FormatUint(mm.updates, 10) + "\n"))
		w.Write([]byte("edns_porkbun_changes_total{kind=\"delete\"} " + strconv.FormatUint(mm.deletes, 10) + "\n"))

		w.Write([]byte("# HELP edns_porkbun_ready 1 if provider readiness probe passing.\n"))
		w.Write([]byte("# TYPE edns_porkbun_ready gauge\n"))
		w.Write([]byte("edns_porkbun_ready " + strconv.FormatInt(int64(mm.ready), 10) + "\n"))
	})
}

type metricsSnapshot struct {
	requests    map[routeKey]uint64
	duration    map[routeKey]*histogram
	endpoints   int64
	applyErrors uint64
	creates     uint64
	updates     uint64
	deletes     uint64
	ready       int32
}

func (m *Metrics) snapshot() metricsSnapshot {
	m.mu.Lock()
	reqs := make(map[routeKey]uint64, len(m.requestsTotal))
	for k, v := range m.requestsTotal {
		reqs[k] = v
	}
	dur := make(map[routeKey]*histogram, len(m.requestDuration))
	for k, v := range m.requestDuration {
		dur[k] = v
	}
	m.mu.Unlock()
	return metricsSnapshot{
		requests:    reqs,
		duration:    dur,
		endpoints:   m.endpointsCurrent.Load(),
		applyErrors: m.applyErrors.Load(),
		creates:     m.createsTotal.Load(),
		updates:     m.updatesTotal.Load(),
		deletes:     m.deletesTotal.Load(),
		ready:       m.ready.Load(),
	}
}

// ----- histogram -----

var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

func newHistogram() *histogram {
	return &histogram{
		buckets: defaultBuckets,
		counts:  make([]uint64, len(defaultBuckets)),
	}
}

func (h *histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.count++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

func (h *histogram) WriteTo(w http.ResponseWriter, name, labels string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.buckets {
		w.Write([]byte(name + "_bucket{" + labels + ",le=\"" + strconv.FormatFloat(b, 'f', -1, 64) + "\"} " + strconv.FormatUint(h.counts[i], 10) + "\n"))
	}
	w.Write([]byte(name + "_bucket{" + labels + ",le=\"+Inf\"} " + strconv.FormatUint(h.count, 10) + "\n"))
	w.Write([]byte(name + "_sum{" + labels + "} " + strconv.FormatFloat(h.sum, 'f', -1, 64) + "\n"))
	w.Write([]byte(name + "_count{" + labels + "} " + strconv.FormatUint(h.count, 10) + "\n"))
}
