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
  --version 0.4.0 > external-dns-porkbun-values.yaml
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
  --version 0.4.0 \
  --namespace external-dns \
  --values external-dns-porkbun-values.yaml
```

The example starts with ExternalDNS's safer `upsert-only` policy. Review the plan and ownership TXT records before opting into `sync`, which also deletes records no longer desired by Kubernetes.

The example disables automatic ServiceAccount-token mounts and explicitly projects the token only into the ExternalDNS container. The webhook sidecar therefore does not inherit the controller's Kubernetes credentials. Porkbun credentials are environment variables read at process start; after rotating the Secret, restart the Deployment so the sidecar loads the new values:

```sh
kubectl -n external-dns rollout restart deployment/external-dns
```

### Webhook timeouts and large changes

ExternalDNS defaults to a 15-second total webhook deadline, while Porkbun operations are serialized and a plan can easily contain hundreds of changes. The canonical values use a five-minute total deadline (`30s` + `4m30s`); this covers roughly 200 single-record mutations at the conservative request rate, while ordinary reconciliations complete much sooner. Multi-target changes or retries can still exceed that budget. ExternalDNS v0.21 does not apply its generic `batch-change-size` setting to webhook providers, so that flag cannot safely shorten this bound. Stage unusually large migrations and watch both containers' logs rather than setting an unbounded timeout.

### TXT representation

Porkbun stores TXT content as one unquoted string. The webhook removes one matching outer pair of double quotes when ExternalDNS supplies it, which covers common SPF, DKIM, and verification records while preserving ordinary boundary whitespace. DNS multi-string segment boundaries are not preserved as distinct segments, and values that depend on escaped embedded quotes may be normalized during a read/write round trip. Verify those uncommon records after reconciliation instead of relying on byte-for-byte presentation identity.

### Chart history and migration

Chart `0.3.0` and earlier deployed only the webhook in a separate Pod and exposed its unauthenticated mutation API through a ClusterIP Service. Those immutable releases remain available for history and are unsupported; `0.3.0` is explicitly marked deprecated. Chart `0.4.0` replaces that topology with the official ExternalDNS chart and the same-Pod sidecar described above, so the latest Artifact Hub package is active again without weakening the security boundary.

Do not use the generic install command above to migrate an existing standalone-chart release. First preserve the separately managed controller's `txtOwnerId`, `txtPrefix`, `txt-wildcard-replacement`, domain filters, and policy. If the old chart created credentials from inline `porkbun.apiKey` values, create an independently managed Secret under a different name before uninstalling or upgrading it—preferably with a rotated Porkbun key—and point the new values at that name. Reusing the legacy Secret name does not protect it: either operation removes that chart-owned Secret from the release. If the old chart used `porkbun.existingSecret`, verify that Secret still exists and is not owned by the legacy Helm release.

Choose one controller path:

- If ExternalDNS is already managed directly with the official chart, keep that release. Add this project's version-pinned [`docs/external-dns-values.yaml`](docs/external-dns-values.yaml) sidecar settings to it, roll out the same-Pod configuration, and then remove the old standalone webhook release.
- To adopt this wrapper, stop and remove the separately managed ExternalDNS controller and the old standalone webhook release, then install `0.4.0` with the preserved ownership settings and independent credential Secret. Never overlap two writable controllers for the same names.

As a final guard, the first in-place Helm upgrade from `0.3.0` or earlier is rejected unless `migration.acknowledgeControllerReplacement=true` is explicitly set. That acknowledgement only confirms that you completed the controller handoff; it does not perform the migration. A fresh `0.4.0` install or an acknowledged migration creates a release-owned topology marker, so later routine upgrades of that release do not need the acknowledgement again.

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
