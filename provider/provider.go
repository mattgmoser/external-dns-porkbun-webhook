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
	"errors"
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
	client                   *porkbun.Client
	domain                   string                 // Porkbun zone, e.g. "example.com"
	domainFilter             *endpoint.DomainFilter // optional further-restricted scope
	include                  []string               // validated explicit include filters
	cache                    *recordCache
	dryRun                   bool
	operationMu              sync.Mutex // keeps Records from observing an in-flight multi-call mutation
	cleanupMu                sync.Mutex
	pendingOwnershipCleanups map[string]pendingOwnershipCleanup
	pendingOwnershipRepairs  map[string]pendingOwnershipRepair
}

type pendingOwnershipCleanup struct {
	ownership *endpoint.Endpoint
	protected *endpoint.Endpoint
}

type pendingOwnershipRepair struct {
	old       *endpoint.Endpoint
	new       *endpoint.Endpoint
	protected *endpoint.Endpoint
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
		client:                   c,
		domain:                   cfg.Domain,
		domainFilter:             cfg.DomainFilter,
		include:                  include,
		dryRun:                   cfg.DryRun,
		cache:                    newRecordCache(cfg.CacheTTL),
		pendingOwnershipCleanups: make(map[string]pendingOwnershipCleanup),
		pendingOwnershipRepairs:  make(map[string]pendingOwnershipRepair),
	}, nil
}

// Ping verifies credentials work. Call once at startup.
func (p *Provider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// GetDomainFilter returns the domain filter so external-dns can pre-filter.
func (p *Provider) GetDomainFilter() *endpoint.DomainFilter { return p.domainFilter }

// Records returns the current state of the world (the records we manage).
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()

	if err := p.drainPendingOwnershipCleanups(ctx); err != nil {
		return nil, fmt.Errorf("reconcile pending ownership cleanup: %w", err)
	}
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
	p.operationMu.Lock()
	defer p.operationMu.Unlock()

	if err := p.validateChanges(changes); err != nil {
		return err
	}
	if err := p.drainPendingOwnershipCleanups(ctx); err != nil {
		return fmt.Errorf("reconcile pending ownership cleanup before apply: %w", err)
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

	// ExternalDNS v0.21 sends registry deletes as [primaries..., ownership
	// TXTs...]. Process each logical pair together so a deadline cannot delete
	// many primaries before reaching their ownership records. If either half is
	// ambiguous, the pending-cleanup queue makes the next Records call either
	// expose the still-owned primary or finish removing the now-orphan TXT.
	deletePairs, pairedDeletes, err := registryDeletePairs(changes.Delete)
	if err != nil {
		return validationErrorf("delete layout: %v", err)
	}
	if pairedDeletes {
		for _, pair := range deletePairs {
			if err := p.deleteEndpoint(ctx, pair.primary, &idx); err != nil {
				return fmt.Errorf("delete %s/%s: %w", pair.primary.DNSName, pair.primary.RecordType,
					p.reconcileOwnershipDeleteFailure(ctx, pair, err))
			}
			if err := p.deleteEndpoint(ctx, pair.ownership, &idx); err != nil {
				return fmt.Errorf("delete %s/%s: %w", pair.ownership.DNSName, pair.ownership.RecordType,
					p.reconcileOwnershipDeleteFailure(ctx, pair, err))
			}
		}
	} else {
		// Provider-only callers do not necessarily use the TXT registry.
		for _, ep := range changes.Delete {
			if err := p.deleteEndpoint(ctx, ep, &idx); err != nil {
				return fmt.Errorf("delete %s/%s: %w", ep.DNSName, ep.RecordType, err)
			}
		}
	}

	// ExternalDNS's TXT registry appends ownership updates in the same paired
	// layout as deletes. Converge each ownership record before its primary so a
	// failed primary mutation never becomes unowned.
	updatePairs, pairedUpdates, err := registryUpdatePairs(changes.UpdateOld, changes.UpdateNew)
	if err != nil {
		return validationErrorf("update layout: %v", err)
	}
	if pairedUpdates {
		for _, pair := range updatePairs {
			if err := p.updateOwnershipEndpoint(ctx, pair.ownershipOld, pair.ownershipNew, &idx); err != nil {
				p.enqueueOwnershipRepair(pendingOwnershipRepair{
					old: pair.ownershipOld, new: pair.ownershipNew, protected: pair.primaryNew,
				})
				if repairErr := p.drainPendingOwnershipCleanups(ctx); repairErr != nil {
					err = errors.Join(err, fmt.Errorf("reconcile generated ownership TXT update: %w", repairErr))
				}
				return fmt.Errorf("update %s/%s: %w", pair.ownershipNew.DNSName, pair.ownershipNew.RecordType, err)
			}
			if err := p.updateEndpoint(ctx, pair.primaryOld, pair.primaryNew, &idx); err != nil {
				p.enqueueOwnershipCleanup(pendingOwnershipCleanup{
					ownership: pair.ownershipNew,
					protected: pair.primaryNew,
				})
				if cleanupErr := p.drainPendingOwnershipCleanups(ctx); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("reconcile generated ownership TXT: %w", cleanupErr))
				}
				return fmt.Errorf("update %s/%s: %w", pair.primaryNew.DNSName, pair.primaryNew.RecordType, err)
			}
		}
	} else {
		for i, newEP := range changes.UpdateNew {
			if err := p.updateEndpoint(ctx, changes.UpdateOld[i], newEP, &idx); err != nil {
				return fmt.Errorf("update %s/%s: %w", newEP.DNSName, newEP.RecordType, err)
			}
		}
	}

	// ExternalDNS's TXT registry appends ownership records after the records
	// they protect. Reverse that dependency before writing so an ambiguous main
	// create can never leave an unowned record. If a later create fails, newly
	// created ownership records are conditionally rolled back only after a fresh
	// retrieve confirms that no protected primary exists.
	createdOwnership := make([]pendingOwnershipCleanup, 0)
	for _, ep := range ownershipFirstCreates(changes.Create) {
		createdTargets, err := p.createEndpoint(ctx, ep, &idx)
		if isRegistryOwnershipTXT(ep) {
			protected := protectedEndpointForOwnership(ep, changes.Create)
			for _, target := range createdTargets {
				ownership := ep.DeepCopy()
				ownership.Targets = endpoint.Targets{target}
				createdOwnership = append(createdOwnership, pendingOwnershipCleanup{
					ownership: ownership,
					protected: protected.DeepCopy(),
				})
			}
		}
		if err != nil {
			cleanupErr := p.rollbackCreatedOwnership(ctx, createdOwnership)
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback generated ownership TXT records: %w", cleanupErr))
			}
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

type registryDeletePair struct {
	primary   *endpoint.Endpoint
	ownership *endpoint.Endpoint
}

type registryUpdatePair struct {
	primaryOld   *endpoint.Endpoint
	primaryNew   *endpoint.Endpoint
	ownershipOld *endpoint.Endpoint
	ownershipNew *endpoint.Endpoint
}

// validateRegistryCreateLayout verifies the partial one-to-one layout used by
// v0.21 creates: generated ownership records are a suffix, but the suffix can
// be shorter than the primary prefix when an ownership TXT already exists.
func validateRegistryCreateLayout(creates []*endpoint.Endpoint) error {
	ownershipStart := -1
	for i, ep := range creates {
		if isRegistryOwnershipTXT(ep) {
			ownershipStart = i
			break
		}
	}
	if ownershipStart == -1 {
		return nil
	}
	if ownershipStart == 0 {
		return fmt.Errorf("generated ownership records must follow primary creates")
	}
	if len(creates)-ownershipStart > ownershipStart {
		return fmt.Errorf("generated ownership records cannot outnumber primary creates")
	}

	primaryNames := make(map[string]struct{}, ownershipStart)
	for _, primary := range creates[:ownershipStart] {
		if isRegistryOwnershipTXT(primary) {
			return fmt.Errorf("generated ownership records must follow all primary creates")
		}
		primaryNames[primary.DNSName] = struct{}{}
	}
	for i, ownership := range creates[ownershipStart:] {
		if !isRegistryOwnershipTXT(ownership) {
			return fmt.Errorf("primary create[%d] follows generated ownership records", ownershipStart+i)
		}
		if len(ownership.Targets) != 1 {
			return fmt.Errorf("ownership create[%d] must contain exactly one target", ownershipStart+i)
		}
		ownedName := canonicalDNSName(ownership.Labels[endpoint.OwnedRecordLabelKey])
		if _, ok := primaryNames[ownedName]; !ok {
			return fmt.Errorf("ownership create[%d] protects %q without a matching primary", ownershipStart+i, ownedName)
		}
	}
	if len(creates)-ownershipStart == ownershipStart {
		for i, ownership := range creates[ownershipStart:] {
			ownedName := canonicalDNSName(ownership.Labels[endpoint.OwnedRecordLabelKey])
			if ownedName != creates[i].DNSName {
				return fmt.Errorf("ownership create[%d] protects %q, want %q", ownershipStart+i, ownedName, creates[i].DNSName)
			}
		}
	}
	return nil
}

// registryDeletePairs recognizes the exact layout emitted by ExternalDNS's
// v0.21 TXT registry. A batch with no generated ownership records is left in
// provider-native order. Once a generated ownership record is present, the
// entire layout must be unambiguous before any Porkbun request is made.
func registryDeletePairs(deletes []*endpoint.Endpoint) ([]registryDeletePair, bool, error) {
	ownershipStart := -1
	for i, ep := range deletes {
		if isRegistryOwnershipTXT(ep) {
			ownershipStart = i
			break
		}
	}
	if ownershipStart == -1 {
		return nil, false, nil
	}
	if ownershipStart == 0 || len(deletes) != 2*ownershipStart {
		return nil, false, fmt.Errorf("generated ownership records must be a one-to-one suffix for primary deletes")
	}

	pairs := make([]registryDeletePair, ownershipStart)
	for i := 0; i < ownershipStart; i++ {
		primary, ownership := deletes[i], deletes[ownershipStart+i]
		if isRegistryOwnershipTXT(primary) || !isRegistryOwnershipTXT(ownership) {
			return nil, false, fmt.Errorf("generated ownership records must follow all primary deletes")
		}
		if len(ownership.Targets) != 1 {
			return nil, false, fmt.Errorf("ownership delete[%d] must contain exactly one target", ownershipStart+i)
		}
		ownedName := canonicalDNSName(ownership.Labels[endpoint.OwnedRecordLabelKey])
		if ownedName == "" || ownedName != primary.DNSName {
			return nil, false, fmt.Errorf("ownership delete[%d] protects %q, want %q", ownershipStart+i, ownedName, primary.DNSName)
		}
		pairs[i] = registryDeletePair{primary: primary, ownership: ownership}
	}
	return pairs, true, nil
}

// registryUpdatePairs recognizes the one-to-one ownership suffix appended to
// both update slices by ExternalDNS's v0.21 TXT registry.
func registryUpdatePairs(oldEndpoints, newEndpoints []*endpoint.Endpoint) ([]registryUpdatePair, bool, error) {
	oldStart, newStart := -1, -1
	for i, ep := range oldEndpoints {
		if isRegistryOwnershipTXT(ep) {
			oldStart = i
			break
		}
	}
	for i, ep := range newEndpoints {
		if isRegistryOwnershipTXT(ep) {
			newStart = i
			break
		}
	}
	if oldStart == -1 && newStart == -1 {
		return nil, false, nil
	}
	if oldStart <= 0 || newStart != oldStart || len(oldEndpoints) != 2*oldStart || len(newEndpoints) != 2*newStart {
		return nil, false, fmt.Errorf("generated ownership records must be matching one-to-one suffixes for primary updates")
	}

	pairs := make([]registryUpdatePair, oldStart)
	for i := 0; i < oldStart; i++ {
		primaryOld, primaryNew := oldEndpoints[i], newEndpoints[i]
		ownershipOld, ownershipNew := oldEndpoints[oldStart+i], newEndpoints[newStart+i]
		if isRegistryOwnershipTXT(primaryOld) || isRegistryOwnershipTXT(primaryNew) ||
			!isRegistryOwnershipTXT(ownershipOld) || !isRegistryOwnershipTXT(ownershipNew) {
			return nil, false, fmt.Errorf("generated ownership records must follow all primary updates")
		}
		if len(ownershipOld.Targets) != 1 || len(ownershipNew.Targets) != 1 {
			return nil, false, fmt.Errorf("ownership update[%d] must contain exactly one old and new target", oldStart+i)
		}
		oldOwnedName := canonicalDNSName(ownershipOld.Labels[endpoint.OwnedRecordLabelKey])
		newOwnedName := canonicalDNSName(ownershipNew.Labels[endpoint.OwnedRecordLabelKey])
		if oldOwnedName != primaryOld.DNSName || newOwnedName != primaryNew.DNSName {
			return nil, false, fmt.Errorf("ownership update[%d] does not protect its paired primary", oldStart+i)
		}
		pairs[i] = registryUpdatePair{
			primaryOld: primaryOld, primaryNew: primaryNew,
			ownershipOld: ownershipOld, ownershipNew: ownershipNew,
		}
	}
	return pairs, true, nil
}

// protectedEndpointForOwnership returns the most specific primary identity the
// provider can infer without knowing the TXT registry's configurable mapper.
// A complete v0.21 ownership suffix is positionally paired with the primary
// prefix, which disambiguates same-name records of different types. A partial
// suffix can omit already-existing markers; when its matching primary type is
// genuinely ambiguous, an empty type deliberately makes cleanup conservative.
func protectedEndpointForOwnership(ownership *endpoint.Endpoint, creates []*endpoint.Endpoint) *endpoint.Endpoint {
	protected := &endpoint.Endpoint{DNSName: canonicalDNSName(ownership.Labels[endpoint.OwnedRecordLabelKey])}
	ownershipStart := -1
	for i, candidate := range creates {
		if isRegistryOwnershipTXT(candidate) {
			ownershipStart = i
			break
		}
	}
	if ownershipStart > 0 && len(creates)-ownershipStart == ownershipStart {
		for i, candidate := range creates[ownershipStart:] {
			if candidate == ownership && creates[i].DNSName == protected.DNSName {
				protected.RecordType = creates[i].RecordType
				return protected
			}
		}
	}
	for _, candidate := range creates {
		if candidate == nil || isRegistryOwnershipTXT(candidate) || candidate.DNSName != protected.DNSName {
			continue
		}
		if protected.RecordType == "" {
			protected.RecordType = candidate.RecordType
			continue
		}
		if protected.RecordType != candidate.RecordType {
			protected.RecordType = ""
			break
		}
	}
	return protected
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

func ownershipCleanupKey(cleanup pendingOwnershipCleanup) string {
	return strings.Join([]string{
		cleanup.ownership.DNSName,
		cleanup.ownership.RecordType,
		strings.Join(cleanup.ownership.Targets, "\x1e"),
		cleanup.protected.DNSName,
		cleanup.protected.RecordType,
	}, "\x1f")
}

func ownershipRepairKey(repair pendingOwnershipRepair) string {
	return strings.Join([]string{
		repair.old.DNSName,
		repair.old.RecordType,
		strings.Join(repair.old.Targets, "\x1e"),
		repair.new.DNSName,
		repair.new.RecordType,
		strings.Join(repair.new.Targets, "\x1e"),
		repair.protected.DNSName,
		repair.protected.RecordType,
	}, "\x1f")
}

func (p *Provider) enqueueOwnershipCleanup(cleanup pendingOwnershipCleanup) {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()

	if p.pendingOwnershipCleanups == nil {
		p.pendingOwnershipCleanups = make(map[string]pendingOwnershipCleanup)
	}
	cleanup.ownership = cleanup.ownership.DeepCopy()
	cleanup.protected = cleanup.protected.DeepCopy()
	p.pendingOwnershipCleanups[ownershipCleanupKey(cleanup)] = cleanup
}

func (p *Provider) enqueueOwnershipRepair(repair pendingOwnershipRepair) {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()

	if p.pendingOwnershipRepairs == nil {
		p.pendingOwnershipRepairs = make(map[string]pendingOwnershipRepair)
	}
	repair.old = repair.old.DeepCopy()
	repair.new = repair.new.DeepCopy()
	repair.protected = repair.protected.DeepCopy()
	p.pendingOwnershipRepairs[ownershipRepairKey(repair)] = repair
}

func (p *Provider) reconcileOwnershipDeleteFailure(ctx context.Context, pair registryDeletePair, applyErr error) error {
	p.enqueueOwnershipCleanup(pendingOwnershipCleanup{
		ownership: pair.ownership,
		protected: pair.primary,
	})
	if cleanupErr := p.drainPendingOwnershipCleanups(ctx); cleanupErr != nil {
		return errors.Join(applyErr, fmt.Errorf("reconcile generated ownership TXT: %w", cleanupErr))
	}
	return applyErr
}

func (p *Provider) rollbackCreatedOwnership(ctx context.Context, cleanups []pendingOwnershipCleanup) error {
	if p.dryRun {
		return nil
	}
	for _, cleanup := range cleanups {
		p.enqueueOwnershipCleanup(cleanup)
	}
	return p.drainPendingOwnershipCleanups(ctx)
}

// drainPendingOwnershipCleanups uses a fresh read, never the provider cache.
// It reconciles mixed ownership updates and removes an ownership record only
// when the protected primary is confirmed absent. Failed work remains queued
// and makes Records fail closed so the TXT registry cannot successfully
// observe and then forget an invisible orphan.
func (p *Provider) drainPendingOwnershipCleanups(ctx context.Context) error {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()

	if len(p.pendingOwnershipCleanups) == 0 && len(p.pendingOwnershipRepairs) == 0 {
		return nil
	}

	p.cache.invalidate()
	records, err := p.client.Retrieve(ctx, p.domain)
	if err != nil {
		return fmt.Errorf("retrieve before ownership cleanup: %w", err)
	}
	idx, err := indexRecords(p.scopedRecords(records))
	if err != nil {
		return fmt.Errorf("index before ownership cleanup: %w", err)
	}

	repairKeys := make([]string, 0, len(p.pendingOwnershipRepairs))
	for key := range p.pendingOwnershipRepairs {
		repairKeys = append(repairKeys, key)
	}
	sort.Strings(repairKeys)
	for _, key := range repairKeys {
		repair := p.pendingOwnershipRepairs[key]
		if err := p.reconcilePendingOwnershipRepair(ctx, repair, &idx); err != nil {
			p.cache.invalidate()
			return fmt.Errorf("repair %s/%s: %w", repair.new.DNSName, repair.new.RecordType, err)
		}
		delete(p.pendingOwnershipRepairs, key)
	}

	keys := make([]string, 0, len(p.pendingOwnershipCleanups))
	for key := range p.pendingOwnershipCleanups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cleanup := p.pendingOwnershipCleanups[key]
		if protectedRecordExists(&idx, cleanup) {
			// The marker is still needed by its protected record. Preserve it and
			// resolve the pending item.
			delete(p.pendingOwnershipCleanups, key)
			continue
		}
		if err := p.deleteEndpoint(ctx, cleanup.ownership, &idx); err != nil {
			p.cache.invalidate()
			return fmt.Errorf("delete %s/%s: %w", cleanup.ownership.DNSName, cleanup.ownership.RecordType, err)
		}
		delete(p.pendingOwnershipCleanups, key)
	}
	p.cache.invalidate()
	return nil
}

func (p *Provider) reconcilePendingOwnershipRepair(ctx context.Context, repair pendingOwnershipRepair, idx *index) error {
	primaryPresent := protectedRecordExists(idx, pendingOwnershipCleanup{
		ownership: repair.new,
		protected: repair.protected,
	})
	if !primaryPresent {
		if err := p.deleteEndpoint(ctx, repair.old, idx); err != nil {
			return err
		}
		if repair.old.Targets[0] != repair.new.Targets[0] {
			if err := p.deleteEndpoint(ctx, repair.new, idx); err != nil {
				return err
			}
		}
		return nil
	}

	oldTarget, newTarget := repair.old.Targets[0], repair.new.Targets[0]
	oldIDs := idx.match(repair.old.DNSName, repair.old.RecordType, oldTarget)
	newIDs := idx.match(repair.new.DNSName, repair.new.RecordType, newTarget)
	if oldTarget == newTarget {
		_, err := p.convergeTarget(ctx, repair.new, newTarget, idx, "create-ownership-repair", "edit-ownership-repair")
		return err
	}
	if (len(oldIDs) > 0) != (len(newIDs) > 0) {
		// Exactly one ownership value still protects the untouched primary, so
		// the next ExternalDNS reconciliation can safely retry the update.
		return nil
	}
	if len(oldIDs) == 0 {
		_, err := p.convergeTarget(ctx, repair.new, newTarget, idx, "create-ownership-repair", "edit-ownership-repair")
		return err
	}
	if err := p.editOwnershipTargets(ctx, repair.new, newTarget, oldIDs, idx); err != nil {
		return err
	}
	_, err := p.convergeTarget(ctx, repair.new, newTarget, idx, "create-ownership-repair", "edit-ownership-repair")
	return err
}

func (p *Provider) editOwnershipTargets(
	ctx context.Context,
	newEP *endpoint.Endpoint,
	newTarget string,
	ids []string,
	idx *index,
) error {
	in, err := p.recordInput(newEP, newTarget)
	if err != nil {
		return err
	}
	for _, id := range ids {
		log.WithFields(log.Fields{
			"action": "edit-ownership-on-update", "name": newEP.DNSName,
			"type": newEP.RecordType, "target": newTarget, "id": id, "dry_run": p.dryRun,
		}).Info("apply")
		if !p.dryRun {
			if err := p.client.Edit(ctx, p.domain, id, in); err != nil {
				return err
			}
		}
		if err := idx.replace(id, newEP.DNSName, in); err != nil {
			return err
		}
	}
	return nil
}

// protectedRecordExists reports whether the exact managed record protected by
// an ownership marker is present. ExternalDNS v0.21 always type-qualifies its
// generated TXT names, so a same-name sibling of a different type must not keep
// an orphaned marker alive. An empty protected type remains conservative for a
// genuinely ambiguous caller-supplied batch.
func protectedRecordExists(idx *index, cleanup pendingOwnershipCleanup) bool {
	name := canonicalDNSName(cleanup.protected.DNSName)
	recordType := strings.ToUpper(strings.TrimSpace(cleanup.protected.RecordType))
	if name == "" {
		return true
	}
	if recordType != "" && name == cleanup.ownership.DNSName && recordType == cleanup.ownership.RecordType {
		// Provider state cannot distinguish a colliding TXT primary from its
		// ownership marker, so deletion would not be safe.
		return true
	}
	for key, ids := range idx.byKey {
		if len(ids) == 0 || key.name != name {
			continue
		}
		if recordType == "" || key.recType == recordType {
			return true
		}
	}
	return false
}

func (p *Provider) inFilter(name string) bool {
	return matchesExplicitFilters(p.include, canonicalDNSName(name))
}

// createEndpoint creates one Porkbun record per target and reports targets for
// which Create was attempted while the record was absent. The API may commit a
// request even when the client receives an error, so failure recovery confirms
// actual state with a fresh Retrieve.
func (p *Provider) createEndpoint(ctx context.Context, ep *endpoint.Endpoint, idx *index) ([]string, error) {
	createdTargets := make([]string, 0, len(ep.Targets))
	for _, target := range ep.Targets {
		attempted, err := p.convergeTarget(ctx, ep, target, idx, "create", "edit-on-create")
		if attempted {
			createdTargets = append(createdTargets, target)
		}
		if err != nil {
			return createdTargets, err
		}
	}
	return createdTargets, nil
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
) (bool, error) {
	in, err := p.recordInput(ep, target)
	if err != nil {
		return false, err
	}
	ids := idx.match(ep.DNSName, ep.RecordType, target)
	if len(ids) == 0 {
		log.WithFields(log.Fields{
			"action": createAction, "name": ep.DNSName, "type": in.Type,
			"target": target, "ttl": in.TTL, "dry_run": p.dryRun,
		}).Info("apply")
		if p.dryRun {
			return true, idx.replace(dryRunRecordID(ep, target), ep.DNSName, in)
		}
		id, err := p.client.Create(ctx, p.domain, in)
		if err != nil {
			return true, err
		}
		return true, idx.replace(id, ep.DNSName, in)
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
				return false, err
			}
		}
		if err := idx.replace(keeper, ep.DNSName, in); err != nil {
			return false, err
		}
	}

	for _, id := range ids {
		if id == keeper {
			continue
		}
		if err := p.deleteIndexedRecord(ctx, idx, id, "delete-duplicate"); err != nil {
			return false, err
		}
	}
	return false, nil
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
		if _, err := p.convergeTarget(ctx, newEP, target, idx, "create-on-update", "edit-on-update"); err != nil {
			return err
		}
	}
	return nil
}

// updateOwnershipEndpoint edits an existing ownership record in place whenever
// its serialized labels change. This avoids a window with two distinct owner
// values, which v0.21 would collapse and only partially delete.
func (p *Provider) updateOwnershipEndpoint(ctx context.Context, oldEP, newEP *endpoint.Endpoint, idx *index) error {
	oldTarget, newTarget := oldEP.Targets[0], newEP.Targets[0]
	if oldTarget != newTarget {
		oldIDs := idx.match(oldEP.DNSName, oldEP.RecordType, oldTarget)
		if len(oldIDs) > 0 {
			if err := p.editOwnershipTargets(ctx, newEP, newTarget, oldIDs, idx); err != nil {
				return err
			}
		}
	}
	if _, err := p.convergeTarget(ctx, newEP, newTarget, idx, "create-ownership-on-update", "edit-ownership-on-update"); err != nil {
		return err
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
