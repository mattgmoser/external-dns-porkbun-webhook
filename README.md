# external-dns-porkbun-webhook

[![ci](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/ci.yaml/badge.svg)](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/ci.yaml)
[![release](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/release.yaml/badge.svg)](https://github.com/mattgmoser/external-dns-porkbun-webhook/actions/workflows/release.yaml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A production-grade [External-DNS](https://kubernetes-sigs.github.io/external-dns/) **webhook provider** for [Porkbun](https://porkbun.com/) DNS.

Watches your Kubernetes Ingress / Service / Gateway resources and keeps a Porkbun zone in sync - automatically. New Ingress with `host: foo.example.com` -> External-DNS asks this webhook -> Porkbun gets a new A record. Done.

## Why this exists

Porkbun isn't built into upstream External-DNS, and the existing community webhooks have gaps (no multi-arch images, dated External-DNS versions, no Helm chart). This project aims to be the canonical, batteries-included Porkbun integration:

- **Multi-arch images** - `linux/amd64`, `linux/arm64`, `linux/arm/v7` (Pi support)
- **Helm chart** - drop-in install
- **Prometheus metrics** + Grafana-friendly histograms
- **Health and readiness probes** with credential validation
- **Rate-limit safe** - respects Porkbun's 1 req/sec API limit by serializing
- **Retry with exponential backoff + jitter** on 5xx and transient network errors
- **Dry-run mode** for safe testing
- **Distroless container** (~15 MB)
- **Domain filter scoping** - narrow what the webhook can touch
- **Comprehensive tests** with mock Porkbun API
- **Apache 2.0** licensed

## Quickstart (Helm)

Install via Helm. Provide your Porkbun creds via an existing Secret (recommended) or inline:

```sh
helm repo add edns-porkbun https://mattgmoser.github.io/external-dns-porkbun-webhook
helm repo update

# 1. Create a Secret with your Porkbun API credentials
kubectl create namespace external-dns
kubectl -n external-dns create secret generic porkbun-creds \
  --from-literal=PORKBUN_API_KEY="$YOUR_API_KEY" \
  --from-literal=PORKBUN_SECRET_API_KEY="$YOUR_SECRET_KEY"

# 2. Install the webhook
helm install porkbun-webhook edns-porkbun/external-dns-porkbun-webhook \
  -n external-dns \
  --set porkbun.domain=example.com \
  --set porkbun.existingSecret.name=porkbun-creds

# 3. Install upstream external-dns pointing at the webhook
helm install external-dns external-dns/external-dns -n external-dns \
  --set provider.name=webhook \
  --set provider.webhook.url=http://porkbun-webhook-external-dns-porkbun-webhook.external-dns.svc.cluster.local:8888 \
  --set domainFilters[0]=example.com \
  --set sources='{ingress,service}' \
  --set policy=sync
```

That's it. Add an Ingress with a hostname under your domain and watch the A record appear at Porkbun.

## Configuration

Environment variables consumed by the binary:

| Variable | Required | Default | Description |
|---|---|---|---|
| `PORKBUN_API_KEY` | yes | - | Porkbun API key (`pk1_...`) |
| `PORKBUN_SECRET_API_KEY` | yes | - | Porkbun secret API key (`sk1_...`) |
| `PORKBUN_DOMAIN` | yes | - | Apex zone, e.g. `example.com` |
| `DOMAIN_FILTER` | no | `[PORKBUN_DOMAIN]` | Comma-separated list of subdomain filters |
| `WEBHOOK_LISTEN` | no | `:8888` | External-DNS webhook server bind |
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

## Endpoints

The webhook side serves the [external-dns webhook protocol v1](https://github.com/kubernetes-sigs/external-dns/blob/master/docs/proposal/webhook-provider.md):

- `GET /` - domain filter negotiation
- `GET /records` - return current managed records
- `POST /records` - apply changes (create/update/delete)
- `POST /adjustendpoints` - pre-store canonicalisation (e.g. enforces 600s TTL minimum)

The ops side (separate port) serves:

- `GET /healthz` - liveness (just "ok")
- `GET /readyz` - readiness - green only when Porkbun credentials validate
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

A `ServiceMonitor` is included in the chart for `kube-prometheus-stack` users (`serviceMonitor.enabled=true`).

## Development

Requires Go 1.23+.

```sh
make build            # build local binary
make test             # unit tests with race detector
make test-coverage    # generate coverage.html
make lint             # vet + gofmt + golangci-lint
make docker           # multi-arch buildx push
```

Tests use an in-memory mock of the Porkbun API; they don't need real credentials.

To run against a real cluster locally:

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
