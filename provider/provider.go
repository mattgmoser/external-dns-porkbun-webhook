// Package provider implements the external-dns Provider interface against the
// Porkbun DNS API.
//
// Mapping:
//
//	Porkbun Record.Name (FQDN) ↔ external-dns endpoint.DNSName (FQDN)
//	Porkbun Record.Content     ↔ endpoint.Targets[0]   (one record per target)
//	Porkbun Record.Type        ↔ endpoint.RecordType
//	Porkbun Record.TTL         ↔ endpoint.RecordTTL
//	Porkbun Record.Prio        ↔ the priority field in MX/SRV targets
//
// Porkbun stores records per domain. Multiple records with the same name+type
// but different content represent multi-target endpoints (e.g. round-robin A).
// We collapse these into one external-dns endpoint with multiple Targets, and
// expand back when applying changes.
package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/mattgmoser/external-dns-porkbun-webhook/porkbun"
)

// Provider implements the external-dns webhook provider for Porkbun.
type Provider struct {
	client       *porkbun.Client
	domain       string                 // Porkbun zone, e.g. "example.com"
	domainFilter *endpoint.DomainFilter // optional further-restricted scope
	include      []string               // validated explicit include filters
	cache        *recordCache
	dryRun       bool
	applyMu      sync.Mutex
}

// Config configures a Provider.
type Config struct {
	APIKey       string
	SecretAPIKey string
	Domain       string                 // the Porkbun zone (root domain)
	DomainFilter *endpoint.DomainFilter // optional; defaults to {Domain}
	DryRun       bool
	CacheTTL     time.Duration // 0 = no caching
}

// ValidationError reports a caller-supplied change set that the provider
// cannot safely or losslessly apply. Callers may use errors.As to distinguish
// these errors from Porkbun connectivity and API failures.
type ValidationError struct {
	Err error
}

func (e *ValidationError) Error() string {
	if e == nil || e.Err == nil {
		return "invalid provider change set"
	}
	return "invalid provider change set: " + e.Err.Error()
}

// Unwrap exposes the underlying validation failure.
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func validationErrorf(format string, args ...any) error {
	return &ValidationError{Err: fmt.Errorf(format, args...)}
}

// New constructs a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" || cfg.SecretAPIKey == "" {
		return nil, fmt.Errorf("api credentials required")
	}
	zone := canonicalDNSName(cfg.Domain)
	if zone == "" {
		return nil, fmt.Errorf("domain required")
	}
	if err := validateDNSName(zone, false); err != nil {
		return nil, fmt.Errorf("invalid domain %q: %w", cfg.Domain, err)
	}
	cfg.Domain = zone
	domainFilter, include, err := validatedDomainFilter(cfg.DomainFilter, zone)
	if err != nil {
		return nil, err
	}
	cfg.DomainFilter = domainFilter
	c := porkbun.New(cfg.APIKey, cfg.SecretAPIKey)
	return &Provider{
		client:       c,
		domain:       cfg.Domain,
		domainFilter: cfg.DomainFilter,
		include:      include,
		dryRun:       cfg.DryRun,
		cache:        newRecordCache(cfg.CacheTTL),
	}, nil
}

// Ping verifies credentials work. Call once at startup.
func (p *Provider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// GetDomainFilter returns the domain filter so external-dns can pre-filter.
func (p *Provider) GetDomainFilter() *endpoint.DomainFilter { return p.domainFilter }

// Records returns the current state of the world (the records we manage).
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	records, err := p.cachedRetrieve(ctx)
	if err != nil {
		return nil, err
	}

	// Collapse same name+type into one endpoint with multiple targets. Target
	// multiplicity is deliberately retained: if Porkbun contains duplicate
	// records, ExternalDNS can plan an update and updateEndpoint can remove the
	// extras rather than permanently hiding the drift.
	type key struct{ name, recType string }
	grouped := map[key]*endpoint.Endpoint{}
	for _, r := range records {
		actualType := strings.ToUpper(strings.TrimSpace(r.Type))
		if !managedType(actualType) {
			continue
		}
		name := canonicalDNSName(r.Name)
		if !p.nameInScope(name) {
			continue
		}
		if err := validateDNSName(name, true); err != nil {
			return nil, fmt.Errorf("invalid Porkbun record name %q: %w", r.Name, err)
		}
		viewType, target, err := recordToEndpointTarget(r)
		if err != nil {
			return nil, fmt.Errorf("decode Porkbun record %s/%s id=%s: %w", name, actualType, r.ID, err)
		}
		k := key{name, viewType}
		rawTTL, validTTL := parseRecordTTL(r)
		if !validTTL {
			rawTTL = 0
		}
		ttl := endpoint.TTL(effectiveTTL(endpoint.TTL(rawTTL)))
		ep, ok := grouped[k]
		if !ok {
			ep = &endpoint.Endpoint{
				DNSName:    name,
				RecordType: viewType,
				RecordTTL:  ttl,
			}
			grouped[k] = ep
		} else if ttl != ep.RecordTTL {
			// A DNS RRset should have one TTL. Mark drift so ExternalDNS's v0.21
			// planner schedules an update even when the minimum happens to equal
			// the desired TTL.
			ep.SetProviderSpecificProperty(providerSpecificTTLDrift, providerSpecificTTLDriftEnabled)
			if ttl < ep.RecordTTL {
				ep.RecordTTL = ttl
			}
		}
		if !validTTL || rawTTL != int64(ttl) {
			// Porkbun applies a 600-second floor. Surface a legacy/unset raw TTL
			// so it is repaired rather than hidden behind the effective value.
			ep.SetProviderSpecificProperty(providerSpecificTTLDrift, providerSpecificTTLDriftEnabled)
		}
		if actualType == "ALIAS" {
			ep.SetProviderSpecificProperty(providerSpecificAlias, "true")
		}
		ep.Targets = append(ep.Targets, target)
	}

	out := make([]*endpoint.Endpoint, 0, len(grouped))
	for _, ep := range grouped {
		sort.Strings(ep.Targets)
		sortProviderSpecific(ep)
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DNSName != out[j].DNSName {
			return out[i].DNSName < out[j].DNSName
		}
		return out[i].RecordType < out[j].RecordType
	})
	return out, nil
}

// ApplyChanges applies the given changes to Porkbun.
//
// external-dns batches changes as Create + UpdateOld+UpdateNew + Delete. For
// idempotence and safety we re-fetch before each apply to catch drift.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	if err := p.validateChanges(changes); err != nil {
		return err
	}
	current, err := p.client.Retrieve(ctx, p.domain)
	if err != nil {
		return fmt.Errorf("retrieve before apply: %w", err)
	}
	p.cache.invalidate()
	defer p.cache.invalidate()

	idx, err := indexRecords(p.scopedRecords(current))
	if err != nil {
		return fmt.Errorf("index current records: %w", err)
	}

	// Order matters: Delete first (frees up name+content collisions), Create+Update next.
	for _, ep := range changes.Delete {
		if err := p.deleteEndpoint(ctx, ep, &idx); err != nil {
			return fmt.Errorf("delete %s/%s: %w", ep.DNSName, ep.RecordType, err)
		}
	}

	// Updates: external-dns sends UpdateOld + UpdateNew with same length and matching positions.
	for i, newEP := range changes.UpdateNew {
		var oldEP *endpoint.Endpoint
		if i < len(changes.UpdateOld) {
			oldEP = changes.UpdateOld[i]
		}
		if err := p.updateEndpoint(ctx, oldEP, newEP, &idx); err != nil {
			return fmt.Errorf("update %s/%s: %w", newEP.DNSName, newEP.RecordType, err)
		}
	}

	// ExternalDNS's TXT registry appends ownership records after the records
	// they protect. Reverse that dependency before writing: if a later main
	// record create fails, the next reconciliation can observe the ownership
	// TXT and safely finish the create. Creating the main record first can leave
	// an unowned record that the registry deliberately will not adopt.
	for _, ep := range ownershipFirstCreates(changes.Create) {
		if err := p.createEndpoint(ctx, ep, &idx); err != nil {
			return fmt.Errorf("create %s/%s: %w", ep.DNSName, ep.RecordType, err)
		}
	}
	return nil
}

func ownershipFirstCreates(creates []*endpoint.Endpoint) []*endpoint.Endpoint {
	ordered := make([]*endpoint.Endpoint, 0, len(creates))
	for _, ep := range creates {
		if isRegistryOwnershipTXT(ep) {
			ordered = append(ordered, ep)
		}
	}
	for _, ep := range creates {
		if !isRegistryOwnershipTXT(ep) {
			ordered = append(ordered, ep)
		}
	}
	return ordered
}

// AdjustEndpoints lets a provider tweak desired endpoints before they're stored.
// Most providers no-op this, but Porkbun has a 600s minimum TTL — we bump
// anything below that.
func (p *Provider) AdjustEndpoints(eps []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	for i, ep := range eps {
		if ep == nil {
			continue
		}
		name := canonicalDNSName(ep.DNSName)
		if !p.nameInScope(name) {
			continue
		}
		if err := p.adjustEndpoint(ep, name); err != nil {
			return nil, validationErrorf("adjust[%d]: %v", i, err)
		}
	}
	return eps, nil
}

// ----- internals -----

func (p *Provider) cachedRetrieve(ctx context.Context) ([]porkbun.Record, error) {
	if recs, ok := p.cache.get(); ok {
		return recs, nil
	}
	recs, err := p.client.Retrieve(ctx, p.domain)
	if err != nil {
		return nil, err
	}
	p.cache.set(recs)
	return recs, nil
}

func (p *Provider) inFilter(name string) bool {
	return matchesExplicitFilters(p.include, canonicalDNSName(name))
}

// createEndpoint creates one Porkbun record per target.
func (p *Provider) createEndpoint(ctx context.Context, ep *endpoint.Endpoint, idx *index) error {
	for _, target := range ep.Targets {
		if err := p.convergeTarget(ctx, ep, target, idx, "create", "edit-on-create"); err != nil {
			return err
		}
	}
	return nil
}

// convergeTarget ensures one exact Porkbun record exists for an endpoint
// target. It makes Create replay-safe against TTL or CNAME/ALIAS drift and is
// shared with Update so duplicate cleanup has one implementation.
func (p *Provider) convergeTarget(
	ctx context.Context,
	ep *endpoint.Endpoint,
	target string,
	idx *index,
	createAction string,
	editAction string,
) error {
	in, err := p.recordInput(ep, target)
	if err != nil {
		return err
	}
	ids := idx.match(ep.DNSName, ep.RecordType, target)
	if len(ids) == 0 {
		log.WithFields(log.Fields{
			"action": createAction, "name": ep.DNSName, "type": in.Type,
			"target": target, "ttl": in.TTL, "dry_run": p.dryRun,
		}).Info("apply")
		if p.dryRun {
			return idx.replace(dryRunRecordID(ep, target), ep.DNSName, in)
		}
		id, err := p.client.Create(ctx, p.domain, in)
		if err != nil {
			return err
		}
		return idx.replace(id, ep.DNSName, in)
	}

	keeper := preferredRecordID(ids, idx, in)
	if recordMatchesInput(idx.byID[keeper], in) {
		log.WithFields(log.Fields{
			"action": "skip-existing", "name": ep.DNSName, "type": in.Type,
			"target": target, "id": keeper, "dry_run": p.dryRun,
		}).Info("apply")
	} else {
		log.WithFields(log.Fields{
			"action": editAction, "name": ep.DNSName, "type": in.Type,
			"target": target, "id": keeper, "ttl": in.TTL, "dry_run": p.dryRun,
		}).Info("apply")
		if !p.dryRun {
			if err := p.client.Edit(ctx, p.domain, keeper, in); err != nil {
				return err
			}
		}
		if err := idx.replace(keeper, ep.DNSName, in); err != nil {
			return err
		}
	}

	for _, id := range ids {
		if id == keeper {
			continue
		}
		if err := p.deleteIndexedRecord(ctx, idx, id, "delete-duplicate"); err != nil {
			return err
		}
	}
	return nil
}

func dryRunRecordID(ep *endpoint.Endpoint, target string) string {
	return "dry-run:create:" + ep.DNSName + "|" + ep.RecordType + "|" + target
}

func (p *Provider) nameInScope(name string) bool {
	return withinZone(name, p.domain) && p.inFilter(name)
}

func (p *Provider) scopedRecords(records []porkbun.Record) []porkbun.Record {
	scoped := make([]porkbun.Record, 0, len(records))
	for _, record := range records {
		if p.nameInScope(canonicalDNSName(record.Name)) {
			scoped = append(scoped, record)
		}
	}
	return scoped
}

// deleteEndpoint deletes all Porkbun records matching name+type+target.
func (p *Provider) deleteEndpoint(ctx context.Context, ep *endpoint.Endpoint, idx *index) error {
	seen := make(map[string]struct{}, len(ep.Targets))
	for _, target := range ep.Targets {
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		ids := idx.match(ep.DNSName, ep.RecordType, target)
		if len(ids) == 0 {
			log.WithFields(log.Fields{"name": ep.DNSName, "type": ep.RecordType, "target": target}).
				Warn("delete requested but no matching record found; skipping")
			continue
		}
		for _, id := range ids {
			log.WithFields(log.Fields{
				"action":  "delete",
				"name":    ep.DNSName,
				"type":    ep.RecordType,
				"target":  target,
				"id":      id,
				"dry_run": p.dryRun,
			}).Info("apply")
			if !p.dryRun {
				if err := p.client.Delete(ctx, p.domain, id); err != nil {
					return err
				}
			}
			idx.remove(id)
		}
	}
	return nil
}

func (p *Provider) deleteIndexedRecord(ctx context.Context, idx *index, id, action string) error {
	r, ok := idx.byID[id]
	if !ok {
		return nil
	}
	_, target, err := recordToEndpointTarget(r)
	if err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"action": action, "name": canonicalDNSName(r.Name), "type": r.Type,
		"target": target, "id": id, "dry_run": p.dryRun,
	}).Info("apply")
	if !p.dryRun {
		if err := p.client.Delete(ctx, p.domain, id); err != nil {
			return err
		}
	}
	idx.remove(id)
	return nil
}

// updateEndpoint converges each external target to exactly one Porkbun record.
// This both handles target additions/removals and repairs duplicate records. A
// matching record with the desired TTL is preferred as the keeper.
func (p *Provider) updateEndpoint(ctx context.Context, oldEP, newEP *endpoint.Endpoint, idx *index) error {
	var oldTargetList []string
	if oldEP != nil {
		oldTargetList = oldEP.Targets
	}
	oldTargets := uniqueStrings(oldTargetList)
	newTargets := uniqueStrings(newEP.Targets)

	for _, target := range sortedMapKeys(oldTargets) {
		if _, keep := newTargets[target]; keep {
			continue
		}
		for _, id := range idx.match(newEP.DNSName, newEP.RecordType, target) {
			if err := p.deleteIndexedRecord(ctx, idx, id, "delete-on-update"); err != nil {
				return err
			}
		}
	}

	for _, target := range sortedMapKeys(newTargets) {
		if err := p.convergeTarget(ctx, newEP, target, idx, "create-on-update", "edit-on-update"); err != nil {
			return err
		}
	}
	return nil
}

// subdomainOf turns "argocd.k3s.example.com" into "argocd.k3s" for zone "example.com".
// Returns "" for the root.
func (p *Provider) subdomainOf(fqdn string) string {
	fqdn = canonicalDNSName(fqdn)
	if fqdn == p.domain {
		return ""
	}
	if strings.HasSuffix(fqdn, "."+p.domain) {
		return strings.TrimSuffix(fqdn, "."+p.domain)
	}
	// ApplyChanges validates scope before any API call; keep this defensive
	// fallback for package-internal callers.
	return fqdn
}

// managedType returns true for record types we'll round-trip.
// ALIAS is exposed to ExternalDNS as CNAME and mapped back on writes.
func managedType(t string) bool {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "A", "AAAA", "MX", "CNAME", "ALIAS", "TXT", "NS", "SRV", "TLSA", "CAA", "SSHFP", "HTTPS", "SVCB":
		return true
	}
	return false
}

func managedEndpointType(t string) bool {
	return managedType(t) && strings.ToUpper(strings.TrimSpace(t)) != "ALIAS"
}

// normaliseContent strips quoting Porkbun adds for TXT records so external-dns
// sees the raw text.
func normaliseContent(recType, content string) string {
	if strings.ToUpper(recType) == "TXT" {
		// Porkbun stores TXT as-is (no quoting), but some upstream tools
		// double-quote. Unwrap a single layer of "..." if present.
		if len(content) >= 2 && content[0] == '"' && content[len(content)-1] == '"' {
			content = content[1 : len(content)-1]
		}
	}
	return content
}

// serialiseContent prepares an endpoint target for sending to Porkbun.
func serialiseContent(recType, target string) string {
	if strings.ToUpper(recType) == "TXT" {
		// external-dns sometimes wraps TXT in quotes; Porkbun wants raw.
		if len(target) >= 2 && target[0] == '"' && target[len(target)-1] == '"' {
			return target[1 : len(target)-1]
		}
		return target
	}
	return target
}

// ttlString converts an endpoint TTL (int seconds, may be 0/unset) to Porkbun's string field.
func ttlString(ttl endpoint.TTL) string {
	return strconv.FormatInt(effectiveTTL(ttl), 10)
}

// ----- record indexing -----

type recordKey struct {
	name, recType, target string
}

type index struct {
	byKey   map[recordKey][]string
	byID    map[string]porkbun.Record
	keyByID map[string]recordKey
}

func indexRecords(records []porkbun.Record) (index, error) {
	idx := index{
		byKey:   map[recordKey][]string{},
		byID:    map[string]porkbun.Record{},
		keyByID: map[string]recordKey{},
	}
	for _, r := range records {
		if !managedType(r.Type) {
			continue
		}
		name := canonicalDNSName(r.Name)
		if err := validateDNSName(name, true); err != nil {
			return index{}, fmt.Errorf("record id=%s has invalid name %q: %w", r.ID, r.Name, err)
		}
		viewType, target, err := recordToEndpointTarget(r)
		if err != nil {
			return index{}, fmt.Errorf("record id=%s: %w", r.ID, err)
		}
		k := recordKey{name: name, recType: viewType, target: target}
		idx.byKey[k] = append(idx.byKey[k], r.ID)
		idx.byID[r.ID] = r
		idx.keyByID[r.ID] = k
	}
	for k := range idx.byKey {
		sort.Strings(idx.byKey[k])
	}
	return idx, nil
}

func (i *index) match(name, recType, target string) []string {
	k := recordKey{canonicalDNSName(name), strings.ToUpper(strings.TrimSpace(recType)), target}
	return append([]string(nil), i.byKey[k]...)
}

func (i *index) remove(id string) {
	k, ok := i.keyByID[id]
	if !ok {
		return
	}
	ids := i.byKey[k]
	for pos, candidate := range ids {
		if candidate == id {
			ids = append(ids[:pos], ids[pos+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(i.byKey, k)
	} else {
		i.byKey[k] = ids
	}
	delete(i.byID, id)
	delete(i.keyByID, id)
}

func (i *index) replace(id, name string, in porkbun.RecordInput) error {
	i.remove(id)
	r := porkbun.Record{
		ID: id, Name: canonicalDNSName(name), Type: in.Type,
		Content: in.Content, TTL: in.TTL, Prio: in.Prio,
	}
	viewType, target, err := recordToEndpointTarget(r)
	if err != nil {
		return err
	}
	k := recordKey{name: r.Name, recType: viewType, target: target}
	i.byKey[k] = append(i.byKey[k], id)
	sort.Strings(i.byKey[k])
	i.byID[id] = r
	i.keyByID[id] = k
	return nil
}

// ----- caching -----

type recordCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	records []porkbun.Record
	expires time.Time
}

func newRecordCache(ttl time.Duration) *recordCache { return &recordCache{ttl: ttl} }

func (c *recordCache) get() ([]porkbun.Record, bool) {
	if c == nil || c.ttl == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().After(c.expires) {
		return nil, false
	}
	return c.records, true
}

func (c *recordCache) set(records []porkbun.Record) {
	if c == nil || c.ttl == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = records
	c.expires = time.Now().Add(c.ttl)
}

func (c *recordCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expires = time.Time{}
}
