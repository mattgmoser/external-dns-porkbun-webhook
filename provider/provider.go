// Package provider implements the external-dns Provider interface against the
// Porkbun DNS API.
//
// Mapping:
//
//	Porkbun Record.Name (FQDN) ↔ external-dns endpoint.DNSName (FQDN)
//	Porkbun Record.Content     ↔ endpoint.Targets[0]   (one record per target)
//	Porkbun Record.Type        ↔ endpoint.RecordType
//	Porkbun Record.TTL         ↔ endpoint.RecordTTL
//	Porkbun Record.Prio        ↔ endpoint.SetIdentifier (for MX)
//
// Porkbun stores records per domain. Multiple records with the same name+type
// but different content represent multi-target endpoints (e.g. round-robin A).
// We collapse these into one external-dns endpoint with multiple Targets, and
// expand back when applying changes.
package provider

import (
	"context"
	"fmt"
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
	cache        *recordCache
	dryRun       bool
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

// New constructs a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" || cfg.SecretAPIKey == "" {
		return nil, fmt.Errorf("api credentials required")
	}
	if cfg.Domain == "" {
		return nil, fmt.Errorf("domain required")
	}
	if cfg.DomainFilter != nil && cfg.DomainFilter.IsConfigured() {
		// caller-supplied filter must be a subset of the zone
		ok := false
		for _, f := range cfg.DomainFilter.Filters {
			if strings.HasSuffix(strings.TrimPrefix(f, "."), cfg.Domain) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("domain filter %v is not within zone %q", cfg.DomainFilter.Filters, cfg.Domain)
		}
	} else {
		cfg.DomainFilter = endpoint.NewDomainFilter([]string{cfg.Domain})
	}
	c := porkbun.New(cfg.APIKey, cfg.SecretAPIKey)
	return &Provider{
		client:       c,
		domain:       cfg.Domain,
		domainFilter: cfg.DomainFilter,
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

	// Collapse same name+type into one endpoint with multiple targets.
	type key struct{ name, recType string }
	grouped := map[key]*endpoint.Endpoint{}
	for _, r := range records {
		if !p.inFilter(r.Name) {
			continue
		}
		if !managedType(r.Type) {
			continue
		}
		k := key{r.Name, r.Type}
		ep, ok := grouped[k]
		if !ok {
			ep = &endpoint.Endpoint{
				DNSName:    r.Name,
				RecordType: r.Type,
				RecordTTL:  endpoint.TTL(r.TTLInt()),
			}
			grouped[k] = ep
		}
		ep.Targets = append(ep.Targets, normaliseContent(r.Type, r.Content))
	}

	out := make([]*endpoint.Endpoint, 0, len(grouped))
	for _, ep := range grouped {
		out = append(out, ep)
	}
	return out, nil
}

// ApplyChanges applies the given changes to Porkbun.
//
// external-dns batches changes as Create + UpdateOld+UpdateNew + Delete. For
// idempotence and safety we re-fetch before each apply to catch drift.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	current, err := p.client.Retrieve(ctx, p.domain)
	if err != nil {
		return fmt.Errorf("retrieve before apply: %w", err)
	}
	p.cache.invalidate()

	idx := indexRecords(current)

	// Order matters: Delete first (frees up name+content collisions), Create+Update next.
	for _, ep := range changes.Delete {
		if err := p.deleteEndpoint(ctx, ep, idx); err != nil {
			return fmt.Errorf("delete %s/%s: %w", ep.DNSName, ep.RecordType, err)
		}
	}

	// Updates: external-dns sends UpdateOld + UpdateNew with same length and matching positions.
	for i, newEP := range changes.UpdateNew {
		var oldEP *endpoint.Endpoint
		if i < len(changes.UpdateOld) {
			oldEP = changes.UpdateOld[i]
		}
		if err := p.updateEndpoint(ctx, oldEP, newEP, idx); err != nil {
			return fmt.Errorf("update %s/%s: %w", newEP.DNSName, newEP.RecordType, err)
		}
	}

	for _, ep := range changes.Create {
		if err := p.createEndpoint(ctx, ep); err != nil {
			return fmt.Errorf("create %s/%s: %w", ep.DNSName, ep.RecordType, err)
		}
	}
	p.cache.invalidate()
	return nil
}

// AdjustEndpoints lets a provider tweak desired endpoints before they're stored.
// Most providers no-op this, but Porkbun has a 600s minimum TTL — we bump
// anything below that.
func (p *Provider) AdjustEndpoints(eps []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	for _, ep := range eps {
		if ep.RecordTTL > 0 && ep.RecordTTL < 600 {
			ep.RecordTTL = 600
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
	if p.domainFilter == nil || !p.domainFilter.IsConfigured() {
		return true
	}
	return p.domainFilter.Match(name)
}

// createEndpoint creates one Porkbun record per target.
func (p *Provider) createEndpoint(ctx context.Context, ep *endpoint.Endpoint) error {
	for _, target := range ep.Targets {
		in := porkbun.RecordInput{
			Name:    p.subdomainOf(ep.DNSName),
			Type:    ep.RecordType,
			Content: serialiseContent(ep.RecordType, target),
			TTL:     ttlString(ep.RecordTTL),
		}
		log.WithFields(log.Fields{
			"action":  "create",
			"name":    ep.DNSName,
			"type":    ep.RecordType,
			"target":  target,
			"ttl":     in.TTL,
			"dry_run": p.dryRun,
		}).Info("apply")
		if p.dryRun {
			continue
		}
		if _, err := p.client.Create(ctx, p.domain, in); err != nil {
			return err
		}
	}
	return nil
}

// deleteEndpoint deletes all Porkbun records matching name+type+target.
func (p *Provider) deleteEndpoint(ctx context.Context, ep *endpoint.Endpoint, idx index) error {
	for _, t := range ep.Targets {
		serialised := serialiseContent(ep.RecordType, t)
		ids := idx.matchByContent(ep.DNSName, ep.RecordType, serialised)
		if len(ids) == 0 {
			log.WithFields(log.Fields{"name": ep.DNSName, "type": ep.RecordType, "target": t}).
				Warn("delete requested but no matching record found; skipping")
			continue
		}
		for _, id := range ids {
			log.WithFields(log.Fields{
				"action":  "delete",
				"name":    ep.DNSName,
				"type":    ep.RecordType,
				"target":  t,
				"id":      id,
				"dry_run": p.dryRun,
			}).Info("apply")
			if p.dryRun {
				continue
			}
			if err := p.client.Delete(ctx, p.domain, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateEndpoint deletes outgoing targets and creates incoming ones.
// We don't use Edit because external-dns may add or remove targets, not just rotate them.
func (p *Provider) updateEndpoint(ctx context.Context, oldEP, newEP *endpoint.Endpoint, idx index) error {
	oldTargets := map[string]bool{}
	if oldEP != nil {
		for _, t := range oldEP.Targets {
			oldTargets[serialiseContent(oldEP.RecordType, t)] = true
		}
	}
	newTargets := map[string]bool{}
	for _, t := range newEP.Targets {
		newTargets[serialiseContent(newEP.RecordType, t)] = true
	}

	// Delete targets that are in old but not in new.
	for content := range oldTargets {
		if newTargets[content] {
			continue
		}
		ids := idx.matchByContent(newEP.DNSName, newEP.RecordType, content)
		for _, id := range ids {
			log.WithFields(log.Fields{
				"action": "delete-on-update", "name": newEP.DNSName, "type": newEP.RecordType,
				"content": content, "id": id, "dry_run": p.dryRun,
			}).Info("apply")
			if p.dryRun {
				continue
			}
			if err := p.client.Delete(ctx, p.domain, id); err != nil {
				return err
			}
		}
	}
	// Create targets that are in new but not in old, OR records whose TTL changed.
	for content := range newTargets {
		// If old has this content with the same TTL, leave it alone.
		if oldTargets[content] {
			matchedSame := false
			for _, id := range idx.matchByContent(newEP.DNSName, newEP.RecordType, content) {
				if recTTL(idx, id) == int(newEP.RecordTTL) {
					matchedSame = true
					break
				}
			}
			if matchedSame {
				continue
			}
			// TTL drift: edit instead of recreate
			for _, id := range idx.matchByContent(newEP.DNSName, newEP.RecordType, content) {
				log.WithFields(log.Fields{
					"action": "edit-ttl", "name": newEP.DNSName, "type": newEP.RecordType,
					"content": content, "id": id, "ttl": newEP.RecordTTL, "dry_run": p.dryRun,
				}).Info("apply")
				if p.dryRun {
					continue
				}
				if err := p.client.Edit(ctx, p.domain, id, porkbun.RecordInput{
					Name:    p.subdomainOf(newEP.DNSName),
					Type:    newEP.RecordType,
					Content: content,
					TTL:     ttlString(newEP.RecordTTL),
				}); err != nil {
					return err
				}
			}
			continue
		}
		// New target → create
		log.WithFields(log.Fields{
			"action": "create-on-update", "name": newEP.DNSName, "type": newEP.RecordType,
			"content": content, "ttl": newEP.RecordTTL, "dry_run": p.dryRun,
		}).Info("apply")
		if p.dryRun {
			continue
		}
		if _, err := p.client.Create(ctx, p.domain, porkbun.RecordInput{
			Name:    p.subdomainOf(newEP.DNSName),
			Type:    newEP.RecordType,
			Content: content,
			TTL:     ttlString(newEP.RecordTTL),
		}); err != nil {
			return err
		}
	}
	return nil
}

// subdomainOf turns "argocd.k3s.example.com" into "argocd.k3s" for zone "example.com".
// Returns "" for the root.
func (p *Provider) subdomainOf(fqdn string) string {
	fqdn = strings.TrimSuffix(fqdn, ".")
	if fqdn == p.domain {
		return ""
	}
	if strings.HasSuffix(fqdn, "."+p.domain) {
		return strings.TrimSuffix(fqdn, "."+p.domain)
	}
	// Defensive: return the FQDN as-is (Porkbun will reject if outside zone).
	return fqdn
}

// recTTL fetches the TTL from the index for an ID, or 0 if not found.
func recTTL(idx index, id string) int {
	if r, ok := idx.byID[id]; ok {
		return r.TTLInt()
	}
	return 0
}

// managedType returns true for record types we'll round-trip.
// (Skip NS/SOA/etc.; external-dns shouldn't touch those.)
func managedType(t string) bool {
	switch strings.ToUpper(t) {
	case "A", "AAAA", "CNAME", "TXT", "MX", "SRV", "CAA":
		return true
	}
	return false
}

// normaliseContent strips quoting Porkbun adds for TXT records so external-dns
// sees the raw text.
func normaliseContent(recType, content string) string {
	if strings.ToUpper(recType) == "TXT" {
		// Porkbun stores TXT as-is (no quoting), but some upstream tools
		// double-quote. Unwrap a single layer of "..." if present.
		content = strings.TrimSpace(content)
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
		t := strings.TrimSpace(target)
		if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
			t = t[1 : len(t)-1]
		}
		return t
	}
	return target
}

// ttlString converts an endpoint TTL (int seconds, may be 0/unset) to Porkbun's string field.
func ttlString(ttl endpoint.TTL) string {
	v := int(ttl)
	if v <= 0 {
		v = 600 // Porkbun's effective minimum
	}
	return strconv.Itoa(v)
}

// ----- record indexing -----

type index struct {
	// keyed by name|type|content for exact target matching
	byKey map[string][]string // key → []id
	byID  map[string]porkbun.Record
}

func indexRecords(records []porkbun.Record) index {
	idx := index{
		byKey: map[string][]string{},
		byID:  map[string]porkbun.Record{},
	}
	for _, r := range records {
		k := r.Name + "|" + r.Type + "|" + normaliseContent(r.Type, r.Content)
		idx.byKey[k] = append(idx.byKey[k], r.ID)
		idx.byID[r.ID] = r
	}
	return idx
}

func (i index) matchByContent(name, recType, content string) []string {
	k := name + "|" + recType + "|" + content
	return i.byKey[k]
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
