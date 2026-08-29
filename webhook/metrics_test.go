package webhook

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Prometheus label values must not be able to close their own quoted string.
// Nothing this package emits today can, but escaping is what keeps that true
// if a future label carries a value the HTTP layer does not police.
func TestEscapeLabelValue(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"records", "records"},
		{"GET", "GET"},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{`x"} injected{y="`, `x\"} injected{y=\"`},
	}
	for _, tc := range tests {
		if got := escapeLabelValue(tc.in); got != tc.want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The rendered exposition must keep one label per dimension even when a value
// contains exposition metacharacters.
func TestMetricsHandlerEscapesLabels(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.ObserveRequest(`records"} evil{x="`, "GET", 200, 5*time.Millisecond)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	// The payload must stay inside its quoted value: it must never begin a
	// series of its own, which is what an unescaped quote would allow.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "evil") {
			t.Fatalf("label value escaped its quoted string and started a new series:\n%s", body)
		}
	}
	if !strings.Contains(body, `route="records\"} evil{x=\""`) {
		t.Fatalf("escaped route label not found in exposition:\n%s", body)
	}
}
