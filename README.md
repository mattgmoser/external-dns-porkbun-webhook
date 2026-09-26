# external-dns-porkbun-webhook

[![ci](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/ci.yaml/badge.svg)](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/ci.yaml)
[![release](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/release.yaml/badge.svg)](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/release.yaml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/external-dns-porkbun-webhook)](https://artifacthub.io/packages/helm/external-dns-porkbun-webhook/external-dns-porkbun-webhook)

A production-grade [External-DNS](https://kubernetes-sigs.github.io/external-dns/) **webhook provider** for [Porkbun](https://porkbun.com/) DNS.

Works with ExternalDNS to watch the Kubernetes sources you enable and keep a Porkbun zone in sync automatically. With the chart defaults, a new Ingress or Service hostname becomes a Porkbun DNS record after ExternalDNS asks this webhook to reconcile it.

## Why this exists

Porkbun isn't built into upstream External-DNS, and the existing community webhooks have gaps (no multi-arch images, dated External-DNS versions, no Helm chart). This project aims to be the canonical, batteries-included Porkbun integration:

- **Multi-arch images** - `linux/amd64`, `linux/arm64`, `linux/arm/v7` (Pi support)
- **Official ExternalDNS chart integration** - secure same-Pod sidecar values included
- **Prometheus metrics** + Grafana-friendly histograms
- **Health and readiness probes** with credential and zone-access validation
- **Conservatively rate limited** - serializes Porkbun API calls with a safe minimum gap
- **Retry-safe writes** - idempotency keys plus bounded retries prevent duplicates after ambiguous failures
- **Complete Porkbun DNS type coverage** - including priority-aware MX/SRV and ALIAS interoperability
- **Dry-run mode** for safe testing
- **Distroless container** (small, runs as nonroot)
- **Domain filter scoping** - narrow what the webhook can touch
- **Tested** with an in-memory mock Porkbun API
- **Apache 2.0** licensed

## Quickstart (Helm)

The supported chart wraps the **official ExternalDNS chart** and configures this provider as its native sidecar. ExternalDNS's provider protocol has no authentication, so the provider listener is bound to `127.0.0.1:8888` and is reachable only from the ExternalDNS container in the same Pod. The separate `:8080` ops listener remains available for health checks and metrics.

Start by creating a namespace and an existing Secret. Avoid putting API keys in Helm values: Helm stores release values in the cluster and command-line values can remain in shell history.

Use a dedicated Porkbun API key and restrict it to the managed domain in [Porkbun's API key settings](https://porkbun.com/account/api). If the cluster has a stable egress address, add an IP restriction too. Porkbun documents both restrictions as per-key controls, so they limit the credential's blast radius independently of Kubernetes.

```sh
kubectl create namespace external-dns --dry-run=client -o yaml | kubectl apply -f -
kubectl -n external-dns create secret generic porkbun-creds \
  --from-literal=PORKBUN_API_KEY="$YOUR_API_KEY" \
  --from-literal=PORKBUN_SECRET_API_KEY="$YOUR_SECRET_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Add the repository and export the chart's version-pinned values:

```sh
helm repo add edns-porkbun https://mattgmoser.github.io/external-dns-porkbun-webhook
helm repo update
helm show values edns-porkbun/external-dns-porkbun-webhook \
  --version 0.6.0 > external-dns-porkbun-values.yaml
```

Change all of these in `external-dns-porkbun-values.yaml` before installing:

- Secret name if you did not use `porkbun-creds`
- `PORKBUN_DOMAIN`, `DOMAIN_FILTER`, and `domainFilters`
- `txtOwnerId` to a stable, unique cluster identifier
- `txtPrefix` if another ExternalDNS instance already owns records in the zone
- `txt-wildcard-replacement` if `_wildcard` is a real first label in the zone

Never change `txtOwnerId`, `txtPrefix`, or `txt-wildcard-replacement` casually after ExternalDNS has created records; those fields are its ownership boundary. If this is an upgrade, preserve the values already used by the cluster. For a new installation, the `_wildcard` default keeps generated ownership records valid; choose a different stable token if it could collide with a real first label.

```sh
helm upgrade --install external-dns \
  edns-porkbun/external-dns-porkbun-webhook \
  --version 0.6.0 \
  --namespace external-dns \
  --values external-dns-porkbun-values.yaml
```

The example starts with ExternalDNS's safer `upsert-only` policy. Review the plan and ownership TXT records before opting into `sync`, which also deletes records no longer desired by Kubernetes.

The example disables automatic ServiceAccount-token mounts and explicitly projects the token only into the ExternalDNS container. The webhook sidecar therefore does not inherit the controller's Kubernetes credentials. Porkbun credentials are environment variables read at process start; after rotating the Secret, restart the Deployment so the sidecar loads the new values:

```sh
kubectl -n external-dns rollout restart deployment/external-dns
```

### Webhook timeouts and large changes

ExternalDNS defaults to a 15-second total webhook deadline, while Porkbun operations are serialized and a plan can easily contain hundreds of changes. The canonical values use a five-minute total deadline (`30s` + `4m30s`); this covers roughly 200 single-record mutations at the conservative request rate, while ordinary reconciliations complete much sooner. Multi-target changes or retries can still exceed that budget. ExternalDNS v0.23 does not apply its generic `batch-change-size` setting to webhook providers, so that flag cannot safely shorten this bound. Stage unusually large migrations and watch both containers' logs rather than setting an unbounded timeout.

### TXT representation

Porkbun stores TXT content as one unquoted string. The webhook removes one matching outer pair of double quotes when ExternalDNS supplies it, which covers common SPF, DKIM, and verification records while preserving ordinary boundary whitespace. DNS multi-string segment boundaries are not preserved as distinct segments, and values that depend on escaped embedded quotes may be normalized during a read/write round trip. Verify those uncommon records after reconciliation instead of relying on byte-for-byte presentation identity.

### Chart history and migration

Chart `0.3.0` and earlier deployed only the webhook in a separate Pod and exposed its unauthenticated mutation API through a ClusterIP Service. Those immutable releases remain available for history and are unsupported; `0.3.0` is explicitly marked deprecated. Chart `0.4.0` replaces that topology with the official ExternalDNS chart and the same-Pod sidecar described above, so the latest Artifact Hub package is active again without weakening the security boundary.

Do not use the generic install command above to migrate an existing standalone-chart release. First preserve the separately managed controller's `txtOwnerId`, `txtPrefix`, `txt-wildcard-replacement`, domain filters, and policy. If the old chart created credentials from inline `porkbun.apiKey` values, create an independently managed Secret under a different name before uninstalling or upgrading it—preferably with a rotated Porkbun key—and point the new values at that name. Reusing the legacy Secret name does not protect it: either operation removes that chart-owned Secret from the release. If the old chart used `porkbun.existingSecret`, verify that Secret still exists and is not owned by the legacy Helm release.

Choose one controller path:

- If ExternalDNS is already managed directly with the official chart, keep that release. Add this project's version-pinned [`docs/external-dns-values.yaml`](docs/external-dns-values.yaml) sidecar settings to it, roll out the same-Pod configuration, and then remove the old standalone webhook release.
- To adopt this wrapper, stop and remove the separately managed ExternalDNS controller and the old standalone webhook release, then install `0.6.0` with the preserved ownership settings and independent credential Secret. Never overlap two writable controllers for the same names.

As a final guard, the first in-place Helm upgrade from `0.3.0` or earlier is rejected unless `migration.acknowledgeControllerReplacement=true` is explicitly set. That acknowledgement only confirms that you completed the controller handoff; it does not perform the migration. A fresh `0.4.0` or later install, or an acknowledged migration, creates a release-owned topology marker, so later routine upgrades of that release do not need the acknowledgement again.

## Configuration

Environment variables consumed by the binary:

| Variable | Required | Default | Description |
|---|---|---|---|
| `PORKBUN_API_KEY` | yes | - | Porkbun API key (`pk1_...`) |
| `PORKBUN_SECRET_API_KEY` | yes | - | Porkbun secret API key (`sk1_...`) |
| `PORKBUN_DOMAIN` | yes | - | Apex zone, e.g. `example.com` |
| `DOMAIN_FILTER` | no | `[PORKBUN_DOMAIN]` | Comma-separated list of subdomain filters |
| `WEBHOOK_LISTEN` | no | `127.0.0.1:8888` | Provider server bind; keep the loopback default for the sidecar |
| `OPS_LISTEN` | no | `:8080` | Health/readiness/metrics bind |
| `DRY_RUN` | no | `false` | Log changes but don't apply |
| `CACHE_TTL` | no | `1m` | Memory cache for `Records()` calls |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | no | `text` | `text` or `json` |

## How it works

```
┌──────────────────────┐         ┌────────────────────────┐
│  external-dns        │ webhook │ external-dns-porkbun-  │
│  (upstream chart)    │◄───────►│ webhook (this project) │
└──────────────────────┘  HTTP   └───────────┬────────────┘
            ▲                                 │
            │ watches                         │ Porkbun JSON API
            │ Ingress/Service/Gateway         ▼
┌──────────────────────┐         ┌────────────────────────┐
│ your kubernetes      │         │ api.porkbun.com        │
│ workloads            │         │ (your DNS records)     │
└──────────────────────┘         └────────────────────────┘
```

External-DNS reconciles cluster-state into desired DNS records. For Porkbun (not a built-in provider) it makes RPC calls to a webhook server over HTTP. This project IS that webhook server.

### Supported record types

The provider codec reads and writes every DNS type currently accepted by Porkbun: `A`, `AAAA`, `CNAME`, `TXT`, `MX`, `NS`, `SRV`, `TLSA`, `CAA`, `SSHFP`, `HTTPS`, and `SVCB`. Porkbun `ALIAS` records are presented to ExternalDNS as CNAME endpoints with `providerSpecific.alias=true`; an apex CNAME is automatically stored as `ALIAS`. The chart's `external-dns-%{record_type}.` TXT prefix keeps apex ownership records inside the managed zone, and `_wildcard` replaces an otherwise invalid `*` label in wildcard ownership records, as required by the [ExternalDNS TXT registry](https://kubernetes-sigs.github.io/external-dns/latest/docs/registry/txt/). Preserve established registry settings during upgrades; changing them requires a planned migration. MX and SRV priorities are translated between ExternalDNS's target syntax and Porkbun's separate `prio` field.

`DNAME`, added to ExternalDNS's supported record types in v0.22.0, is deliberately unmanaged because Porkbun has no `DNAME` record type. ExternalDNS only sends the types listed in `--managed-record-types` (default `A`, `AAAA`, `CNAME`), so it is never sent unless you opt in; if you do, the provider rejects it with an explicit validation error rather than silently dropping the record.

### Upgrading

The quickstart exports the chart's values to a file you keep. That file pins
both `external-dns.image.tag` and `external-dns.provider.webhook.image.tag`,
so re-using it unchanged across a chart upgrade keeps the **old controller and
webhook images**, including for a security release. An old controller pin is
still an exact digest, so the chart cannot tell it apart from a deliberate one.
Re-export the values on upgrade, or bump both tags by hand, and diff your copy
against the new defaults:

```sh
helm show values edns-porkbun/external-dns-porkbun-webhook --version 0.6.0 \
  | diff -u external-dns-porkbun-values.yaml - || true
```

The chart pins the bundled ExternalDNS controller image to an exact digest and
refuses to render if that pin is removed or replaced with a floating tag,
because the dependency chart's own default is the older, vulnerable image.

### Upgrading to ExternalDNS v0.23.0

Chart `0.6.0` moves the bundled controller from ExternalDNS v0.22.0 to v0.23.0, pinned by digest, on the official chart `1.22.0`. With this chart's TXT registry and default sources the upgrade needs no values changes beyond the two image tags above. Upgrading from `0.4.x` or earlier also crosses v0.22.0, so read the next section first.

- Ownership TXT records are now deleted and replaced using the value read from the zone, rather than a re-serialization of their labels ([kubernetes-sigs/external-dns#6680](https://github.com/kubernetes-sigs/external-dns/pull/6680)). Under v0.22, an ownership record whose stored value differed from that serialization, such as one written by an older ExternalDNS, was silently left behind when its record was deleted and duplicated when it was updated. v0.23 deletes or rewrites it in place. It does not go back and clean up orphans that v0.22 already left.
- Webhook request and response bodies are capped at 32 MiB (`--webhook-provider-max-body-size`). A Porkbun zone's record listing is far below that.
- If you enable `--txt-encrypt-enabled`, which this chart does not, read upstream's [encryption recommendations](https://kubernetes-sigs.github.io/external-dns/latest/docs/registry/txt/#encryption) first. Go 1.27 changed gzip output, so encrypted ownership values are no longer byte-stable across versions.
- Upstream added `--enable-legacy-annotation-prefix`, which also reads `external-dns.alpha.kubernetes.io/` annotations during a prefix migration. This chart does not set it, because its pinned prefix already preserves behaviour.
- The `crd` registry now needs `--crd-registry-namespace` when upgrading from v0.22.0. This chart uses the TXT registry.

The rendered `app.kubernetes.io/version` label comes from the dependency chart's appVersion, so it reads `0.22.0`; the running controller is the pinned v0.23.0 image. Pass your values file explicitly rather than using `--reuse-values`, for the reason given below.

### Upgrading to ExternalDNS v0.22.0

ExternalDNS v0.22.0 changed the default annotation prefix to `external-dns.kubernetes.io/` **with no fallback**, and made `--policy` a required flag. A release that still annotates with `external-dns.alpha.kubernetes.io/` would stop being seen by the controller after the change, and under `policy: sync` the planner deletes the records behind those hostnames.

This chart therefore pins `external-dns.annotationPrefix` to `external-dns.alpha.kubernetes.io/`, so upgrading is behaviour preserving, and rejects an unset, malformed, or unrecognised prefix at render time. Resources that take their hostname from an Ingress `spec.rules[].host` are unaffected either way, because that path does not involve annotations.

To move to the new prefix, migrate your annotations first, then set it deliberately as a separate change, after the upgrade has settled:

```sh
helm upgrade external-dns edns-porkbun/external-dns-porkbun-webhook \
  --version 0.6.0 \
  --values external-dns-porkbun-values.yaml \
  --set-string external-dns.annotationPrefix=external-dns.kubernetes.io/
```

Do not use `--reuse-values` for this upgrade. It replaces the new chart's defaults with the previous release's values, so the release would report chart `0.6.0` while still running the **old** ExternalDNS and webhook images -- defeating the point of a security release. Pass your values file explicitly, as above.

Upstream's [version update playbook](https://kubernetes-sigs.github.io/external-dns/latest/docs/version-update-playbook/) recommends a dry run first. Set `DRY_RUN` in your values file rather than with `--set`, because a bare `--set ...value=true` renders a YAML boolean and Kubernetes requires `EnvVar.value` to be a string:

```yaml
external-dns:
  provider:
    webhook:
      env:
        - name: DRY_RUN
          value: "true"
```

#### Source prerequisites

Two v0.22.0 source migrations are **not** guarded by this chart, because they depend on CRDs it does not install. Check these before upgrading if you use them:

- `gateway-tlsroute` now requires Gateway API `v1` `TLSRoute` (standard channel since Gateway API v1.5.0). On older CRDs the informer never syncs and DNS reconciliation stops silently.
- `ambassador-host` now requires `getambassador.io/v3alpha1`.

The chart's default sources are `ingress` and `service`, neither of which is affected.

## Endpoints

The webhook side serves the [ExternalDNS webhook protocol v1](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/):

- `GET /` - domain filter negotiation
- `GET /records` - return current managed records
- `POST /records` - apply changes (create/update/delete)
- `POST /adjustendpoints` - pre-store canonicalisation (e.g. enforces 600s TTL minimum)

The ops side (separate port) serves:

- `GET /healthz` - liveness (just "ok")
- `GET /readyz` - readiness - green only when credentials work and the configured zone can be retrieved
- `GET /metrics` - Prometheus exposition

## Metrics

| Metric | Type | Description |
|---|---|---|
| `edns_porkbun_requests_total{route,method,code}` | counter | HTTP request count |
| `edns_porkbun_request_duration_seconds{route,method,code}` | histogram | HTTP latency |
| `edns_porkbun_endpoints` | gauge | Currently advertised endpoints |
| `edns_porkbun_apply_errors_total` | counter | Apply failures |
| `edns_porkbun_changes_total{kind=create|update|delete}` | counter | Change volume |
| `edns_porkbun_ready` | gauge | 1 if creds + connectivity good |

The bundled official ExternalDNS chart can add the webhook endpoint to its `ServiceMonitor`; set `external-dns.serviceMonitor.enabled=true` when Prometheus Operator is configured to discover that namespace.

## Development

Requires Go 1.26.1 or newer. `go.mod` selects the security-patched Go 1.26.5 toolchain used by CI and the container build when automatic toolchain selection is enabled.

```sh
make build            # build local binary
make test             # unit tests with race detector
make test-coverage    # generate coverage.html
make lint             # vet + gofmt + golangci-lint
make helm-check       # render the wrapper + direct upstream configurations
make docker           # multi-arch buildx push
```

Tests use an in-memory mock of the Porkbun API; they don't need real credentials.

### Container vulnerability reports

The release workflow scans the webhook image by immutable digest on every
published architecture. It reports all Trivy findings and rejects any finding
with an available fix, at any severity. The pre-publication binary scan
prevents known findings from consuming an image version; the final digest scan
blocks chart publication and mutable-channel promotion. The latest immutable
release is rescanned daily so newly disclosed findings are not missed. This
project does not maintain a Trivy ignore list.

Artifact Hub calculates the chart's security grade from both runtime images
declared by the chart. In its security report, findings under
`ghcr.io/mattgmoser/external-dns-porkbun-webhook` belong to this project, while
findings under `registry.k8s.io/external-dns/external-dns` belong to the pinned
official ExternalDNS dependency. Both remain visible, and the upstream image is
updated when a tested supported release becomes available.

To run the webhook locally against a real Porkbun zone:

```sh
PORKBUN_API_KEY=pk1_... \
PORKBUN_SECRET_API_KEY=sk1_... \
PORKBUN_DOMAIN=example.com \
LOG_LEVEL=debug \
go run ./
```

## Status

Actively maintained. Issues, feature requests, and PRs welcome - see [CONTRIBUTING.md](CONTRIBUTING.md). Security disclosures go to [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
