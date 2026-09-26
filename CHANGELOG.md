# Changelog

## 0.6.0

### Security

- Pin the bundled ExternalDNS controller to `v0.23.0` by digest. The `v0.22.0` image now carries four fixable findings: CVE-2026-84445 (HIGH, `google.golang.org/grpc` `v1.83.1`), CVE-2026-56855 and CVE-2026-78662 (MEDIUM, `golang.org/x/crypto` `v0.55.0`), and DLA-4792-1 (`tzdata`). `v0.23.0` carries only DLA-4792-1, from upstream's own Debian 12 base. Artifact Hub grades this package on both images.
- A values file saved from an earlier release pins the old controller digest, which still passes the chart's digest check. Re-export the values or bump `external-dns.image.tag` as well as the webhook tag when upgrading.

### ExternalDNS v0.23.0

- Update to ExternalDNS `v0.23.0` and the official chart `1.22.0`. The chart renders identically to `1.21.1` for these values apart from its version labels, and every argument it renders, including a `policy: sync` production configuration, was verified against the `v0.23.0` binary before pinning. The webhook provider protocol is unchanged.
- Ownership TXT records are now deleted and replaced using the value stored in the zone (kubernetes-sigs/external-dns#6680). With `v0.22`, an ownership record whose stored value differed from the re-serialized labels was skipped on delete, because the provider matches deletes by value, and duplicated on update. New regression tests seed exactly that record and fail against `v0.22.0`.
- Document the `v0.23.0` changes that reach this chart: the 32 MiB webhook body cap, the `--txt-encrypt-enabled` gzip change under Go 1.27, the new `--enable-legacy-annotation-prefix` migration flag, and the `crd` registry namespace change. None needs a values change with the TXT registry.
- The rendered `app.kubernetes.io/version` label follows the dependency chart's appVersion and reads `0.22.0`; the running controller is the pinned `v0.23.0` image.

### Build and dependencies

- Rebuild the webhook on Go `1.27.1`. The `sigs.k8s.io/external-dns` `v0.23.0` module requires Go `1.27`, which also supersedes the Go `1.26.8` bug-fix release.
- Update `github.com/sirupsen/logrus` to `v1.10.2` (no functional change) and the transitive modules `v0.23.0` requires.
- Update CI to golangci-lint `v2.14.0` (Go `1.27` support arrived in `v2.13.0`) and govulncheck `v1.8.0`.

### Testing

- Re-point the upstream TXT registry integration suite at the `v0.23.0` registry and planner it now exercises.

## 0.5.1

### Security

- Rebuild the webhook image on `gcr.io/distroless/static-debian13:nonroot`, clearing DLA-4792-1 (`tzdata` `2026b-0+deb12u1`, fixed in `2026c-0+deb12u1`), which had blocked the daily release scan of the `0.5.0` image on every published architecture since 2026-09-24. Debian 12 is now in LTS, where this update shipped, and the distroless Debian 12 base had still not picked it up as of 2026-09-26; the Debian 13 base ships `tzdata` `2026c-0+deb13u1` on `linux/amd64`, `linux/arm64`, and `linux/arm/v7`.

### Changed

- The runtime base moves from Debian 12 to Debian 13 distroless static. The webhook is a static `CGO_ENABLED=0` binary, so the base supplies only CA certificates, time-zone data, and the nonroot `65532` user; the binary, chart templates, and bundled ExternalDNS `v0.22.0` controller are unchanged.
- A saved values file pins `external-dns.provider.webhook.image.tag`. Bump it to `0.5.1`, or re-export the chart values, when upgrading; otherwise the release keeps running the `0.5.0` image.

## 0.5.0

### Security

- Rebuild the webhook on Go `1.26.7`, clearing eight fixable HIGH Go standard library findings that had blocked the daily release scan since the `0.4.1` image was published: CVE-2026-33818 (`encoding/asn1`), CVE-2026-39821 (`golang.org/x/net/idna`), CVE-2026-46600 (`golang.org/x/net/dns/dnsmessage`), CVE-2026-56853 (`net/http`), CVE-2026-56858 (`html/template`), CVE-2026-56859 (`encoding/xml`), CVE-2026-56860 (`net/url`), and CVE-2026-56862 (`crypto/tls`). Five of these were reachable from this binary's own call graph according to `govulncheck`.
- Pin the bundled ExternalDNS controller to the patched `v0.22.0` image by digest. The official chart dependency remains `1.21.1`, whose `0.21.0` appVersion image is built with Go `1.26.1` and contributed 55 fixable findings (37 HIGH) to this package's published image set; `v0.22.0` reports none. Every argument this chart renders was verified against the `v0.22.0` binary before pinning.
- Validate `annotationPrefix` and the controller image tag as raw values rather than trimmed ones. The dependency renders both verbatim, so a leading space passed a trimmed allowlist while the controller searched for annotation keys beginning with a space -- every annotation-derived endpoint would leave the desired set and `policy: sync` would delete the records behind them.
- Accept any DNS-subdomain `annotationPrefix`, not only the two upstream spellings. A split-horizon release may legitimately run a custom prefix, and forcing it onto one of ours would hide its annotations and cause exactly the deletion this guard exists to prevent.
- Fail closed when the bundled ExternalDNS controller image is not pinned to an exact digest. The dependency chart's own default is the `0.21.0` image, so a values file that clears or floats `external-dns.image.tag` -- which a coalesced values dump reintroduces as `tag: null` -- would otherwise silently downgrade the controller back to the vulnerable runtime.
- Escape Prometheus label values in the hand-rolled exposition renderer. No value emitted today can break out of its quoted string, because route names are internal constants and `net/http` rejects a non-token request method before a handler runs, but escaping removes the dependency on those invariants.

### ExternalDNS v0.22.0

- Update to ExternalDNS `v0.22.0`. The webhook provider protocol is unchanged (`application/external.dns.webhook+json;version=1`), and the serialised `Endpoint` and `plan.Changes` shapes are wire-compatible with `v0.21`, so this release interoperates with both controller versions.
- Pin `annotationPrefix` to `external-dns.alpha.kubernetes.io/`. ExternalDNS `v0.22.0` changed the default to `external-dns.kubernetes.io/` with no fallback; inheriting that silently would hide already-annotated hostnames from the controller, and under `policy: sync` the planner deletes the records it can no longer see. Migrate annotations first, then set the new prefix deliberately.
- Fail closed at render time when `annotationPrefix` or `policy` is unset, and when `annotationPrefix` is malformed or is not a recognised ExternalDNS prefix. `v0.22.0` made `--policy` required with no default.
- Keep `DNAME`, added to ExternalDNS's supported record types in `v0.22.0`, unmanaged: Porkbun has no `DNAME` record type. ExternalDNS only sends types listed in `--managed-record-types` (default `A`, `AAAA`, `CNAME`), so it is never sent unless an operator opts in, and an opt-in now produces a clear validation error rather than a silent drop.
- Update `github.com/miekg/dns` to `v1.1.73` and `github.com/sirupsen/logrus` to `v1.10.1`, superseding the two dependency updates whose CI runs failed on the Go `1.26.5` toolchain.

### Documentation

- Warn that a saved values file pins the webhook image tag, so re-using it unchanged across an upgrade keeps the old image, and show how to diff a saved copy against the new defaults.
- Warn against `--reuse-values` for this upgrade: it replaces the new chart's defaults with the previous release's values, leaving a release that reports chart `0.5.0` while still running both old images.
- Correct the dry-run instructions to set `DRY_RUN` in a values file, because `--set ...value=true` renders a YAML boolean and Kubernetes requires `EnvVar.value` to be a string.
- Document the two v0.22.0 source migrations this chart cannot guard, `gateway-tlsroute` requiring Gateway API `v1` `TLSRoute` and `ambassador-host` requiring `getambassador.io/v3alpha1`, either of which stops reconciliation silently on older CRDs.

### Testing

- Pin the upstream `DomainFilter` JSON contract with regression tests. The provider's scope guard inspects the filter through its JSON representation because the type's fields are unexported, so a renamed or dropped key would silently stop it rejecting exclusion and regular-expression filters it cannot honour. An upgrade that changes that shape now fails loudly in tests.
- Re-point the upstream TXT registry integration suite at the `v0.22.0` registry and planner it now exercises, confirming the paired ownership layouts this provider depends on are unchanged.

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
