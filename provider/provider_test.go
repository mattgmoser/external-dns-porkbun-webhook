package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
)

// fakePorkbun is a small in-memory mock of the Porkbun JSON API enough for the
// provider tests.
type fakePorkbun struct {
	mu      sync.Mutex
	records map[string]porkbun.Record // by id
	nextID  atomic.Int64
	calls   atomic.Int32
}

func newFakePorkbun() *fakePorkbun {
	return &fakePorkbun{records: map[string]porkbun.Record{}}
}

func (f *fakePorkbun) seed(records ...porkbun.Record) {
	for _, r := range records {
		f.mu.Lock()
		if r.ID == "" {
			id := f.nextID.Add(1)
			r.ID = idStr(id)
		}
		f.records[r.ID] = r
		f.mu.Unlock()
	}
}

func (f *fakePorkbun) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/dns/retrieve/"):
			f.handleRetrieve(w)
		case strings.HasPrefix(path, "/dns/create/"):
			f.handleCreate(w, r)
		case strings.HasPrefix(path, "/dns/delete/"):
			id := strings.TrimPrefix(path, "/dns/delete/")
			id = strings.SplitN(id, "/", 2)[1]
			f.handleDelete(w, id)
		case strings.HasPrefix(path, "/dns/edit/"):
			parts := strings.SplitN(strings.TrimPrefix(path, "/dns/edit/"), "/", 2)
			if len(parts) != 2 {
				http.Error(w, "bad path", 400)
				return
			}
			f.handleEdit(w, r, parts[1])
		case path == "/ping":
			json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS"})
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakePorkbun) handleRetrieve(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := struct {
		Status  string           `json:"status"`
		Records []porkbun.Record `json:"records"`
	}{Status: "SUCCESS"}
	for _, r := range f.records {
		out.Records = append(out.Records, r)
	}
	sort.Slice(out.Records, func(i, j int) bool { return out.Records[i].ID < out.Records[j].ID })
	json.NewEncoder(w).Encode(out)
}

func (f *fakePorkbun) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name, Type, Content, TTL string
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	id := f.nextID.Add(1)
	rec := porkbun.Record{
		ID: idStr(id), Name: body.Name + ".example.com",
		Type: body.Type, Content: body.Content, TTL: body.TTL,
	}
	if body.Name == "" {
		rec.Name = "example.com"
	}
	f.records[rec.ID] = rec
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "id": id})
}

func (f *fakePorkbun) handleDelete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	delete(f.records, id)
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"status": "SUCCESS"})
}

func (f *fakePorkbun) handleEdit(w http.ResponseWriter, r *http.Request, id string) {
	var body struct{ Name, Type, Content, TTL string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	if rec, ok := f.records[id]; ok {
		if body.Type != "" {
			rec.Type = body.Type
		}
		if body.Content != "" {
			rec.Content = body.Content
		}
		if body.TTL != "" {
			rec.TTL = body.TTL
		}
		f.records[id] = rec
	}
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"status": "SUCCESS"})
}

func idStr(n int64) string {
	return "id-" + intToString(n)
}

func intToString(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// ----- tests -----

func newTestProvider(t *testing.T, fake *fakePorkbun) *Provider {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	prov, err := New(Config{
		APIKey:       "k",
		SecretAPIKey: "s",
		Domain:       "example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// re-wire client to test server
	prov.client = porkbun.New("k", "s",
		porkbun.WithBaseURL(srv.URL),
		porkbun.WithMinGap(0),
		porkbun.WithMaxRetries(0),
	)
	return prov
}

func TestRecordsCollapsesMultiTarget(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{Name: "foo.example.com", Type: "A", Content: "1.1.1.1", TTL: "300"},
		porkbun.Record{Name: "foo.example.com", Type: "A", Content: "2.2.2.2", TTL: "300"},
	)
	prov := newTestProvider(t, fake)
	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
	if len(eps[0].Targets) != 2 {
		t.Errorf("expected 2 targets, got %d", len(eps[0].Targets))
	}
}

func TestRecordsSkipsUnmanagedTypes(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{Name: "example.com", Type: "NS", Content: "ns1.porkbun.com", TTL: "600"},
		porkbun.Record{Name: "example.com", Type: "SOA", Content: "soa", TTL: "600"},
		porkbun.Record{Name: "foo.example.com", Type: "A", Content: "1.1.1.1", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Errorf("expected 1 managed endpoint, got %d", len(eps))
	}
}

func TestApplyChangesCreate(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)

	err := prov.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "argocd.example.com", RecordType: "A", Targets: []string{"192.168.1.1"}, RecordTTL: 600},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 1 {
		t.Errorf("expected 1 record created, got %d", len(fake.records))
	}
}

func TestApplyChangesDelete(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(porkbun.Record{Name: "foo.example.com", Type: "A", Content: "1.1.1.1", TTL: "600"})
	prov := newTestProvider(t, fake)
	err := prov.ApplyChanges(context.Background(), &plan.Changes{
		Delete: []*endpoint.Endpoint{
			{DNSName: "foo.example.com", RecordType: "A", Targets: []string{"1.1.1.1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 0 {
		t.Errorf("expected 0 records, got %d", len(fake.records))
	}
}

func TestApplyChangesUpdateTargets(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(porkbun.Record{Name: "foo.example.com", Type: "A", Content: "1.1.1.1", TTL: "600"})
	prov := newTestProvider(t, fake)
	old := &endpoint.Endpoint{DNSName: "foo.example.com", RecordType: "A", Targets: []string{"1.1.1.1"}, RecordTTL: 600}
	newEP := &endpoint.Endpoint{DNSName: "foo.example.com", RecordType: "A", Targets: []string{"2.2.2.2"}, RecordTTL: 600}
	err := prov.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{old},
		UpdateNew: []*endpoint.Endpoint{newEP},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One of the records should have content 2.2.2.2 now
	found := false
	for _, r := range fake.records {
		if r.Content == "2.2.2.2" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected record with new target to exist; records=%+v", fake.records)
	}
}

func TestAdjustEndpointsBumpsTTL(t *testing.T) {
	prov, _ := New(Config{APIKey: "k", SecretAPIKey: "s", Domain: "example.com"})
	eps := []*endpoint.Endpoint{{DNSName: "foo.example.com", RecordType: "A", RecordTTL: 60}}
	out, _ := prov.AdjustEndpoints(eps)
	if out[0].RecordTTL != 600 {
		t.Errorf("ttl should have been bumped to 600, got %d", out[0].RecordTTL)
	}
}

func TestSubdomainOf(t *testing.T) {
	p := &Provider{domain: "example.com"}
	cases := map[string]string{
		"foo.example.com":     "foo",
		"foo.bar.example.com": "foo.bar",
		"example.com":         "",
	}
	for in, want := range cases {
		if got := p.subdomainOf(in); got != want {
			t.Errorf("subdomainOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormaliseTXTContent(t *testing.T) {
	if got := normaliseContent("TXT", "\"v=spf1 ~all\""); got != "v=spf1 ~all" {
		t.Errorf("got %q", got)
	}
	if got := normaliseContent("A", "1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("got %q", got)
	}
}

func TestDryRun(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)
	prov.dryRun = true

	err := prov.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			{DNSName: "x.example.com", RecordType: "A", Targets: []string{"1.1.1.1"}, RecordTTL: 600},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 0 {
		t.Errorf("dry-run created records: %+v", fake.records)
	}
}
