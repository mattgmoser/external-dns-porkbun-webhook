package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
)

// fakePorkbun is a small in-memory mock of the Porkbun JSON API enough for the
// provider tests.
type fakePorkbun struct {
	mu             sync.Mutex
	records        map[string]porkbun.Record // by id
	nextID         atomic.Int64
	calls          atomic.Int32
	failCreate     atomic.Bool
	beforeRetrieve func()
	afterDelete    func()
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
			if f.beforeRetrieve != nil {
				f.beforeRetrieve()
			}
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
	if f.failCreate.Load() {
		json.NewEncoder(w).Encode(map[string]string{"status": "ERROR", "message": "forced create failure"})
		return
	}
	var body struct {
		Name, Type, Content, TTL, Prio string
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	id := f.nextID.Add(1)
	rec := porkbun.Record{
		ID: idStr(id), Name: body.Name + ".example.com",
		Type: body.Type, Content: body.Content, TTL: body.TTL, Prio: body.Prio,
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
	if f.afterDelete != nil {
		f.afterDelete()
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "SUCCESS"})
}

func (f *fakePorkbun) handleEdit(w http.ResponseWriter, r *http.Request, id string) {
	var body struct{ Name, Type, Content, TTL, Prio string }
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
		rec.Prio = body.Prio
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

func TestRecordsSkipsOnlyUnsupportedTypes(t *testing.T) {
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
	if len(eps) != 2 {
		t.Errorf("expected A and NS endpoints, got %d", len(eps))
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

func TestApplyCreateConvergesFreshDriftWithoutDuplicates(t *testing.T) {
	t.Run("TTL and duplicates", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(
			porkbun.Record{ID: "stale-a", Name: "replay.example.com", Type: "A", Content: "192.0.2.10", TTL: "1200"},
			porkbun.Record{ID: "stale-b", Name: "replay.example.com", Type: "A", Content: "192.0.2.10", TTL: "900"},
		)
		prov := newTestProvider(t, fake)
		err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
			DNSName: "replay.example.com", RecordType: "A", Targets: []string{"192.0.2.10"}, RecordTTL: 600,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(fake.records) != 1 {
			t.Fatalf("stale Create produced duplicates: %+v", fake.records)
		}
		for _, record := range fake.records {
			if record.TTL != "600" {
				t.Fatalf("stale TTL was not repaired: %+v", record)
			}
		}
	})

	t.Run("CNAME and ALIAS representation", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{ID: "alias", Name: "replay.example.com", Type: "ALIAS", Content: "target.example.net", TTL: "600"})
		prov := newTestProvider(t, fake)
		err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
			DNSName: "replay.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(fake.records) != 1 || fake.records["alias"].Type != "CNAME" {
			t.Fatalf("stale ALIAS representation did not converge in place: %+v", fake.records)
		}
	})
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
	eps := []*endpoint.Endpoint{{DNSName: "foo.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 60}}
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
	if got := normaliseContent("TXT", "  padded  "); got != "  padded  " {
		t.Errorf("unquoted whitespace changed to %q", got)
	}
	if got := normaliseContent("TXT", "\"  padded  \""); got != "  padded  " {
		t.Errorf("quoted whitespace changed to %q", got)
	}
	if got := serialiseContent("TXT", "  padded  "); got != "  padded  " {
		t.Errorf("serialized whitespace changed to %q", got)
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
			{DNSName: "x.example.com", RecordType: "A", Targets: []string{"1.1.1.1", "2.2.2.2"}, RecordTTL: 600},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 0 {
		t.Errorf("dry-run created records: %+v", fake.records)
	}
}

func TestNewCanonicalizesZoneAndRequiresEveryFilterWithinIt(t *testing.T) {
	filter := endpoint.NewDomainFilter([]string{"Sub.Example.COM."})
	prov, err := New(Config{
		APIKey: "k", SecretAPIKey: "s", Domain: " Example.COM. ", DomainFilter: filter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prov.domain != "example.com" {
		t.Fatalf("canonical domain = %q", prov.domain)
	}
	if !prov.inFilter("WWW.SUB.EXAMPLE.COM.") || prov.inFilter("not-sub.example.com") {
		t.Fatalf("canonical filter did not retain label-boundary matching")
	}

	for _, filters := range [][]string{
		{"example.com", "badexample.com"},
		{"example.com.evil"},
		{"evil-example.com"},
	} {
		_, err := New(Config{
			APIKey: "k", SecretAPIKey: "s", Domain: "example.com",
			DomainFilter: endpoint.NewDomainFilter(filters),
		})
		if err == nil {
			t.Errorf("expected filters %v to be rejected", filters)
		}
	}
}

func TestNewRejectsRegexAndExclusionDomainFilters(t *testing.T) {
	tests := map[string]*endpoint.DomainFilter{
		"exclusion": endpoint.NewDomainFilterWithExclusions([]string{"example.com"}, []string{"private.example.com"}),
		"regex":     endpoint.NewRegexDomainFilter(regexp.MustCompile(`(^|\.)example\.com$`), nil),
	}
	for name, filter := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := New(Config{APIKey: "k", SecretAPIKey: "s", Domain: "example.com", DomainFilter: filter})
			if err == nil {
				t.Fatalf("configured %s filter was accepted", name)
			}
		})
	}
}

func TestExplicitFilterMatchingPreservesWildcardLabels(t *testing.T) {
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	prov, err := New(Config{
		APIKey: "k", SecretAPIKey: "s", Domain: "example.com",
		DomainFilter: endpoint.NewDomainFilter([]string{"wildcard.example.com"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if prov.inFilter("*.example.com") {
		t.Fatal("literal wildcard label collided with the filter's 'wildcard' label")
	}
	if !prov.inFilter("wildcard.example.com") {
		t.Fatal("exact wildcard.example.com filter did not match")
	}
	if strings.Contains(logs.String(), "error while parsing domain") {
		t.Fatalf("direct filter matching emitted an IDNA warning: %s", logs.String())
	}

	childrenOnly, err := New(Config{
		APIKey: "k", SecretAPIKey: "s", Domain: "example.com",
		DomainFilter: endpoint.NewDomainFilter([]string{".example.com"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if childrenOnly.inFilter("example.com") || !childrenOnly.inFilter("*.example.com") {
		t.Fatal("leading-dot children-only semantics were not preserved")
	}
}

func TestAdjustEndpointsLeavesOutOfScopeEndpointsUntouched(t *testing.T) {
	prov, err := New(Config{
		APIKey: "k", SecretAPIKey: "s", Domain: "example.com",
		DomainFilter: endpoint.NewDomainFilter([]string{"managed.example.com"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	eps := []*endpoint.Endpoint{
		{
			DNSName: "OUTSIDE.EXAMPLE.COM.", RecordType: "BOGUS", Targets: []string{"not valid"}, RecordTTL: -1,
			ProviderSpecific: endpoint.ProviderSpecific{{Name: "unsupported", Value: "anything"}},
		},
		{DNSName: "bad name.other.net", RecordType: "ALIAS"},
	}
	want := make([]endpoint.Endpoint, len(eps))
	for i, ep := range eps {
		want[i] = *ep
		want[i].Targets = append(endpoint.Targets(nil), ep.Targets...)
		want[i].ProviderSpecific = append(endpoint.ProviderSpecific(nil), ep.ProviderSpecific...)
	}

	out, err := prov.AdjustEndpoints(eps)
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if !reflect.DeepEqual(*out[i], want[i]) {
			t.Fatalf("out-of-scope endpoint %d changed: got %+v, want %+v", i, *out[i], want[i])
		}
	}
}

func TestAdjustEndpointsCanonicalizesBeforeV021Planner(t *testing.T) {
	tests := []struct {
		name        string
		record      porkbun.Record
		desired     *endpoint.Endpoint
		managedType string
	}{
		{
			name:        "CNAME case and trailing dot",
			record:      porkbun.Record{Name: "cname.example.com", Type: "CNAME", Content: "target.example.net", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "CNAME.EXAMPLE.COM.", RecordType: "cname", Targets: []string{"TARGET.EXAMPLE.NET.", "target.example.net"}},
			managedType: "CNAME",
		},
		{
			name:        "AAAA expanded form",
			record:      porkbun.Record{Name: "v6.example.com", Type: "AAAA", Content: "2001:db8::1", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "V6.EXAMPLE.COM.", RecordType: "aaaa", Targets: []string{"2001:0DB8:0000:0000:0000:0000:0000:0001"}},
			managedType: "AAAA",
		},
		{
			name:        "MX leading zero and dot",
			record:      porkbun.Record{Name: "example.com", Type: "MX", Content: "mail.example.net", Prio: "10", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "EXAMPLE.COM.", RecordType: "mx", Targets: []string{"010 MAIL.EXAMPLE.NET."}},
			managedType: "MX",
		},
		{
			name:        "SRV leading zeros and dot",
			record:      porkbun.Record{Name: "_sip._tcp.example.com", Type: "SRV", Content: "5 5060 sip.example.net", Prio: "10", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "_SIP._TCP.EXAMPLE.COM.", RecordType: "srv", Targets: []string{"010 005 05060 SIP.EXAMPLE.NET"}},
			managedType: "SRV",
		},
		{
			name:        "quoted TXT",
			record:      porkbun.Record{Name: "txt.example.com", Type: "TXT", Content: "hello world", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "TXT.EXAMPLE.COM.", RecordType: "txt", Targets: []string{"\"hello world\""}},
			managedType: "TXT",
		},
		{
			name:        "unquoted TXT whitespace",
			record:      porkbun.Record{Name: "space.example.com", Type: "TXT", Content: "  padded  ", TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "SPACE.EXAMPLE.COM.", RecordType: "txt", Targets: []string{"  padded  "}},
			managedType: "TXT",
		},
		{
			name:        "CAA quoted repeated whitespace",
			record:      porkbun.Record{Name: "caa.example.com", Type: "CAA", Content: `0 ISSUE "ca.example; account=alpha  beta"`, TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "CAA.EXAMPLE.COM.", RecordType: "caa", Targets: []string{`0 issue "ca.example;\032account=alpha\032\032beta"`}},
			managedType: "CAA",
		},
		{
			name:        "SVCB escaped repeated whitespace",
			record:      porkbun.Record{Name: "_svc.example.com", Type: "SVCB", Content: `1 . key65400="alpha  beta" port=443`, TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "_SVC.EXAMPLE.COM.", RecordType: "svcb", Targets: []string{`1 . key65400=alpha\032\032beta port=0443`}},
			managedType: "SVCB",
		},
		{
			name:        "HTTPS typed params and ordering",
			record:      porkbun.Record{Name: "https.example.com", Type: "HTTPS", Content: `1 Svc.Example.NET. alpn=h2,h3 ipv4hint=192.0.2.1`, TTL: "600"},
			desired:     &endpoint.Endpoint{DNSName: "HTTPS.EXAMPLE.COM.", RecordType: "https", Targets: []string{`1 SVC.EXAMPLE.NET ipv4hint="192.0.2.1" alpn="h2,h3"`}},
			managedType: "HTTPS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakePorkbun()
			fake.seed(tt.record)
			prov := newTestProvider(t, fake)
			current, err := prov.Records(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			desired, err := prov.AdjustEndpoints([]*endpoint.Endpoint{tt.desired})
			if err != nil {
				t.Fatal(err)
			}
			if len(desired[0].Targets) != 1 {
				t.Fatalf("canonical target deduplication failed: %v", desired[0].Targets)
			}
			changes := (&plan.Plan{
				Current: current, Desired: desired,
				Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{tt.managedType},
			}).Calculate().Changes
			if countPlanChanges(changes) != 0 {
				t.Fatalf("canonical equivalents produced a v0.21 plan: %+v", changes)
			}
		})
	}
}

func TestServiceBindingCanonicalizationIsStrictAndLossless(t *testing.T) {
	valid := map[string]string{
		`1 . key65400="alpha  beta"`:                      `1 . key65400="alpha\ \ beta"`,
		`1 . key65400=a\032b`:                             `1 . key65400="a\ b"`,
		`1 . ipv6hint=2001:0DB8:0:0::1 port=0443 alpn=h2`: `1 . alpn="h2" port="443" ipv6hint="2001:db8::1"`,
		`1 . no-default-alpn alpn=h2`:                     `1 . alpn="h2" no-default-alpn=""`,
		`1 .`:                                             `1 .`,
		`0 .`:                                             `0 .`,
		`0 Svc.Example.NET. key65400="alpha beta"`: `0 svc.example.net key65400="alpha\ beta"`,
	}
	for _, recType := range []string{"SVCB", "HTTPS"} {
		for input, want := range valid {
			t.Run(recType+" valid "+input, func(t *testing.T) {
				got, err := canonicalEndpointTarget(recType, input)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("canonical target = %q, want %q", got, want)
				}
				again, err := canonicalEndpointTarget(recType, got)
				if err != nil || again != got {
					t.Fatalf("canonical target was not idempotent: %q, %v", again, err)
				}
				encoded, err := (&Provider{domain: "example.com"}).recordInput(&endpoint.Endpoint{
					DNSName: "record.example.com", RecordType: recType, RecordTTL: 600,
				}, input)
				if err != nil || encoded.Content != want {
					t.Fatalf("Porkbun content = %q, %v; want %q", encoded.Content, err, want)
				}
			})
		}
	}

	invalid := []string{
		`1 . key65400="alpha`,
		`1 . port=65536`,
		`1 . ipv4hint=2001:db8::1`,
		`1 . ipv6hint=192.0.2.1`,
		`1 . port=443 port=8443`,
		`1 . key65535=x`,
		`1 . no-default-alpn`,
		`1 . mandatory=ipv4hint`,
		`1 . alpn=`,
	}
	for _, recType := range []string{"SVCB", "HTTPS"} {
		for _, input := range invalid {
			t.Run(recType+" invalid "+input, func(t *testing.T) {
				if got, err := canonicalEndpointTarget(recType, input); err == nil {
					t.Fatalf("malformed target accepted as %q", got)
				}
			})
		}
	}
}

func TestCAACanonicalizationIsStrictAndLossless(t *testing.T) {
	input := `000 ISSUE "ca.example; account=alpha  beta"`
	want := `0 issue "ca.example; account=alpha  beta"`
	got, err := canonicalEndpointTarget("CAA", input)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical CAA = %q, want %q", got, want)
	}
	again, err := canonicalEndpointTarget("CAA", got)
	if err != nil || again != got {
		t.Fatalf("canonical CAA was not idempotent: %q, %v", again, err)
	}
	encoded, err := (&Provider{domain: "example.com"}).recordInput(&endpoint.Endpoint{
		DNSName: "record.example.com", RecordType: "CAA", RecordTTL: 600,
	}, input)
	if err != nil || encoded.Content != want {
		t.Fatalf("Porkbun content = %q, %v; want %q", encoded.Content, err, want)
	}

	for _, invalid := range []string{
		`0 issue "unterminated`,
		`0 issue-wild "ca.example"`,
		`256 issue "ca.example"`,
	} {
		t.Run(invalid, func(t *testing.T) {
			if got, err := canonicalEndpointTarget("CAA", invalid); err == nil {
				t.Fatalf("malformed CAA accepted as %q", got)
			}
		})
	}
}

func TestStructuredRecordValidationPreventsPartialWrites(t *testing.T) {
	cases := []struct {
		recType string
		target  string
	}{
		{recType: "SVCB", target: `1 . port=443 port=8443`},
		{recType: "HTTPS", target: `1 . ipv4hint=2001:db8::1`},
		{recType: "CAA", target: `0 issue "unterminated`},
	}
	for _, tt := range cases {
		t.Run(tt.recType, func(t *testing.T) {
			fake := newFakePorkbun()
			prov := newTestProvider(t, fake)
			err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
				{DNSName: "valid.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}},
				{DNSName: "invalid.example.com", RecordType: tt.recType, Targets: []string{tt.target}},
			}})
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %T: %v", err, err)
			}
			if calls := fake.calls.Load(); calls != 0 {
				t.Fatalf("invalid batch made %d API calls", calls)
			}
		})
	}
}

func TestAdjustEndpointsNormalizesAndValidatesProviderProperties(t *testing.T) {
	prov, err := New(Config{APIKey: "k", SecretAPIKey: "s", Domain: "example.com"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("alias false becomes absence", func(t *testing.T) {
		ep := &endpoint.Endpoint{
			DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"},
			ProviderSpecific: endpoint.ProviderSpecific{{Name: "alias", Value: " FALSE "}},
		}
		out, err := prov.AdjustEndpoints([]*endpoint.Endpoint{ep})
		if err != nil {
			t.Fatal(err)
		}
		if len(out[0].ProviderSpecific) != 0 {
			t.Fatalf("alias=false was retained: %+v", out[0].ProviderSpecific)
		}
	})

	t.Run("alias true converges with ALIAS view", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{Name: "alias.example.com", Type: "ALIAS", Content: "target.example.net", TTL: "600"})
		aliasProvider := newTestProvider(t, fake)
		current, err := aliasProvider.Records(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		desired, err := aliasProvider.AdjustEndpoints([]*endpoint.Endpoint{{
			DNSName: "ALIAS.EXAMPLE.COM.", RecordType: "cname", Targets: []string{"TARGET.EXAMPLE.NET."},
			ProviderSpecific: endpoint.ProviderSpecific{{Name: "alias", Value: "TRUE"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		changes := (&plan.Plan{
			Current: current, Desired: desired, Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"CNAME"},
		}).Calculate().Changes
		if countPlanChanges(changes) != 0 {
			t.Fatalf("canonical alias property produced a plan: %+v", changes)
		}
	})

	invalid := map[string]endpoint.ProviderSpecific{
		"duplicate alias": {{Name: "alias", Value: "true"}, {Name: "alias", Value: "true"}},
		"invalid alias":   {{Name: "alias", Value: "sometimes"}},
		"unsupported":     {{Name: "evaluate-target-health", Value: "true"}},
		"internal marker": {{Name: providerSpecificTTLDrift, Value: "true"}},
	}
	for name, properties := range invalid {
		t.Run(name, func(t *testing.T) {
			ep := &endpoint.Endpoint{
				DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, ProviderSpecific: properties,
			}
			_, err := prov.AdjustEndpoints([]*endpoint.Endpoint{ep})
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %T: %v", err, err)
			}
		})
	}
}

func TestApplyChangesPrevalidatesEntireBatchWithoutAPICalls(t *testing.T) {
	valid := &endpoint.Endpoint{
		DNSName: "valid.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 600,
	}
	cases := map[string]*plan.Changes{
		"nil endpoint": {
			Create: []*endpoint.Endpoint{nil},
		},
		"valid then outside zone": {
			Create: []*endpoint.Endpoint{valid, {DNSName: "example.com.evil", RecordType: "A", Targets: []string{"192.0.2.2"}}},
		},
		"label suffix is not descendant": {
			Delete: []*endpoint.Endpoint{{DNSName: "badexample.com", RecordType: "A", Targets: []string{"192.0.2.2"}}},
		},
		"unsupported type": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "PTR", Targets: []string{"example.com"}}},
		},
		"direct alias": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "ALIAS", Targets: []string{"target.example.net"}}},
		},
		"invalid target": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"not-an-ip"}}},
		},
		"invalid MX": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "MX", Targets: []string{"mail.example.com"}}},
		},
		"no targets": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "TXT"}},
		},
		"negative TTL": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: -1}},
		},
		"overflow TTL": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: endpoint.TTL(maxDNSTTL + 1)}},
		},
		"set identifier": {
			Create: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, SetIdentifier: "weighted"}},
		},
		"duplicate alias property": {
			Create: []*endpoint.Endpoint{{
				DNSName: "x.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"},
				ProviderSpecific: endpoint.ProviderSpecific{{Name: "alias", Value: "true"}, {Name: "alias", Value: "true"}},
			}},
		},
		"invalid alias property": {
			Create: []*endpoint.Endpoint{{
				DNSName: "x.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"},
				ProviderSpecific: endpoint.ProviderSpecific{{Name: "alias", Value: "sometimes"}},
			}},
		},
		"unsupported provider property": {
			Create: []*endpoint.Endpoint{{
				DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"},
				ProviderSpecific: endpoint.ProviderSpecific{{Name: "unsupported", Value: "true"}},
			}},
		},
		"forged internal property": {
			Create: []*endpoint.Endpoint{{
				DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"},
				ProviderSpecific: endpoint.ProviderSpecific{{Name: providerSpecificTTLDrift, Value: "true"}},
			}},
		},
		"update length mismatch": {
			UpdateOld: []*endpoint.Endpoint{valid},
		},
		"update identity mismatch": {
			UpdateOld: []*endpoint.Endpoint{valid},
			UpdateNew: []*endpoint.Endpoint{{DNSName: "other.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}},
		},
	}

	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakePorkbun()
			prov := newTestProvider(t, fake)
			err := prov.ApplyChanges(context.Background(), changes)
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %T: %v", err, err)
			}
			if calls := fake.calls.Load(); calls != 0 {
				t.Fatalf("invalid batch made %d API calls", calls)
			}
		})
	}

	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)
	if err := prov.ApplyChanges(context.Background(), nil); err == nil {
		t.Fatal("nil changes unexpectedly accepted")
	}
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("nil batch made %d API calls", calls)
	}
}

func TestApplyChangesEnforcesConfiguredFilterWithoutAPICalls(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)
	prov.domainFilter = endpoint.NewDomainFilter([]string{"managed.example.com"})
	prov.include = []string{"managed.example.com"}
	err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
		DNSName: "other.example.com", RecordType: "A", Targets: []string{"192.0.2.1"},
	}}})
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("out-of-filter batch made %d API calls", calls)
	}
}

func TestTTLZeroUsesStableEffectiveDefault(t *testing.T) {
	t.Run("adjust", func(t *testing.T) {
		prov, _ := New(Config{APIKey: "k", SecretAPIKey: "s", Domain: "example.com"})
		eps, err := prov.AdjustEndpoints([]*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 0}})
		if err != nil || eps[0].RecordTTL != 600 {
			t.Fatalf("adjusted TTL = %d, err=%v", eps[0].RecordTTL, err)
		}
	})

	t.Run("records", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{Name: "x.example.com", Type: "A", Content: "192.0.2.1", TTL: "0"})
		prov := newTestProvider(t, fake)
		eps, err := prov.Records(context.Background())
		if err != nil || len(eps) != 1 || eps[0].RecordTTL != 600 {
			t.Fatalf("Records() = %+v, err=%v", eps, err)
		}
	})

	t.Run("create", func(t *testing.T) {
		fake := newFakePorkbun()
		prov := newTestProvider(t, fake)
		err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
			DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 0,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range fake.records {
			if r.TTL != "600" {
				t.Fatalf("created TTL = %q", r.TTL)
			}
		}
	})

	t.Run("update does not loop", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{ID: "existing", Name: "x.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"})
		prov := newTestProvider(t, fake)
		err := prov.ApplyChanges(context.Background(), &plan.Changes{
			UpdateOld: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 600}},
			UpdateNew: []*endpoint.Endpoint{{DNSName: "x.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 0}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls := fake.calls.Load(); calls != 1 {
			t.Fatalf("expected retrieve only, got %d API calls", calls)
		}
	})
}

func TestWildcardRecordsMatchFilterWithoutIDNAWarnings(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(porkbun.Record{Name: "*.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"})
	prov := newTestProvider(t, fake)

	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].DNSName != "*.example.com" {
		t.Fatalf("wildcard endpoint missing: %+v", eps)
	}
	if strings.Contains(logs.String(), "error while parsing domain") {
		t.Fatalf("wildcard filter emitted IDNA warning: %s", logs.String())
	}
}

func TestPorkbunRecordCodecsRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		record      porkbun.Record
		viewType    string
		target      string
		content     string
		prio        string
		writtenType string
	}{
		{name: "A", record: porkbun.Record{Type: "A", Content: "192.0.2.1"}, viewType: "A", target: "192.0.2.1", content: "192.0.2.1", writtenType: "A"},
		{name: "AAAA", record: porkbun.Record{Type: "AAAA", Content: "2001:0DB8:0:0::1"}, viewType: "AAAA", target: "2001:db8::1", content: "2001:db8::1", writtenType: "AAAA"},
		{name: "CNAME", record: porkbun.Record{Type: "CNAME", Content: "Target.Example.NET."}, viewType: "CNAME", target: "target.example.net", content: "target.example.net", writtenType: "CNAME"},
		{name: "ALIAS", record: porkbun.Record{Type: "ALIAS", Content: "Target.Example.NET."}, viewType: "CNAME", target: "target.example.net", content: "target.example.net", writtenType: "ALIAS"},
		{name: "TXT", record: porkbun.Record{Type: "TXT", Content: "\"hello world\""}, viewType: "TXT", target: "hello world", content: "hello world", writtenType: "TXT"},
		{name: "NS", record: porkbun.Record{Type: "NS", Content: "NS1.Example.NET."}, viewType: "NS", target: "ns1.example.net", content: "ns1.example.net", writtenType: "NS"},
		{name: "MX", record: porkbun.Record{Type: "MX", Content: "Mail.Example.NET.", Prio: "010"}, viewType: "MX", target: "10 mail.example.net", content: "mail.example.net", prio: "10", writtenType: "MX"},
		{name: "SRV", record: porkbun.Record{Type: "SRV", Content: "005 0443 Srv.Example.NET.", Prio: "010"}, viewType: "SRV", target: "10 5 443 srv.example.net.", content: "5 443 srv.example.net", prio: "10", writtenType: "SRV"},
		{name: "TLSA", record: porkbun.Record{Type: "TLSA", Content: "3 1 1 AABB"}, viewType: "TLSA", target: "3 1 1 aabb", content: "3 1 1 aabb", writtenType: "TLSA"},
		{name: "CAA", record: porkbun.Record{Type: "CAA", Content: "0 ISSUE \"letsencrypt.org\""}, viewType: "CAA", target: "0 issue \"letsencrypt.org\"", content: "0 issue \"letsencrypt.org\"", writtenType: "CAA"},
		{name: "SSHFP", record: porkbun.Record{Type: "SSHFP", Content: "4 2 AABB"}, viewType: "SSHFP", target: "4 2 aabb", content: "4 2 aabb", writtenType: "SSHFP"},
		{name: "HTTPS", record: porkbun.Record{Type: "HTTPS", Content: "1 . alpn=h2"}, viewType: "HTTPS", target: `1 . alpn="h2"`, content: `1 . alpn="h2"`, writtenType: "HTTPS"},
		{name: "HTTPS quoted params", record: porkbun.Record{Type: "HTTPS", Content: `1 . alpn="h2,h3" dohpath="/dns-query{?dns}"`}, viewType: "HTTPS", target: `1 . alpn="h2,h3" dohpath="/dns-query{?dns}"`, content: `1 . alpn="h2,h3" dohpath="/dns-query{?dns}"`, writtenType: "HTTPS"},
		{name: "SVCB", record: porkbun.Record{Type: "SVCB", Content: "1 Svc.Example.NET. port=443"}, viewType: "SVCB", target: `1 svc.example.net port="443"`, content: `1 svc.example.net port="443"`, writtenType: "SVCB"},
		{name: "SVCB root escaped params", record: porkbun.Record{Type: "SVCB", Content: `2 . key65400="alpha beta" key65401=a\032b`}, viewType: "SVCB", target: `2 . key65400="alpha\ beta" key65401="a\ b"`, content: `2 . key65400="alpha\ beta" key65401="a\ b"`, writtenType: "SVCB"},
	}

	prov := &Provider{domain: "example.com"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.record.Name = "record.example.com"
			tt.record.TTL = "600"
			viewType, target, err := recordToEndpointTarget(tt.record)
			if err != nil {
				t.Fatal(err)
			}
			if viewType != tt.viewType || target != tt.target {
				t.Fatalf("decoded (%q, %q), want (%q, %q)", viewType, target, tt.viewType, tt.target)
			}
			ep := &endpoint.Endpoint{
				DNSName: "record.example.com", RecordType: viewType, Targets: []string{target}, RecordTTL: 600,
			}
			if tt.record.Type == "ALIAS" {
				ep.SetProviderSpecificProperty("alias", "true")
			}
			input, err := prov.recordInput(ep, target)
			if err != nil {
				t.Fatal(err)
			}
			if input.Type != tt.writtenType || input.Content != tt.content || input.Prio != tt.prio {
				t.Fatalf("encoded type=%q content=%q prio=%q", input.Type, input.Content, input.Prio)
			}
			encoded := porkbun.Record{Type: input.Type, Content: input.Content, Prio: input.Prio, TTL: input.TTL}
			encodedView, encodedTarget, err := recordToEndpointTarget(encoded)
			if err != nil || encodedView != viewType || encodedTarget != target {
				t.Fatalf("round trip = (%q, %q, %v)", encodedView, encodedTarget, err)
			}
		})
	}
}

func TestMXPriorityParticipatesInRecordIdentity(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "mx10", Name: "example.com", Type: "MX", Content: "mail.example.net", Prio: "10", TTL: "600"},
		porkbun.Record{ID: "mx20", Name: "example.com", Type: "MX", Content: "mail.example.net", Prio: "20", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || !reflect.DeepEqual([]string(eps[0].Targets), []string{"10 mail.example.net", "20 mail.example.net"}) {
		t.Fatalf("MX targets = %+v", eps)
	}

	err = prov.ApplyChanges(context.Background(), &plan.Changes{Delete: []*endpoint.Endpoint{{
		DNSName: "example.com", RecordType: "MX", Targets: []string{"10 mail.example.net"}, RecordTTL: 600,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := fake.records["mx10"]; exists {
		t.Fatal("priority-10 MX record was not deleted")
	}
	if _, exists := fake.records["mx20"]; !exists {
		t.Fatal("priority-20 MX record was incorrectly deleted")
	}
}

func TestSRVPriorityParticipatesInRecordIdentity(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "srv10", Name: "_sip._tcp.example.com", Type: "SRV", Content: "5 5060 sip.example.net", Prio: "10", TTL: "600"},
		porkbun.Record{ID: "srv20", Name: "_sip._tcp.example.com", Type: "SRV", Content: "5 5060 sip.example.net", Prio: "20", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	err := prov.ApplyChanges(context.Background(), &plan.Changes{Delete: []*endpoint.Endpoint{{
		DNSName: "_sip._tcp.example.com", RecordType: "SRV", Targets: []string{"10 5 5060 sip.example.net."}, RecordTTL: 600,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := fake.records["srv10"]; exists {
		t.Fatal("priority-10 SRV record was not deleted")
	}
	if _, exists := fake.records["srv20"]; !exists {
		t.Fatal("priority-20 SRV record was incorrectly deleted")
	}
}

func TestAliasMappingAndConvergence(t *testing.T) {
	t.Run("records expose alias as CNAME", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{Name: "alias.example.com", Type: "ALIAS", Content: "target.example.net", TTL: "600"})
		prov := newTestProvider(t, fake)
		eps, err := prov.Records(context.Background())
		if err != nil || len(eps) != 1 {
			t.Fatalf("Records() = %+v, %v", eps, err)
		}
		alias, ok := eps[0].GetProviderSpecificProperty("alias")
		if eps[0].RecordType != "CNAME" || !ok || alias != "true" {
			t.Fatalf("ALIAS view = %+v", eps[0])
		}
	})

	t.Run("apex CNAME creates stable ALIAS", func(t *testing.T) {
		fake := newFakePorkbun()
		prov := newTestProvider(t, fake)
		err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
			DNSName: "example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range fake.records {
			if r.Type != "ALIAS" {
				t.Fatalf("apex CNAME written as %q", r.Type)
			}
		}
		eps, err := prov.Records(context.Background())
		if err != nil || len(eps) != 1 || eps[0].RecordType != "CNAME" {
			t.Fatalf("apex ALIAS did not round trip: %+v, %v", eps, err)
		}
		alias, ok := eps[0].GetProviderSpecificProperty("alias")
		if !ok || alias != "true" {
			t.Fatalf("apex ALIAS metadata missing: %+v", eps[0])
		}
		fake.calls.Store(0)
		desired := &endpoint.Endpoint{DNSName: "example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600}
		if err := prov.ApplyChanges(context.Background(), &plan.Changes{
			UpdateOld: eps, UpdateNew: []*endpoint.Endpoint{desired},
		}); err != nil {
			t.Fatal(err)
		}
		if calls := fake.calls.Load(); calls != 1 {
			t.Fatalf("stable apex ALIAS made %d calls; expected retrieve only", calls)
		}
	})

	t.Run("non-apex ALIAS converges to requested CNAME", func(t *testing.T) {
		fake := newFakePorkbun()
		fake.seed(porkbun.Record{ID: "alias", Name: "www.example.com", Type: "ALIAS", Content: "target.example.net", TTL: "600"})
		prov := newTestProvider(t, fake)
		oldEP := &endpoint.Endpoint{DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600}
		oldEP.SetProviderSpecificProperty("alias", "true")
		newEP := &endpoint.Endpoint{DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600}
		err := prov.ApplyChanges(context.Background(), &plan.Changes{
			UpdateOld: []*endpoint.Endpoint{oldEP}, UpdateNew: []*endpoint.Endpoint{newEP},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := fake.records["alias"].Type; got != "CNAME" {
			t.Fatalf("existing ALIAS converged to %q", got)
		}
		fake.calls.Store(0)
		eps, err := prov.Records(context.Background())
		if err != nil || len(eps) != 1 {
			t.Fatalf("Records() = %+v, %v", eps, err)
		}
		if _, hasAlias := eps[0].GetProviderSpecificProperty("alias"); hasAlias {
			t.Fatalf("converted CNAME still advertises alias: %+v", eps[0])
		}
		planned := (&plan.Plan{
			Current:  eps,
			Desired:  []*endpoint.Endpoint{{DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600}},
			Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"CNAME"},
		}).Calculate().Changes
		if len(planned.Create)+len(planned.UpdateNew)+len(planned.Delete) != 0 {
			t.Fatalf("converted CNAME would loop in planner: %+v", planned)
		}
	})

	t.Run("provider-specific alias creates ALIAS", func(t *testing.T) {
		fake := newFakePorkbun()
		prov := newTestProvider(t, fake)
		ep := &endpoint.Endpoint{DNSName: "www.example.com", RecordType: "CNAME", Targets: []string{"target.example.net"}, RecordTTL: 600}
		ep.SetProviderSpecificProperty("alias", "true")
		if err := prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{ep}}); err != nil {
			t.Fatal(err)
		}
		for _, r := range fake.records {
			if r.Type != "ALIAS" {
				t.Fatalf("annotated CNAME written as %q", r.Type)
			}
		}
	})
}

func TestDuplicateRecordsRemainPlannerVisibleAndAreRepaired(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "stale", Name: "dup.example.com", Type: "A", Content: "192.0.2.1", TTL: "700"},
		porkbun.Record{ID: "keeper", Name: "dup.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	current, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || len(current[0].Targets) != 2 {
		t.Fatalf("duplicates were hidden from planner: %+v", current)
	}
	desired := &endpoint.Endpoint{DNSName: "dup.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 600}
	planned := (&plan.Plan{
		Current: current, Desired: []*endpoint.Endpoint{desired},
		Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
	}).Calculate().Changes
	if len(planned.UpdateOld) != 1 || len(planned.UpdateNew) != 1 {
		t.Fatalf("planner did not surface duplicate cleanup: %+v", planned)
	}

	fake.calls.Store(0)
	if err := prov.ApplyChanges(context.Background(), planned); err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 1 {
		t.Fatalf("expected exactly one record after repair: %+v", fake.records)
	}
	if _, ok := fake.records["keeper"]; !ok {
		t.Fatalf("record with correct TTL was not retained: %+v", fake.records)
	}
	if calls := fake.calls.Load(); calls != 2 {
		t.Fatalf("expected retrieve plus one duplicate delete, got %d calls", calls)
	}
}

func TestRecordsAreDeterministic(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "3", Name: "z.example.com", Type: "A", Content: "192.0.2.2", TTL: "600"},
		porkbun.Record{ID: "1", Name: "a.example.com", Type: "TXT", Content: "z", TTL: "600"},
		porkbun.Record{ID: "2", Name: "z.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0].DNSName != "a.example.com" || eps[1].DNSName != "z.example.com" {
		t.Fatalf("endpoint order = %+v", eps)
	}
	if !reflect.DeepEqual([]string(eps[1].Targets), []string{"192.0.2.1", "192.0.2.2"}) {
		t.Fatalf("target order = %v", eps[1].Targets)
	}
}

func TestApplyChangesSerializesConcurrentCreateReplay(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)

	retrieveStarted := make(chan struct{}, 2)
	releaseRetrieve := make(chan struct{})
	fake.beforeRetrieve = func() {
		retrieveStarted <- struct{}{}
		<-releaseRetrieve
	}

	makeChanges := func() *plan.Changes {
		return &plan.Changes{Create: []*endpoint.Endpoint{{
			DNSName: "replay.example.com", RecordType: "A", Targets: []string{"192.0.2.10"}, RecordTTL: 600,
		}}}
	}
	errs := make(chan error, 2)
	go func() { errs <- prov.ApplyChanges(context.Background(), makeChanges()) }()
	<-retrieveStarted
	go func() { errs <- prov.ApplyChanges(context.Background(), makeChanges()) }()

	select {
	case <-retrieveStarted:
		close(releaseRetrieve)
		t.Fatal("second ApplyChanges reached Retrieve before the first apply completed")
	case <-time.After(75 * time.Millisecond):
	}
	close(releaseRetrieve)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 1 {
		t.Fatalf("concurrent replay created %d records: %+v", len(fake.records), fake.records)
	}
	if calls := fake.calls.Load(); calls != 3 {
		t.Fatalf("expected retrieve/create/retrieve, got %d API calls", calls)
	}
}

func TestApplyFailureInvalidatesCachePopulatedMidApply(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(porkbun.Record{ID: "old", Name: "cache.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"})
	prov := newTestProvider(t, fake)
	prov.cache = newRecordCache(time.Hour)
	fake.failCreate.Store(true)

	midApplyRecords := make(chan error, 1)
	fake.afterDelete = func() {
		_, err := prov.Records(context.Background())
		midApplyRecords <- err
	}
	err := prov.ApplyChanges(context.Background(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{{DNSName: "cache.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 600}},
		UpdateNew: []*endpoint.Endpoint{{DNSName: "cache.example.com", RecordType: "A", Targets: []string{"192.0.2.2"}, RecordTTL: 600}},
	})
	if err == nil {
		t.Fatal("forced create failure unexpectedly succeeded")
	}
	if err := <-midApplyRecords; err != nil {
		t.Fatalf("mid-apply Records failed: %v", err)
	}

	// The mid-apply read cached the temporary empty state. A deferred
	// invalidation must discard it even though the apply returned an error.
	fake.seed(porkbun.Record{Name: "visible.example.com", Type: "A", Content: "192.0.2.99", TTL: "600"})
	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].DNSName != "visible.example.com" {
		t.Fatalf("failure path reused a mid-apply cache snapshot: %+v", eps)
	}
}

func TestOutOfScopeMalformedRecordsDoNotBlockRecordsOrApply(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "malformed", Name: "bad name.outside.example.com", Type: "A", Content: "not-an-ip", TTL: "600"},
		porkbun.Record{ID: "valid", Name: "one.managed.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"},
	)
	prov := newTestProvider(t, fake)
	prov.domainFilter = endpoint.NewDomainFilter([]string{"managed.example.com"})
	prov.include = []string{"managed.example.com"}

	eps, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].DNSName != "one.managed.example.com" {
		t.Fatalf("out-of-scope malformed record leaked or blocked Records: %+v", eps)
	}
	err = prov.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{{
		DNSName: "two.managed.example.com", RecordType: "A", Targets: []string{"192.0.2.2"}, RecordTTL: 600,
	}}})
	if err != nil {
		t.Fatalf("out-of-scope malformed record blocked ApplyChanges: %v", err)
	}
	if len(fake.records) != 3 {
		t.Fatalf("in-scope create missing: %+v", fake.records)
	}

	t.Run("root filter still rejects malformed in-scope data", func(t *testing.T) {
		rootFake := newFakePorkbun()
		rootFake.seed(porkbun.Record{Name: "bad name.example.com", Type: "A", Content: "not-an-ip", TTL: "600"})
		rootProvider := newTestProvider(t, rootFake)
		if _, err := rootProvider.Records(context.Background()); err == nil {
			t.Fatal("in-scope malformed record was silently skipped")
		}
	})
}

func TestMixedTTLRRSetPlansAndConvergesAllTargets(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{ID: "low", Name: "mixed.example.com", Type: "A", Content: "192.0.2.1", TTL: "600"},
		porkbun.Record{ID: "high", Name: "mixed.example.com", Type: "A", Content: "192.0.2.2", TTL: "1200"},
	)
	prov := newTestProvider(t, fake)
	current, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("current endpoints = %+v", current)
	}
	if marker, ok := current[0].GetProviderSpecificProperty(providerSpecificTTLDrift); !ok || marker != "true" {
		t.Fatalf("mixed TTL marker missing: %+v", current[0])
	}
	desired := &endpoint.Endpoint{
		DNSName: "mixed.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, RecordTTL: 600,
	}
	firstPlan := (&plan.Plan{
		Current: current, Desired: []*endpoint.Endpoint{desired},
		Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
	}).Calculate().Changes
	if len(firstPlan.UpdateOld) != 1 || len(firstPlan.UpdateNew) != 1 {
		t.Fatalf("v0.21 planner did not surface mixed TTL drift: %+v", firstPlan)
	}
	if err := prov.ApplyChanges(context.Background(), firstPlan); err != nil {
		t.Fatal(err)
	}
	for id, record := range fake.records {
		if record.TTL != "600" {
			t.Fatalf("record %s retained TTL %q", id, record.TTL)
		}
	}

	converged, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondPlan := (&plan.Plan{
		Current: converged, Desired: []*endpoint.Endpoint{desired},
		Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
	}).Calculate().Changes
	if countPlanChanges(secondPlan) != 0 {
		t.Fatalf("second v0.21 plan was not empty: %+v", secondPlan)
	}
}

func TestRawSubMinimumTTLPlansAndConverges(t *testing.T) {
	for _, rawTTL := range []string{"0", "300"} {
		t.Run("raw TTL "+rawTTL, func(t *testing.T) {
			fake := newFakePorkbun()
			fake.seed(porkbun.Record{ID: "legacy", Name: "legacy.example.com", Type: "A", Content: "192.0.2.50", TTL: rawTTL})
			prov := newTestProvider(t, fake)
			current, err := prov.Records(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if marker, ok := current[0].GetProviderSpecificProperty(providerSpecificTTLDrift); !ok || marker != "true" {
				t.Fatalf("raw TTL %s was hidden: %+v", rawTTL, current[0])
			}
			desired, err := prov.AdjustEndpoints([]*endpoint.Endpoint{{
				DNSName: "legacy.example.com", RecordType: "A", Targets: []string{"192.0.2.50"}, RecordTTL: 0,
			}})
			if err != nil {
				t.Fatal(err)
			}
			firstPlan := (&plan.Plan{
				Current: current, Desired: desired,
				Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
			}).Calculate().Changes
			if len(firstPlan.UpdateOld) != 1 || len(firstPlan.UpdateNew) != 1 {
				t.Fatalf("v0.21 planner missed raw TTL %s: %+v", rawTTL, firstPlan)
			}
			if err := prov.ApplyChanges(context.Background(), firstPlan); err != nil {
				t.Fatal(err)
			}
			if got := fake.records["legacy"].TTL; got != "600" {
				t.Fatalf("raw TTL %s converged to %q", rawTTL, got)
			}

			converged, err := prov.Records(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			secondPlan := (&plan.Plan{
				Current: converged, Desired: desired,
				Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
			}).Calculate().Changes
			if countPlanChanges(secondPlan) != 0 {
				t.Fatalf("raw TTL %s did not converge: %+v", rawTTL, secondPlan)
			}
		})
	}
}

func TestWhitespacePaddedPorkbunTTLDoesNotCreateFalseDrift(t *testing.T) {
	fake := newFakePorkbun()
	fake.seed(porkbun.Record{
		ID: "padded", Name: "ttl.example.com", Type: "A", Content: "192.0.2.60", TTL: " 600 ",
	})
	prov := newTestProvider(t, fake)
	current, err := prov.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].RecordTTL != 600 {
		t.Fatalf("Records() = %+v", current)
	}
	if marker, exists := current[0].GetProviderSpecificProperty(providerSpecificTTLDrift); exists {
		t.Fatalf("padded valid TTL was marked as drift (%q): %+v", marker, current[0])
	}
	desired := []*endpoint.Endpoint{{
		DNSName: "ttl.example.com", RecordType: "A", Targets: []string{"192.0.2.60"}, RecordTTL: 600,
	}}
	changes := (&plan.Plan{
		Current: current, Desired: desired,
		Policies: []plan.Policy{&plan.SyncPolicy{}}, ManagedRecords: []string{"A"},
	}).Calculate().Changes
	if countPlanChanges(changes) != 0 {
		t.Fatalf("padded valid TTL created a v0.21 plan: %+v", changes)
	}
}

func countPlanChanges(changes *plan.Changes) int {
	if changes == nil {
		return 0
	}
	return len(changes.Create) + len(changes.UpdateOld) + len(changes.UpdateNew) + len(changes.Delete)
}
