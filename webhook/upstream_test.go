package webhook

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	externalprovider "sigs.k8s.io/external-dns/provider"
	upstreamwebhook "sigs.k8s.io/external-dns/provider/webhook"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
	providerimpl "github.com/mattgmoser/external-dns-porkbun-webhook/provider"
)

// This exercises our handlers through the client shipped by the exact
// ExternalDNS version in go.mod. It catches wire-protocol drift that isolated
// handler tests cannot, including negotiation headers and response codes.
func TestExternalDNSClientCompatibility(t *testing.T) {
	p := &fakeProvider{records: []*endpoint.Endpoint{{
		DNSName:    "app.example.com",
		RecordType: endpoint.RecordTypeA,
		Targets:    endpoint.Targets{"192.0.2.1"},
		RecordTTL:  endpoint.TTL(600),
	}}}
	s := newHandlerServer(p)
	httpServer := httptest.NewServer(s.apiServer.Handler)
	t.Cleanup(httpServer.Close)

	cfg := &externaldns.Config{
		WebhookProviderURL:          httpServer.URL,
		WebhookProviderReadTimeout:  2 * time.Second,
		WebhookProviderWriteTimeout: 2 * time.Second,
	}
	client, err := upstreamwebhook.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("negotiate with ExternalDNS client: %v", err)
	}
	if !client.GetDomainFilter().Match("app.example.com") {
		t.Fatal("negotiated domain filter does not match app.example.com")
	}

	records, err := client.Records(context.Background())
	if err != nil {
		t.Fatalf("ExternalDNS Records: %v", err)
	}
	if len(records) != 1 || records[0].DNSName != "app.example.com" {
		t.Fatalf("ExternalDNS Records = %#v", records)
	}

	changes := &plan.Changes{Create: []*endpoint.Endpoint{{
		DNSName:    "new.example.com",
		RecordType: endpoint.RecordTypeA,
		Targets:    endpoint.Targets{"192.0.2.2"},
		RecordTTL:  endpoint.TTL(600),
	}}}
	if err := client.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatalf("ExternalDNS ApplyChanges: %v", err)
	}
	p.mu.Lock()
	applied := p.changes
	p.mu.Unlock()
	if applied == nil || len(applied.Create) != 1 || applied.Create[0].DNSName != "new.example.com" {
		t.Fatalf("applied changes = %#v", applied)
	}

	adjusted, err := client.AdjustEndpoints([]*endpoint.Endpoint{{
		DNSName:    "new.example.com",
		RecordType: endpoint.RecordTypeA,
		Targets:    endpoint.Targets{"192.0.2.2"},
		RecordTTL:  endpoint.TTL(60),
	}})
	if err != nil {
		t.Fatalf("ExternalDNS AdjustEndpoints: %v", err)
	}
	if len(adjusted) != 1 || adjusted[0].RecordTTL != endpoint.TTL(600) {
		t.Fatalf("adjusted endpoints = %#v", adjusted)
	}
}

func TestExternalDNSClientErrorClassification(t *testing.T) {
	p := &fakeProvider{}
	s := newHandlerServer(p)
	httpServer := httptest.NewServer(s.apiServer.Handler)
	t.Cleanup(httpServer.Close)

	cfg := &externaldns.Config{
		WebhookProviderURL:          httpServer.URL,
		WebhookProviderReadTimeout:  2 * time.Second,
		WebhookProviderWriteTimeout: 2 * time.Second,
	}
	client, err := upstreamwebhook.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes := &plan.Changes{Create: []*endpoint.Endpoint{{
		DNSName: "new.example.com", RecordType: endpoint.RecordTypeA, Targets: endpoint.Targets{"192.0.2.2"},
	}}}

	p.applyErr = &porkbun.APIError{HTTPStatus: 429, Message: "backend detail", Retryable: true}
	err = client.ApplyChanges(context.Background(), changes)
	if !errors.Is(err, externalprovider.SoftError) {
		t.Fatalf("backend error = %v, want ExternalDNS soft error", err)
	}

	p.applyErr = &providerimpl.ValidationError{Err: errors.New("outside zone")}
	err = client.ApplyChanges(context.Background(), changes)
	if err == nil || errors.Is(err, externalprovider.SoftError) {
		t.Fatalf("validation error = %v, want hard ExternalDNS error", err)
	}
}
