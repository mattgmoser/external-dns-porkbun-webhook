# Changelog

## 0.4.1

### Security and distribution

- Upgrade `golang.org/x/net` to `v0.56.0` for GO-2026-5942 / CVE-2026-46600 and `golang.org/x/text` to `v0.39.0` for GO-2026-5970 / CVE-2026-56852, removing both actionable package-level findings from the `0.4.0` webhook image; `govulncheck` reported no reachable vulnerable call path before the upgrade.
- Add a pre-publication compiled-binary scan, then scan the immutable release image on `linux/amd64`, `linux/arm64`, and `linux/arm/v7` with pinned Trivy `v0.72.0`; report every finding, retain the JSON reports, and block chart publication and mutable-channel promotion for any vulnerability with an available fix at any severity.
- Rescan the latest verified immutable webhook image daily so newly disclosed actionable vulnerabilities surface after release.
- Document that Artifact Hub aggregates the webhook image with the official ExternalDNS runtime image when calculating the chart's security grade, while keeping both images and all residual upstream findings visible.

## 0.4.0

### Helm and security

- Reactivate the Artifact Hub package with a supported chart that wraps the official ExternalDNS `1.21.1` chart and installs the Porkbun provider through its native webhook sidecar integration.
- Keep the unauthenticated provider API on `127.0.0.1:8888` inside the shared Pod and expose only the separate operations endpoint through the Service.
- Explicitly point ExternalDNS at `http://127.0.0.1:8888` so client resolution cannot diverge from the webhook's IPv4 loopback listener.
- Continue projecting Kubernetes API credentials only into the ExternalDNS container, so the webhook sidecar receives no controller token.
- Pin the Deployment strategy to `Recreate` so upgrades do not overlap independently rate-limited webhook sidecars.
- Preserve immutable standalone releases through `0.3.0` as unsupported migration history (`0.3.0` remains explicitly deprecated); document that legacy users must preserve TXT ownership settings and avoid running two writable controllers.
- Require an explicit controller-replacement acknowledgement for the first in-place upgrade from a legacy release, then record the safe topology so routine upgrades after a fresh or acknowledged `0.4.0` install remain repeatable.
- Reject placeholder domains and TXT owner IDs at render time, before an unsafe or nonfunctional workload reaches the cluster.
- Require the webhook provider, supported loopback provider URL, and exact listener split at render time so values overrides cannot remove the Porkbun sidecar or re-expose the unauthenticated mutation endpoint.
- Warn inline-credential users to create a differently named independent Secret before uninstalling or upgrading a legacy release that owns its credential Secret.

### Provider compatibility

- Accept and discard provider metadata that ExternalDNS v0.21 copies onto generated ownership TXT endpoints, while continuing to reject alias metadata on ordinary TXT records.
- Consume the TXT registry's current-endpoint `txt/force-update` control marker instead of rejecting its metadata repair path.
- Use apex-safe `external-dns-%{record_type}.` ownership names plus a stable wildcard replacement for new installs, and test non-apex, apex ALIAS, and wildcard writes through the exact v0.21 TXT registry.
- Write generated ownership TXT records before the records they protect, validate v0.21's paired mutation layouts, and conditionally clean invisible ownership orphans after partial creates, updates, or deletes.
- Reject multi-target CNAME and ALIAS endpoints before any Porkbun write because those record types are single-target by definition.

### Distribution

- Point the README badge at the package's permanent Artifact Hub URL instead of a search view that hid deprecated packages.
- Publish `artifacthub-repo.yml` beside the Helm index with the live repository ID, enabling Artifact Hub publisher verification.
- Replace the stale GitHub Pages landing page with installation instructions for the supported same-Pod chart.
- Declare the upstream chart dependency and both runtime images explicitly in chart metadata.
- Add native Helm dependency updates so Dependabot can track new supported ExternalDNS chart releases.
- Reject a release tag when the wrapper defaults, direct-integration example, or Artifact Hub image metadata do not point at that release's image.
- Accept only stable core-SemVer release tags, keep full-version image tags immutable across workflow retries, and move the `major.minor`, `major`, and `latest` image tags plus GitHub's latest release only after the highest stable release's chart, signature, and published index are verified.
- Bind image metadata and provenance to the exact source repository, revision, and GitHub Actions run; bind the chart-releaser tag to the source commit; and anchor chart provenance verification to the checked-in signing key.
- Safely recreate only validated zero- or one-asset partial chart releases after an interrupted upload, compare regenerated chart contents semantically, and require the published Helm index to match the immutable downloaded release asset's digest and URL.
- Document TXT multi-string segmentation and escaped-quote normalization instead of leaving those representation limits implicit.

## 0.3.0

### Security

- Make the official ExternalDNS v0.21 chart's same-Pod sidecar the canonical deployment. The provider API binds to `127.0.0.1:8888`; only the separate ops endpoint is exposed.
- Project the Kubernetes API token only into the ExternalDNS container, so the webhook sidecar does not inherit controller credentials.
- Deprecate the repository's standalone chart because it exposes an unauthenticated DNS mutation API through a Service. Legacy installs now require `legacyStandalone.acceptRisk=true`.
- Disable ServiceAccount token automounting and overlapping rollouts in the legacy standalone Deployment.
- Move CI and the container build to the security-patched Go 1.26.5 toolchain.
- Upgrade `golang.org/x/net` to v0.55.0, resolving reachable IDNA vulnerability `GO-2026-5026`.

### Reliability

- Update the project to ExternalDNS v0.21.0 and add a wire-level compatibility test that exercises this server through the upstream v0.21 webhook client.
- Attach a unique Porkbun idempotency key to every logical API call and reuse it across retries, preventing duplicate writes when a response is lost after a mutation commits.
- Preserve structured Porkbun error codes, request IDs, retry hints, and retryability; bound each HTTP attempt to 10 seconds and the client to two retries with context-cancellable rate/backoff waits.
- Validate an entire ExternalDNS change set before contacting Porkbun, including exact zone/domain-filter boundaries, record type, target syntax, TTL, and ALIAS representation.
- Normalize an unset ExternalDNS TTL to Porkbun's 600-second minimum, eliminating repeated invalid TTL edits, and handle wildcard owners without repeated IDNA warnings.
- Translate MX/SRV priority fields correctly and round-trip every Porkbun DNS type: A, AAAA, CNAME, ALIAS, TXT, MX, NS, SRV, TLSA, CAA, SSHFP, HTTPS, and SVCB.
- Strictly parse and canonically round-trip CAA, HTTPS, and SVCB presentation data without altering quoted or escaped values; preserve significant TXT boundary whitespace and reject malformed structured records before any write.
- Produce deterministic endpoint ordering, retain duplicate-record drift for the planner, and converge duplicates while preferring a correctly typed/TTL record.
- Start the protocol listener without a blocking provider preflight; background readiness retrieves the configured zone rather than only pinging the account. Validate boolean/cache/logging configuration instead of silently accepting unsafe values.
- Pin and document a tested ExternalDNS v0.21/chart v1.21.1 sidecar configuration, including stable TXT ownership, credential-aware readiness, security contexts, resources, and practical webhook deadlines.
- Validate the legacy chart's domain, credentials, risk acknowledgement, and singleton replica count before rendering.
- Keep legacy listener environment variables and declared container ports in sync.
- Roll legacy Pods when chart-managed inline credentials change.
- Fix ServiceMonitor discovery when the monitor object is placed in a different namespace from the Service.

### Build and release

- Migrate golangci-lint configuration and CI to golangci-lint v2.
- Stop the Makefile from treating lint failures as a missing optional tool.
- Render both the canonical upstream chart configuration and the deprecated standalone chart in CI.
- Scan reachable Go call paths with a pinned `govulncheck` in CI and before publication.
- Gate image publication on tests and chart verification, with job-scoped release permissions.
- Publish `main` and immutable `sha-*` candidate images from the default branch without moving `latest`; stable version tags publish the semver tags and advance `latest`.
- Prevent overlapping main-branch release runs from moving the mutable `main` image tag backwards, and enforce chart/example/changelog version consistency on tags.
- Publish max-mode build provenance and an SBOM alongside each multi-architecture image.
