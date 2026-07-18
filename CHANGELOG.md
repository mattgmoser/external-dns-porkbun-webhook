# Changelog

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
