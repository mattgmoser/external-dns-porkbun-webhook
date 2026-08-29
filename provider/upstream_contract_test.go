package provider

import (
	"encoding/json"
	"regexp"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
)

// validatedDomainFilter cannot read endpoint.DomainFilter's unexported fields,
// so it inspects the filter through the type's own JSON marshaller. That makes
// the scope guard depend on an upstream serialisation contract this project
// does not own: if ExternalDNS renamed or dropped a key, the guard would stop
// observing exclusions or regular expressions and silently widen the blast
// radius of a misconfigured filter rather than failing closed.
//
// These tests pin that contract to the bundled ExternalDNS release so an
// upgrade fails here, loudly, instead of in production.

func TestUpstreamDomainFilterJSONContract(t *testing.T) {
	t.Parallel()

	filter := endpoint.NewDomainFilter([]string{"example.com"})
	encoded, err := json.Marshal(filter)
	if err != nil {
		t.Fatalf("marshal include filter: %v", err)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatalf("unmarshal include filter into probe: %v", err)
	}
	if _, ok := probe["include"]; !ok {
		t.Fatalf("upstream DomainFilter no longer serialises an %q key; validatedDomainFilter would stop seeing configured scope: %s", "include", encoded)
	}

	var config struct {
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(encoded, &config); err != nil {
		t.Fatalf("unmarshal include filter: %v", err)
	}
	if len(config.Include) != 1 || config.Include[0] != "example.com" {
		t.Fatalf("include filter round-trip = %v, want [example.com]", config.Include)
	}
}

// A filter this provider must reject has to remain visible through the same
// JSON view. If these keys disappear, validatedDomainFilter would accept an
// exclusion or regex filter it cannot actually honour.
func TestUpstreamDomainFilterRejectableShapesRemainVisible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		filter  *endpoint.DomainFilter
		wantKey string
	}{
		{
			name:    "exclusion",
			filter:  endpoint.NewDomainFilterWithExclusions([]string{"example.com"}, []string{"skip.example.com"}),
			wantKey: "exclude",
		},
		{
			name:    "regex include",
			filter:  endpoint.NewRegexDomainFilter(regexpMustCompileForTest(t, `^example\.com$`), nil),
			wantKey: "regexInclude",
		},
		{
			name:    "regex exclude",
			filter:  endpoint.NewRegexDomainFilter(nil, regexpMustCompileForTest(t, `^skip\.example\.com$`)),
			wantKey: "regexExclude",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(tc.filter)
			if err != nil {
				t.Fatalf("marshal %s filter: %v", tc.name, err)
			}
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &probe); err != nil {
				t.Fatalf("unmarshal %s filter: %v", tc.name, err)
			}
			if _, ok := probe[tc.wantKey]; !ok {
				t.Fatalf("upstream DomainFilter no longer serialises %q; validatedDomainFilter would stop rejecting %s filters: %s",
					tc.wantKey, tc.name, encoded)
			}

			// The guard must still reject it end to end.
			if _, _, err := validatedDomainFilter(tc.filter, "example.com"); err == nil {
				t.Fatalf("validatedDomainFilter accepted a %s filter it cannot honour", tc.name)
			}
		})
	}
}

// The provider maps Porkbun's record vocabulary onto ExternalDNS's. DNAME
// arrived in ExternalDNS v0.22 and Porkbun does not offer it, so it must stay
// unmanaged: ExternalDNS only sends types listed in --managed-record-types
// (default A, AAAA, CNAME), and an operator who opts DNAME in should get a
// clear validation error rather than a silently dropped record.
func TestDNAMEIsNotManaged(t *testing.T) {
	t.Parallel()

	if managedType(endpoint.RecordTypeDNAME) {
		t.Fatal("DNAME is reported as managed, but Porkbun has no DNAME record type")
	}
	if managedEndpointType(endpoint.RecordTypeDNAME) {
		t.Fatal("DNAME is reported as a managed endpoint type, but Porkbun has no DNAME record type")
	}
}

// Every type this provider claims to manage must still be a type ExternalDNS
// knows how to plan, apart from Porkbun's native ALIAS, which is deliberately
// presented to ExternalDNS as a CNAME.
func TestManagedTypesRemainKnownUpstream(t *testing.T) {
	t.Parallel()

	// Types Porkbun accepts that ExternalDNS has no constant for are still
	// legitimate to store; they simply never appear in a plan by default.
	upstreamKnown := map[string]bool{
		endpoint.RecordTypeA:     true,
		endpoint.RecordTypeAAAA:  true,
		endpoint.RecordTypeCNAME: true,
		endpoint.RecordTypeTXT:   true,
		endpoint.RecordTypeMX:    true,
		endpoint.RecordTypeNS:    true,
		endpoint.RecordTypeSRV:   true,
	}
	for recType := range upstreamKnown {
		if !managedType(recType) {
			t.Fatalf("record type %s is planned by ExternalDNS but not managed by this provider", recType)
		}
	}
}

func regexpMustCompileForTest(t *testing.T, expr string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(expr)
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return re
}
