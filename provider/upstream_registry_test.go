package provider

import (
	"context"
	"testing"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	txtregistry "sigs.k8s.io/external-dns/registry/txt"
)

// upstreamProviderAdapter models what the ExternalDNS webhook client exposes
// to its registry. The concrete provider returns *DomainFilter for the webhook
// server, whereas ExternalDNS's in-process Provider interface returns the
// DomainFilterInterface abstraction.
type upstreamProviderAdapter struct {
	provider *Provider
}

func (a upstreamProviderAdapter) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	return a.provider.Records(ctx)
}

func (a upstreamProviderAdapter) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	return a.provider.ApplyChanges(ctx, changes)
}

func (a upstreamProviderAdapter) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	return a.provider.AdjustEndpoints(endpoints)
}

func (a upstreamProviderAdapter) GetDomainFilter() endpoint.DomainFilterInterface {
	return a.provider.GetDomainFilter()
}

// TestExternalDNSV021TXTRegistryIntegration exercises the exact registry
// implementation bundled with ExternalDNS v0.21. It protects integration
// boundaries that provider-only tests cannot see: inherited ALIAS metadata,
// apex-safe TXT names, and valid ownership names for wildcard records.
func TestExternalDNSV021TXTRegistryIntegration(t *testing.T) {
	tests := []struct {
		name          string
		dnsName       string
		explicitAlias bool
		wantAlias     bool
		porkbunType   string
		ownershipName string
	}{
		{
			name:          "non-apex explicit alias",
			dnsName:       "www.example.com",
			explicitAlias: true,
			wantAlias:     true,
			porkbunType:   "ALIAS",
			ownershipName: "external-dns-cname.www.example.com",
		},
		{
			name:          "apex automatic alias",
			dnsName:       "example.com",
			wantAlias:     true,
			porkbunType:   "ALIAS",
			ownershipName: "external-dns-cname.example.com",
		},
		{
			name:          "wildcard CNAME",
			dnsName:       "*.example.com",
			porkbunType:   endpoint.RecordTypeCNAME,
			ownershipName: "external-dns-cname._wildcard.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakePorkbun()
			prov := newTestProvider(t, fake)
			registry, err := txtregistry.New(&externaldns.Config{
				TXTOwnerID:             "registry-integration-test",
				TXTPrefix:              "external-dns-%{record_type}.",
				TXTWildcardReplacement: "_wildcard",
				ManagedDNSRecordTypes:  []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
			}, upstreamProviderAdapter{provider: prov})
			if err != nil {
				t.Fatal(err)
			}

			desired := endpoint.NewEndpoint(tt.dnsName, endpoint.RecordTypeCNAME, "target.example.net")
			if tt.explicitAlias {
				desired.SetProviderSpecificProperty(providerSpecificAlias, "true")
			}
			adjusted, err := registry.AdjustEndpoints([]*endpoint.Endpoint{desired})
			if err != nil {
				t.Fatal(err)
			}
			alias, hasAlias := adjusted[0].GetProviderSpecificProperty(providerSpecificAlias)
			if tt.wantAlias && (!hasAlias || alias != "true") {
				t.Fatalf("adjusted endpoint did not request ALIAS: %+v", adjusted[0])
			}
			if !tt.wantAlias && hasAlias {
				t.Fatalf("ordinary CNAME unexpectedly requested ALIAS: %+v", adjusted[0])
			}

			// ExternalDNS calls Records before ApplyChanges on every reconciliation.
			if _, err := registry.Records(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := registry.ApplyChanges(context.Background(), &plan.Changes{Create: adjusted}); err != nil {
				t.Fatal(err)
			}

			fake.mu.Lock()
			if len(fake.records) != 2 {
				fake.mu.Unlock()
				t.Fatalf("created %d records, want ALIAS plus ownership TXT", len(fake.records))
			}
			var foundPrimary, foundOwnership bool
			for _, record := range fake.records {
				switch {
				case record.Name == tt.dnsName && record.Type == tt.porkbunType && record.Content == "target.example.net":
					foundPrimary = true
				case record.Name == tt.ownershipName && record.Type == endpoint.RecordTypeTXT:
					foundOwnership = true
				}
			}
			fake.mu.Unlock()
			if !foundPrimary || !foundOwnership {
				t.Fatalf("missing primary or ownership TXT: primary=%t ownership=%t", foundPrimary, foundOwnership)
			}

			current, err := registry.Records(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(current) != 1 || current[0].DNSName != tt.dnsName || current[0].RecordType != endpoint.RecordTypeCNAME {
				t.Fatalf("registry did not reconstruct the owned CNAME view: %+v", current)
			}
			alias, hasAlias = current[0].GetProviderSpecificProperty(providerSpecificAlias)
			if tt.wantAlias && (!hasAlias || alias != "true") {
				t.Fatalf("registry round trip lost ALIAS metadata: %+v", current[0])
			}
			if !tt.wantAlias && hasAlias {
				t.Fatalf("registry round trip added ALIAS metadata: %+v", current[0])
			}
			if owner := current[0].Labels[endpoint.OwnerLabelKey]; owner != "registry-integration-test" {
				t.Fatalf("registry round trip lost ownership: %q", owner)
			}

			steadyDesired := endpoint.NewEndpoint(tt.dnsName, endpoint.RecordTypeCNAME, "target.example.net")
			if tt.explicitAlias {
				steadyDesired.SetProviderSpecificProperty(providerSpecificAlias, "true")
			}
			steadyDesired, err = singleAdjustedEndpoint(registry, steadyDesired)
			if err != nil {
				t.Fatal(err)
			}
			steadyPlan := (&plan.Plan{
				Current: current, Desired: []*endpoint.Endpoint{steadyDesired},
				Policies: []plan.Policy{&plan.UpsertOnlyPolicy{}}, ManagedRecords: []string{endpoint.RecordTypeCNAME},
				OwnerID: "registry-integration-test",
			}).Calculate()
			if countPlanChanges(steadyPlan.Changes) != 0 {
				t.Fatalf("registry round trip did not converge: %+v", steadyPlan.Changes)
			}

			if err := registry.ApplyChanges(context.Background(), &plan.Changes{Delete: current}); err != nil {
				t.Fatal(err)
			}
			assertFakeRecordTypes(t, fake, map[string]int{})
		})
	}
}

type endpointAdjuster interface {
	AdjustEndpoints([]*endpoint.Endpoint) ([]*endpoint.Endpoint, error)
}

func singleAdjustedEndpoint(registry endpointAdjuster, ep *endpoint.Endpoint) (*endpoint.Endpoint, error) {
	adjusted, err := registry.AdjustEndpoints([]*endpoint.Endpoint{ep})
	if err != nil {
		return nil, err
	}
	return adjusted[0], nil
}

func TestNormalizeExternalDNSRegistryMetadata(t *testing.T) {
	t.Run("ownership TXT drops inherited alias", func(t *testing.T) {
		ep := endpoint.NewEndpoint("external-dns-cname.www.example.com", endpoint.RecordTypeTXT, "heritage=external-dns")
		ep.Labels[endpoint.OwnedRecordLabelKey] = "www.example.com"
		ep.SetProviderSpecificProperty(providerSpecificAlias, "true")
		if err := normalizeProviderSpecific(ep, true); err != nil {
			t.Fatal(err)
		}
		if len(ep.ProviderSpecific) != 0 {
			t.Fatalf("ownership TXT retained copied provider metadata: %+v", ep.ProviderSpecific)
		}
	})

	t.Run("ordinary TXT still rejects alias", func(t *testing.T) {
		ep := endpoint.NewEndpoint("user.example.com", endpoint.RecordTypeTXT, "value")
		ep.SetProviderSpecificProperty(providerSpecificAlias, "true")
		if err := normalizeProviderSpecific(ep, true); err == nil {
			t.Fatal("ordinary TXT endpoint accepted alias metadata")
		}
	})

	t.Run("ownership TXT still rejects unknown metadata", func(t *testing.T) {
		ep := endpoint.NewEndpoint("external-dns-cname.www.example.com", endpoint.RecordTypeTXT, "heritage=external-dns")
		ep.Labels[endpoint.OwnedRecordLabelKey] = "www.example.com"
		ep.SetProviderSpecificProperty("unknown/property", "true")
		if err := normalizeProviderSpecific(ep, true); err == nil {
			t.Fatal("ownership TXT accepted unknown provider metadata")
		}
	})

	t.Run("ownership TXT validates inherited alias", func(t *testing.T) {
		ep := endpoint.NewEndpoint("external-dns-cname.www.example.com", endpoint.RecordTypeTXT, "heritage=external-dns")
		ep.Labels[endpoint.OwnedRecordLabelKey] = "www.example.com"
		ep.SetProviderSpecificProperty(providerSpecificAlias, "sometimes")
		if err := normalizeProviderSpecific(ep, true); err == nil {
			t.Fatal("ownership TXT accepted an invalid inherited alias value")
		}
	})

	t.Run("current endpoint drops force-update", func(t *testing.T) {
		ep := endpoint.NewEndpoint("www.example.com", endpoint.RecordTypeCNAME, "target.example.net")
		ep.SetProviderSpecificProperty(providerSpecificTXTForceUpdate, "true")
		if err := normalizeProviderSpecific(ep, false); err != nil {
			t.Fatal(err)
		}
		if len(ep.ProviderSpecific) != 0 {
			t.Fatalf("force-update marker was not consumed: %+v", ep.ProviderSpecific)
		}
	})

	t.Run("desired endpoint rejects force-update", func(t *testing.T) {
		ep := endpoint.NewEndpoint("www.example.com", endpoint.RecordTypeCNAME, "target.example.net")
		ep.SetProviderSpecificProperty(providerSpecificTXTForceUpdate, "true")
		if err := normalizeProviderSpecific(ep, true); err == nil {
			t.Fatal("desired endpoint accepted registry-internal force-update marker")
		}
	})
}

// ExternalDNS v0.21 marks a current endpoint with txt/force-update when it
// finds only the legacy ownership name. Exercise that migration end to end:
// the marker must survive the registry/planner boundary, be consumed by the
// provider, and result in the missing type-qualified ownership TXT record.
func TestExternalDNSV021TXTRegistryForceUpdateRepair(t *testing.T) {
	const ownerID = "registry-force-update-test"
	fake := newFakePorkbun()
	fake.seed(
		porkbun.Record{
			Name: "repair.example.com", Type: endpoint.RecordTypeCNAME,
			Content: "target.example.net", TTL: "600",
		},
		porkbun.Record{
			// Before v0.12, the ownership name omitted the record type that
			// v0.21 inserts after a literal prefix.
			Name: "external-dns-repair.example.com", Type: endpoint.RecordTypeTXT,
			Content: endpoint.Labels{endpoint.OwnerLabelKey: ownerID}.SerializePlain(false), TTL: "600",
		},
	)
	prov := newTestProvider(t, fake)
	registry, err := txtregistry.New(&externaldns.Config{
		TXTOwnerID:            ownerID,
		TXTPrefix:             "external-dns-",
		ManagedDNSRecordTypes: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
	}, upstreamProviderAdapter{provider: prov})
	if err != nil {
		t.Fatal(err)
	}

	current, err := registry.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("registry returned %d endpoints, want the owned CNAME", len(current))
	}
	if marker, ok := current[0].GetProviderSpecificProperty(providerSpecificTXTForceUpdate); !ok || marker != "true" {
		t.Fatalf("v0.21 registry did not request TXT repair: %+v", current[0])
	}

	desired, err := singleAdjustedEndpoint(registry,
		endpoint.NewEndpoint("repair.example.com", endpoint.RecordTypeCNAME, "target.example.net"))
	if err != nil {
		t.Fatal(err)
	}
	repairPlan := (&plan.Plan{
		Current: current, Desired: []*endpoint.Endpoint{desired},
		Policies: []plan.Policy{&plan.UpsertOnlyPolicy{}}, ManagedRecords: []string{endpoint.RecordTypeCNAME},
		OwnerID: ownerID,
	}).Calculate()
	if countPlanChanges(repairPlan.Changes) == 0 {
		t.Fatal("planner ignored the v0.21 TXT force-update marker")
	}
	if err := registry.ApplyChanges(context.Background(), repairPlan.Changes); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	var newFormatCount int
	for _, record := range fake.records {
		if record.Name == "external-dns-cname-repair.example.com" && record.Type == endpoint.RecordTypeTXT {
			newFormatCount++
		}
	}
	fake.mu.Unlock()
	if newFormatCount != 1 {
		t.Fatalf("new-format ownership TXT count = %d, want 1", newFormatCount)
	}

	current, err = registry.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("registry returned %d endpoints after repair, want 1", len(current))
	}
	if _, ok := current[0].GetProviderSpecificProperty(providerSpecificTXTForceUpdate); ok {
		t.Fatalf("force-update marker remained after repair: %+v", current[0])
	}
	steadyDesired, err := singleAdjustedEndpoint(registry,
		endpoint.NewEndpoint("repair.example.com", endpoint.RecordTypeCNAME, "target.example.net"))
	if err != nil {
		t.Fatal(err)
	}
	steadyPlan := (&plan.Plan{
		Current: current, Desired: []*endpoint.Endpoint{steadyDesired},
		Policies: []plan.Policy{&plan.UpsertOnlyPolicy{}}, ManagedRecords: []string{endpoint.RecordTypeCNAME},
		OwnerID: ownerID,
	}).Calculate()
	if countPlanChanges(steadyPlan.Changes) != 0 {
		t.Fatalf("repaired registry did not converge: %+v", steadyPlan.Changes)
	}
}

func TestExternalDNSV021TXTRegistryOwnershipFailurePreventsPrimaryCreate(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)
	registry, err := txtregistry.New(&externaldns.Config{
		TXTOwnerID:            "registry-failure-test",
		TXTPrefix:             "external-dns-%{record_type}.",
		ManagedDNSRecordTypes: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
	}, upstreamProviderAdapter{provider: prov})
	if err != nil {
		t.Fatal(err)
	}
	desired := endpoint.NewEndpoint("blocked.example.com", endpoint.RecordTypeCNAME, "target.example.net")
	desired.SetProviderSpecificProperty(providerSpecificAlias, "true")
	adjusted, err := registry.AdjustEndpoints([]*endpoint.Endpoint{desired})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Records(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.failCreateType.Store(endpoint.RecordTypeTXT)
	if err := registry.ApplyChanges(context.Background(), &plan.Changes{Create: adjusted}); err == nil {
		t.Fatal("forced ownership-TXT failure unexpectedly succeeded")
	}
	assertFakeRecordTypes(t, fake, map[string]int{})
}

// ExternalDNS deliberately does not adopt an existing record without its TXT
// owner marker. Prove that a partial Porkbun failure leaves the recoverable
// half of the pair (ownership first), then that the exact v0.21 registry can
// complete the main record on its next reconciliation without duplicating TXT.
func TestExternalDNSV021TXTRegistryPartialCreateRecovery(t *testing.T) {
	fake := newFakePorkbun()
	prov := newTestProvider(t, fake)
	registry, err := txtregistry.New(&externaldns.Config{
		TXTOwnerID:            "registry-recovery-test",
		TXTPrefix:             "external-dns-%{record_type}.",
		ManagedDNSRecordTypes: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
	}, upstreamProviderAdapter{provider: prov})
	if err != nil {
		t.Fatal(err)
	}

	desired := endpoint.NewEndpoint("recover.example.com", endpoint.RecordTypeCNAME, "target.example.net")
	desired.SetProviderSpecificProperty(providerSpecificAlias, "true")
	adjusted, err := registry.AdjustEndpoints([]*endpoint.Endpoint{desired})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Records(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.failCreateType.Store("ALIAS")
	if err := registry.ApplyChanges(context.Background(), &plan.Changes{Create: adjusted}); err == nil {
		t.Fatal("forced main-record failure unexpectedly succeeded")
	}
	assertFakeRecordTypes(t, fake, map[string]int{endpoint.RecordTypeTXT: 1})

	// Records refreshes the registry's existing-TXT index, just as a real
	// ExternalDNS reconciliation does before retrying ApplyChanges.
	fake.failCreateType.Store("")
	current, err := registry.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("orphan ownership TXT surfaced as a managed endpoint: %+v", current)
	}

	retryDesired := endpoint.NewEndpoint("recover.example.com", endpoint.RecordTypeCNAME, "target.example.net")
	retryDesired.SetProviderSpecificProperty(providerSpecificAlias, "true")
	adjustedRetry, err := registry.AdjustEndpoints([]*endpoint.Endpoint{retryDesired})
	if err != nil {
		t.Fatal(err)
	}
	retryPlan := (&plan.Plan{
		Current:        current,
		Desired:        adjustedRetry,
		Policies:       []plan.Policy{&plan.UpsertOnlyPolicy{}},
		ManagedRecords: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
		OwnerID:        "registry-recovery-test",
	}).Calculate()
	if len(retryPlan.Changes.Create) != 1 || retryPlan.Changes.Create[0].DNSName != "recover.example.com" {
		t.Fatalf("planner did not retry exactly the missing main record: %+v", retryPlan.Changes)
	}
	if err := registry.ApplyChanges(context.Background(), retryPlan.Changes); err != nil {
		t.Fatal(err)
	}
	assertFakeRecordTypes(t, fake, map[string]int{"ALIAS": 1, endpoint.RecordTypeTXT: 1})

	current, err = registry.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Labels[endpoint.OwnerLabelKey] != "registry-recovery-test" {
		t.Fatalf("registry did not recover the owned endpoint: %+v", current)
	}
}

func assertFakeRecordTypes(t *testing.T, fake *fakePorkbun, want map[string]int) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	got := make(map[string]int)
	for _, record := range fake.records {
		got[record.Type]++
	}
	if len(got) != len(want) {
		t.Fatalf("record type counts = %v, want %v", got, want)
	}
	for recordType, count := range want {
		if got[recordType] != count {
			t.Fatalf("record type counts = %v, want %v", got, want)
		}
	}
}
