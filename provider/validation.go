package provider

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/miekg/dns"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
)

const (
	defaultTTL                      = int64(600)
	maxDNSTTL                       = int64(1<<32 - 1)
	providerSpecificTTLDrift        = "porkbun/ttl-drift"
	providerSpecificAlias           = "alias"
	providerSpecificTTLDriftEnabled = "true"
)

// canonicalDNSName returns the representation used for endpoint and record
// identity. DNS names are case-insensitive and a final root label is optional.
func canonicalDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

func canonicalDomainFilter(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	childrenOnly := strings.HasPrefix(raw, ".")
	name := canonicalDNSName(strings.TrimPrefix(raw, "."))
	if err := validateDNSName(name, false); err != nil {
		return "", err
	}
	if childrenOnly {
		return "." + name, nil
	}
	return name, nil
}

func validatedDomainFilter(filter *endpoint.DomainFilter, zone string) (*endpoint.DomainFilter, []string, error) {
	if filter == nil || !filter.IsConfigured() {
		include := []string{zone}
		return endpoint.NewDomainFilter(include), include, nil
	}

	var config struct {
		Include      []string `json:"include"`
		Exclude      []string `json:"exclude"`
		RegexInclude string   `json:"regexInclude"`
		RegexExclude string   `json:"regexExclude"`
	}
	encoded, err := json.Marshal(filter)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect configured domain filter: %w", err)
	}
	if err := json.Unmarshal(encoded, &config); err != nil {
		return nil, nil, fmt.Errorf("inspect configured domain filter: %w", err)
	}
	if config.RegexInclude != "" || config.RegexExclude != "" {
		return nil, nil, fmt.Errorf("configured domain filter must not use regular expressions")
	}
	if len(config.Exclude) != 0 {
		return nil, nil, fmt.Errorf("configured domain filter must not use exclusions")
	}
	if len(config.Include) == 0 {
		return nil, nil, fmt.Errorf("configured domain filter must use explicit include filters")
	}

	seen := make(map[string]struct{}, len(config.Include))
	include := make([]string, 0, len(config.Include))
	for _, raw := range config.Include {
		canonical, err := canonicalDomainFilter(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid domain filter %q: %w", raw, err)
		}
		if !withinZone(strings.TrimPrefix(canonical, "."), zone) {
			return nil, nil, fmt.Errorf("domain filter %q is not within zone %q", raw, zone)
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		include = append(include, canonical)
	}
	sort.Strings(include)
	return endpoint.NewDomainFilter(include), include, nil
}

func matchesExplicitFilters(filters []string, name string) bool {
	if len(filters) == 0 {
		return true
	}
	name = canonicalDNSName(name)
	for _, filter := range filters {
		if strings.HasPrefix(filter, ".") {
			base := strings.TrimPrefix(filter, ".")
			if name != base && strings.HasSuffix(name, "."+base) {
				return true
			}
			continue
		}
		if name == filter || strings.HasSuffix(name, "."+filter) {
			return true
		}
	}
	return false
}

func sortProviderSpecific(ep *endpoint.Endpoint) {
	if ep == nil {
		return
	}
	sort.Slice(ep.ProviderSpecific, func(i, j int) bool {
		if ep.ProviderSpecific[i].Name != ep.ProviderSpecific[j].Name {
			return ep.ProviderSpecific[i].Name < ep.ProviderSpecific[j].Name
		}
		return ep.ProviderSpecific[i].Value < ep.ProviderSpecific[j].Value
	})
}

// validateDNSName validates the ASCII presentation accepted by Porkbun. An
// underscore is allowed because service records use owner names such as
// _sip._tcp.example.com. Unicode names should be supplied in A-label form.
func validateDNSName(name string, allowWildcard bool) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("name is longer than 253 bytes")
	}
	labels := strings.Split(name, ".")
	for pos, label := range labels {
		if label == "" {
			return fmt.Errorf("name contains an empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("label %q is longer than 63 bytes", label)
		}
		if label == "*" {
			if !allowWildcard || pos != 0 {
				return fmt.Errorf("wildcard is only allowed as the first owner-name label")
			}
			continue
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q starts or ends with a hyphen", label)
		}
		for _, r := range label {
			valid := r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
			if r > unicode.MaxASCII || !valid {
				return fmt.Errorf("label %q contains invalid character %q", label, r)
			}
		}
	}
	return nil
}

func withinZone(name, zone string) bool {
	name = canonicalDNSName(name)
	zone = canonicalDNSName(zone)
	return name == zone || strings.HasSuffix(name, "."+zone)
}

func effectiveTTL(ttl endpoint.TTL) int64 {
	if int64(ttl) < defaultTTL {
		return defaultTTL
	}
	return int64(ttl)
}

func (p *Provider) adjustEndpoint(ep *endpoint.Endpoint, canonicalName string) error {
	if err := validateDNSName(canonicalName, true); err != nil {
		return fmt.Errorf("invalid DNS name %q: %w", ep.DNSName, err)
	}
	ep.DNSName = canonicalName
	ep.RecordType = strings.ToUpper(strings.TrimSpace(ep.RecordType))
	if ep.RecordType == "ALIAS" {
		return fmt.Errorf("record type ALIAS must be represented as CNAME with providerSpecific alias=true")
	}
	if !managedEndpointType(ep.RecordType) {
		return fmt.Errorf("unsupported record type %q", ep.RecordType)
	}
	if ep.SetIdentifier != "" {
		return fmt.Errorf("setIdentifier %q is unsupported", ep.SetIdentifier)
	}
	if ep.RecordTTL < 0 || int64(ep.RecordTTL) > maxDNSTTL {
		return fmt.Errorf("TTL %d is outside the DNS uint32 range", ep.RecordTTL)
	}
	ep.RecordTTL = endpoint.TTL(effectiveTTL(ep.RecordTTL))
	if err := normalizeProviderSpecific(ep, true); err != nil {
		return err
	}
	if ep.RecordType == endpoint.RecordTypeCNAME && ep.DNSName == p.domain {
		ep.SetProviderSpecificProperty(providerSpecificAlias, "true")
	}
	if len(ep.Targets) == 0 {
		return fmt.Errorf("endpoint has no targets")
	}

	targets := make(map[string]struct{}, len(ep.Targets))
	for i, target := range ep.Targets {
		canonical, err := canonicalEndpointTarget(ep.RecordType, target)
		if err != nil {
			return fmt.Errorf("invalid target[%d] %q: %w", i, target, err)
		}
		targets[canonical] = struct{}{}
	}
	ep.Targets = endpoint.Targets(sortedMapKeys(targets))
	sortProviderSpecific(ep)
	return nil
}

func normalizeProviderSpecific(ep *endpoint.Endpoint, desired bool) error {
	seen := make(map[string]struct{}, len(ep.ProviderSpecific))
	normalized := make(endpoint.ProviderSpecific, 0, len(ep.ProviderSpecific))
	for _, property := range ep.ProviderSpecific {
		if _, duplicate := seen[property.Name]; duplicate {
			return fmt.Errorf("duplicate providerSpecific property %q", property.Name)
		}
		seen[property.Name] = struct{}{}

		switch property.Name {
		case providerSpecificAlias:
			value := strings.ToLower(strings.TrimSpace(property.Value))
			if value != "true" && value != "false" {
				return fmt.Errorf("providerSpecific alias must be true or false, got %q", property.Value)
			}
			if ep.RecordType != endpoint.RecordTypeCNAME {
				return fmt.Errorf("providerSpecific alias is only supported for CNAME endpoints")
			}
			if value == "true" {
				normalized = append(normalized, endpoint.ProviderSpecificProperty{Name: providerSpecificAlias, Value: "true"})
			}
		case providerSpecificTTLDrift:
			if desired {
				return fmt.Errorf("providerSpecific property %q is provider-internal", property.Name)
			}
			if property.Value != providerSpecificTTLDriftEnabled {
				return fmt.Errorf("invalid provider-internal TTL drift marker %q", property.Value)
			}
			normalized = append(normalized, property)
		default:
			return fmt.Errorf("unsupported providerSpecific property %q", property.Name)
		}
	}
	ep.ProviderSpecific = normalized
	sortProviderSpecific(ep)
	return nil
}

// validateChanges canonicalizes and validates the complete batch before
// ApplyChanges performs even its initial retrieve. This prevents a later bad
// endpoint from turning a batch into a partial write.
func (p *Provider) validateChanges(changes *plan.Changes) error {
	if changes == nil {
		return validationErrorf("changes must not be nil")
	}
	if len(changes.UpdateOld) != len(changes.UpdateNew) {
		return validationErrorf("updateOld and updateNew lengths differ (%d != %d)", len(changes.UpdateOld), len(changes.UpdateNew))
	}

	for i, ep := range changes.Create {
		if err := p.validateEndpoint(ep, true); err != nil {
			return validationErrorf("create[%d]: %v", i, err)
		}
	}
	for i, ep := range changes.Delete {
		if err := p.validateEndpoint(ep, false); err != nil {
			return validationErrorf("delete[%d]: %v", i, err)
		}
	}
	for i := range changes.UpdateOld {
		oldEP, newEP := changes.UpdateOld[i], changes.UpdateNew[i]
		if err := p.validateEndpoint(oldEP, false); err != nil {
			return validationErrorf("updateOld[%d]: %v", i, err)
		}
		if err := p.validateEndpoint(newEP, true); err != nil {
			return validationErrorf("updateNew[%d]: %v", i, err)
		}
		if oldEP.DNSName != newEP.DNSName || oldEP.RecordType != newEP.RecordType {
			return validationErrorf("update[%d] changes identity from %s/%s to %s/%s", i, oldEP.DNSName, oldEP.RecordType, newEP.DNSName, newEP.RecordType)
		}
	}
	return nil
}

func (p *Provider) validateEndpoint(ep *endpoint.Endpoint, desired bool) error {
	if ep == nil {
		return fmt.Errorf("endpoint must not be nil")
	}
	ep.DNSName = canonicalDNSName(ep.DNSName)
	if err := validateDNSName(ep.DNSName, true); err != nil {
		return fmt.Errorf("invalid DNS name %q: %w", ep.DNSName, err)
	}
	if !withinZone(ep.DNSName, p.domain) {
		return fmt.Errorf("DNS name %q is outside zone %q", ep.DNSName, p.domain)
	}
	if !p.inFilter(ep.DNSName) {
		return fmt.Errorf("DNS name %q is outside the configured domain filter", ep.DNSName)
	}

	ep.RecordType = strings.ToUpper(strings.TrimSpace(ep.RecordType))
	if ep.RecordType == "ALIAS" {
		return fmt.Errorf("record type ALIAS must be represented as CNAME with providerSpecific alias=true")
	}
	if !managedEndpointType(ep.RecordType) {
		return fmt.Errorf("unsupported record type %q", ep.RecordType)
	}
	if ep.SetIdentifier != "" {
		return fmt.Errorf("setIdentifier %q is unsupported", ep.SetIdentifier)
	}
	if ep.RecordTTL < 0 || int64(ep.RecordTTL) > maxDNSTTL {
		return fmt.Errorf("TTL %d is outside the DNS uint32 range", ep.RecordTTL)
	}
	ep.RecordTTL = endpoint.TTL(effectiveTTL(ep.RecordTTL))

	if err := normalizeProviderSpecific(ep, desired); err != nil {
		return err
	}
	if ep.RecordType == endpoint.RecordTypeCNAME && ep.DNSName == p.domain {
		ep.SetProviderSpecificProperty(providerSpecificAlias, "true")
	}

	if len(ep.Targets) == 0 {
		return fmt.Errorf("endpoint has no targets")
	}
	seen := make(map[string]struct{}, len(ep.Targets))
	for i, target := range ep.Targets {
		canonical, err := canonicalEndpointTarget(ep.RecordType, target)
		if err != nil {
			return fmt.Errorf("invalid target[%d] %q: %w", i, target, err)
		}
		if desired {
			if _, exists := seen[canonical]; exists {
				return fmt.Errorf("duplicate desired target %q", canonical)
			}
		}
		seen[canonical] = struct{}{}
		ep.Targets[i] = canonical
	}
	sort.Strings(ep.Targets)
	return nil
}

// recordToEndpointTarget decodes Porkbun's split priority representation into
// ExternalDNS's conventional target form. ALIAS is intentionally exposed as a
// CNAME endpoint so it can flow through ExternalDNS's planner.
func recordToEndpointTarget(r porkbun.Record) (viewType, target string, err error) {
	actualType := strings.ToUpper(strings.TrimSpace(r.Type))
	if !managedType(actualType) {
		return "", "", fmt.Errorf("unsupported record type %q", r.Type)
	}
	viewType = actualType
	if actualType == "ALIAS" {
		viewType = endpoint.RecordTypeCNAME
	}

	switch actualType {
	case endpoint.RecordTypeMX:
		priority, err := canonicalPriority(r.Prio, true)
		if err != nil {
			return "", "", fmt.Errorf("invalid MX priority: %w", err)
		}
		target, err = canonicalEndpointTarget(actualType, priority+" "+r.Content)
		if err != nil {
			return "", "", err
		}
	case endpoint.RecordTypeSRV:
		priority, err := canonicalPriority(r.Prio, true)
		if err != nil {
			return "", "", fmt.Errorf("invalid SRV priority: %w", err)
		}
		parts := strings.Fields(r.Content)
		if len(parts) != 3 {
			return "", "", fmt.Errorf("SRV content must be weight, port, and target")
		}
		target, err = canonicalEndpointTarget(actualType, strings.Join([]string{priority, parts[0], parts[1], parts[2]}, " "))
		if err != nil {
			return "", "", err
		}
	default:
		target, err = canonicalEndpointTarget(viewType, normaliseContent(actualType, r.Content))
		if err != nil {
			return "", "", err
		}
	}
	return viewType, target, nil
}

func (p *Provider) recordInput(ep *endpoint.Endpoint, target string) (porkbun.RecordInput, error) {
	if ep == nil {
		return porkbun.RecordInput{}, fmt.Errorf("endpoint must not be nil")
	}
	recType := strings.ToUpper(strings.TrimSpace(ep.RecordType))
	if recType == "ALIAS" {
		return porkbun.RecordInput{}, fmt.Errorf("direct ALIAS endpoints are unsupported")
	}
	canonical, err := canonicalEndpointTarget(recType, target)
	if err != nil {
		return porkbun.RecordInput{}, err
	}
	actualType := recType
	if recType == endpoint.RecordTypeCNAME {
		alias, _ := ep.GetProviderSpecificProperty("alias")
		if canonicalDNSName(ep.DNSName) == p.domain || strings.EqualFold(strings.TrimSpace(alias), "true") {
			actualType = "ALIAS"
		}
	}

	in := porkbun.RecordInput{
		Name: p.subdomainOf(ep.DNSName),
		Type: actualType,
		TTL:  ttlString(ep.RecordTTL),
	}
	switch recType {
	case endpoint.RecordTypeMX:
		parts := strings.Fields(canonical)
		in.Prio, in.Content = parts[0], trimRootDot(parts[1])
	case endpoint.RecordTypeSRV:
		parts := strings.Fields(canonical)
		in.Prio = parts[0]
		in.Content = strings.Join([]string{parts[1], parts[2], trimRootDot(parts[3])}, " ")
	default:
		in.Content = serialiseContent(recType, canonical)
	}
	return in, nil
}

func canonicalEndpointTarget(recType, target string) (string, error) {
	recType = strings.ToUpper(strings.TrimSpace(recType))
	switch recType {
	case endpoint.RecordTypeA, endpoint.RecordTypeAAAA:
		addr, err := netip.ParseAddr(strings.TrimSpace(target))
		if err != nil {
			return "", fmt.Errorf("not an IP address: %w", err)
		}
		if addr.Zone() != "" {
			return "", fmt.Errorf("IP address must not contain a zone identifier")
		}
		if recType == endpoint.RecordTypeA && !addr.Is4() {
			return "", fmt.Errorf("target is not IPv4 for an A record")
		}
		if recType == endpoint.RecordTypeAAAA && (!addr.Is6() || addr.Is4In6()) {
			return "", fmt.Errorf("AAAA target is not IPv6")
		}
		return addr.String(), nil
	case endpoint.RecordTypeCNAME, "ALIAS", endpoint.RecordTypeNS:
		return canonicalHost(target, false, false)
	case endpoint.RecordTypeMX:
		parts := strings.Fields(target)
		if len(parts) != 2 {
			return "", fmt.Errorf("MX target must be 'priority host'")
		}
		priority, err := canonicalPriority(parts[0], false)
		if err != nil {
			return "", err
		}
		host, err := canonicalHost(parts[1], true, false)
		if err != nil {
			return "", err
		}
		return priority + " " + host, nil
	case endpoint.RecordTypeSRV:
		parts := strings.Fields(target)
		if len(parts) != 4 {
			return "", fmt.Errorf("SRV target must be 'priority weight port host'")
		}
		for i := 0; i < 3; i++ {
			value, err := canonicalUint(parts[i], 16)
			if err != nil {
				return "", fmt.Errorf("invalid SRV numeric field %d: %w", i, err)
			}
			parts[i] = value
		}
		host, err := canonicalHost(parts[3], true, true)
		if err != nil {
			return "", err
		}
		return strings.Join([]string{parts[0], parts[1], parts[2], host}, " "), nil
	case endpoint.RecordTypeTXT:
		return normaliseContent(recType, target), nil
	case "TLSA":
		parts := strings.Fields(target)
		if len(parts) != 4 {
			return "", fmt.Errorf("TLSA target must be 'usage selector matching-type association-data'")
		}
		limits := []uint64{3, 1, 2}
		for i, limit := range limits {
			value, err := canonicalUintMax(parts[i], 8, limit)
			if err != nil {
				return "", fmt.Errorf("invalid TLSA field %d: %w", i, err)
			}
			parts[i] = value
		}
		data, err := canonicalHex(parts[3])
		if err != nil {
			return "", fmt.Errorf("invalid TLSA association data: %w", err)
		}
		parts[3] = data
		return strings.Join(parts, " "), nil
	case "SSHFP":
		parts := strings.Fields(target)
		if len(parts) != 3 {
			return "", fmt.Errorf("SSHFP target must be 'algorithm fingerprint-type fingerprint'")
		}
		for i := 0; i < 2; i++ {
			value, err := canonicalUint(parts[i], 8)
			if err != nil {
				return "", fmt.Errorf("invalid SSHFP numeric field %d: %w", i, err)
			}
			parts[i] = value
		}
		fingerprint, err := canonicalHex(parts[2])
		if err != nil {
			return "", fmt.Errorf("invalid SSHFP fingerprint: %w", err)
		}
		parts[2] = fingerprint
		return strings.Join(parts, " "), nil
	case "CAA":
		return canonicalCAATarget(target)
	case "HTTPS", "SVCB":
		return canonicalServiceBindingTarget(recType, target)
	default:
		return "", fmt.Errorf("unsupported record type %q", recType)
	}
}

func canonicalCAATarget(target string) (string, error) {
	rr, err := parseStrictPresentationRR("CAA", target)
	if err != nil {
		return "", err
	}
	caa, ok := rr.(*dns.CAA)
	if !ok {
		return "", fmt.Errorf("parsed record is not CAA")
	}
	caa.Tag = strings.ToLower(caa.Tag)
	if caa.Tag == "" {
		return "", fmt.Errorf("CAA tag is empty")
	}
	for _, r := range caa.Tag {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", fmt.Errorf("CAA tag %q contains invalid character", caa.Tag)
		}
	}
	return strings.TrimPrefix(caa.String(), caa.Hdr.String()), nil
}

func canonicalServiceBindingTarget(recType, target string) (string, error) {
	rr, err := parseStrictPresentationRR(recType, target)
	if err != nil {
		return "", err
	}

	var svc *dns.SVCB
	switch typed := rr.(type) {
	case *dns.SVCB:
		svc = typed
	case *dns.HTTPS:
		svc = &typed.SVCB
	default:
		return "", fmt.Errorf("parsed record is not %s", recType)
	}
	if err := validateServiceBinding(svc); err != nil {
		return "", fmt.Errorf("invalid %s target: %w", recType, err)
	}

	targetName, err := canonicalHost(svc.Target, true, false)
	if err != nil {
		return "", fmt.Errorf("invalid %s target name: %w", recType, err)
	}
	sort.Slice(svc.Value, func(i, j int) bool { return svc.Value[i].Key() < svc.Value[j].Key() })

	var result strings.Builder
	result.WriteString(strconv.FormatUint(uint64(svc.Priority), 10))
	result.WriteByte(' ')
	result.WriteString(targetName)
	for _, parameter := range svc.Value {
		result.WriteByte(' ')
		result.WriteString(parameter.Key().String())
		result.WriteString(`="`)
		result.WriteString(parameter.String())
		result.WriteByte('"')
	}
	return result.String(), nil
}

func validateServiceBinding(svc *dns.SVCB) error {
	if svc == nil {
		return fmt.Errorf("record is nil")
	}

	present := make(map[dns.SVCBKey]struct{}, len(svc.Value))
	var mandatory *dns.SVCBMandatory
	for _, parameter := range svc.Value {
		key := parameter.Key()
		if key.String() == "" {
			return fmt.Errorf("reserved or invalid SvcParam key")
		}
		if _, duplicate := present[key]; duplicate {
			return fmt.Errorf("duplicate SvcParam key %q", key.String())
		}
		present[key] = struct{}{}
		switch value := parameter.(type) {
		case *dns.SVCBMandatory:
			mandatory = value
		case *dns.SVCBAlpn:
			if len(value.Alpn) == 0 {
				return fmt.Errorf("alpn must list at least one protocol")
			}
		}
	}

	// AliasMode SvcParams are legal presentation data but are ignored by
	// recipients. Preserve them losslessly; self-consistency only applies to
	// ServiceMode records.
	if svc.Priority != 0 {
		if _, noDefaultALPN := present[dns.SVCB_NO_DEFAULT_ALPN]; noDefaultALPN {
			if _, hasALPN := present[dns.SVCB_ALPN]; !hasALPN {
				return fmt.Errorf("no-default-alpn requires alpn")
			}
		}
		if mandatory != nil {
			if len(mandatory.Code) == 0 {
				return fmt.Errorf("mandatory must list at least one key")
			}
			listed := make(map[dns.SVCBKey]struct{}, len(mandatory.Code))
			for _, key := range mandatory.Code {
				if key.String() == "" || key == dns.SVCB_MANDATORY {
					return fmt.Errorf("mandatory contains reserved or invalid key")
				}
				if _, duplicate := listed[key]; duplicate {
					return fmt.Errorf("mandatory contains duplicate key %q", key.String())
				}
				listed[key] = struct{}{}
				if _, exists := present[key]; !exists {
					return fmt.Errorf("mandatory key %q is not present", key.String())
				}
			}
			sort.Slice(mandatory.Code, func(i, j int) bool { return mandatory.Code[i] < mandatory.Code[j] })
		}
	}
	return nil
}

func parseStrictPresentationRR(recType, target string) (dns.RR, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("%s target is empty", recType)
	}
	if strings.ContainsAny(target, "\r\n") || hasUnescapedComment(target) {
		return nil, fmt.Errorf("%s target contains unsupported record separators or comments", recType)
	}
	presentation := "canonical.invalid. 600 IN " + recType + " " + target
	rr, err := dns.NewRR(presentation)
	if err != nil {
		return nil, fmt.Errorf("invalid %s presentation: %w", recType, err)
	}
	buffer := make([]byte, dns.Len(rr))
	if _, err := dns.PackRR(rr, buffer, 0, nil, false); err != nil {
		return nil, fmt.Errorf("invalid %s wire data: %w", recType, err)
	}
	return rr, nil
}

func hasUnescapedComment(value string) bool {
	quoted := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			if i+1 < len(value) {
				i++
			}
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				return true
			}
		}
	}
	return false
}

func canonicalPriority(value string, emptyIsZero bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && emptyIsZero {
		value = "0"
	}
	return canonicalUint(value, 16)
}

func canonicalUint(value string, bits int) (string, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, bits)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(parsed, 10), nil
}

func canonicalUintMax(value string, bits int, maxValue uint64) (string, error) {
	canonical, err := canonicalUint(value, bits)
	if err != nil {
		return "", err
	}
	parsed, _ := strconv.ParseUint(canonical, 10, bits)
	if parsed > maxValue {
		return "", fmt.Errorf("value %d exceeds %d", parsed, maxValue)
	}
	return canonical, nil
}

func canonicalHex(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("value is empty")
	}
	if len(value)%2 != 0 {
		return "", fmt.Errorf("value has an odd number of hexadecimal digits")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", err
	}
	return strings.ToLower(value), nil
}

func canonicalHost(value string, allowRoot, trailingDot bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "." {
		if allowRoot {
			return ".", nil
		}
		return "", fmt.Errorf("root target is not valid for this record type")
	}
	host := canonicalDNSName(value)
	if err := validateDNSName(host, false); err != nil {
		return "", fmt.Errorf("invalid host %q: %w", value, err)
	}
	if trailingDot {
		return host + ".", nil
	}
	return host, nil
}

func trimRootDot(value string) string {
	if value == "." {
		return value
	}
	return strings.TrimSuffix(value, ".")
}

func uniqueStrings(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedMapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func preferredRecordID(ids []string, idx *index, desired porkbun.RecordInput) string {
	bestID, bestScore := "", -1
	for _, id := range ids {
		r, ok := idx.byID[id]
		if !ok {
			continue
		}
		score := 0
		if strings.EqualFold(strings.TrimSpace(r.Type), desired.Type) {
			score++
		}
		if recordTTLMatches(r, desired.TTL) {
			score += 2
		}
		if score > bestScore || score == bestScore && (bestID == "" || id < bestID) {
			bestID, bestScore = id, score
		}
	}
	return bestID
}

func recordMatchesInput(r porkbun.Record, desired porkbun.RecordInput) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Type), desired.Type) {
		return false
	}
	if !recordTTLMatches(r, desired.TTL) {
		return false
	}
	_, currentTarget, err := recordToEndpointTarget(r)
	if err != nil {
		return false
	}
	desiredRecord := porkbun.Record{Type: desired.Type, Content: desired.Content, Prio: desired.Prio, TTL: desired.TTL}
	_, desiredTarget, err := recordToEndpointTarget(desiredRecord)
	return err == nil && currentTarget == desiredTarget
}

func recordTTLMatches(record porkbun.Record, desired string) bool {
	currentTTL, valid := parseRecordTTL(record)
	if !valid {
		return false
	}
	desiredTTL, err := strconv.ParseInt(strings.TrimSpace(desired), 10, 64)
	return err == nil && currentTTL == desiredTTL
}

func parseRecordTTL(record porkbun.Record) (int64, bool) {
	ttl, err := strconv.ParseInt(strings.TrimSpace(record.TTL), 10, 64)
	return ttl, err == nil
}
