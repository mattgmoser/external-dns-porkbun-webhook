# Architecture

## Overview

This project is one of the [External-DNS webhook providers](https://kubernetes-sigs.github.io/external-dns/v0.14.0/tutorials/webhook-provider/) — an HTTP service that External-DNS calls to translate desired Kubernetes-DNS state into Porkbun API calls.

```
┌────────────────────────────────────────────────────────────────────┐
│  Kubernetes cluster                                                │
│                                                                    │
│  ┌────────────────────┐    HTTP    ┌────────────────────────────┐  │
│  │ external-dns       │◄──────────►│ external-dns-porkbun-      │  │
│  │ (upstream)         │            │ webhook (this project)     │  │
│  │                    │            │                            │  │
│  │ - watches Ingress, │            │  ┌──────────────────────┐  │  │
│  │   Service, Gateway │            │  │ webhook handler      │  │  │
│  │ - computes Plan    │            │  │ (HTTP, port :8888)   │  │  │
│  │ - calls webhook to │            │  └──────────┬───────────┘  │  │
│  │   apply Plan       │            │             │              │  │
│  └────────────────────┘            │  ┌──────────▼───────────┐  │  │
│                                    │  │ provider             │  │  │
│                                    │  │ - Records()          │  │  │
│                                    │  │ - ApplyChanges()     │  │  │
│                                    │  │ - AdjustEndpoints()  │  │  │
│                                    │  └──────────┬───────────┘  │  │
│                                    │             │              │  │
│                                    │  ┌──────────▼───────────┐  │  │
│                                    │  │ porkbun client       │  │  │
│                                    │  │ - retries            │  │  │
│                                    │  │ - rate limiting      │  │  │
│                                    │  │ - JSON API           │  │  │
│                                    │  └──────────┬───────────┘  │  │
│                                    └─────────────┼──────────────┘  │
└──────────────────────────────────────────────────┼─────────────────┘
                                                   │ HTTPS
                                                   ▼
                                       ┌──────────────────────┐
                                       │ api.porkbun.com      │
                                       └──────────────────────┘
```

## Packages

### `porkbun/`

Thin client for the Porkbun JSON DNS API. Endpoint coverage:

| API endpoint | Method |
|---|---|
| `/ping` | `Ping(ctx)` |
| `/dns/retrieve/{domain}` | `Retrieve(ctx, domain)` |
| `/dns/create/{domain}` | `Create(ctx, domain, input)` |
| `/dns/edit/{domain}/{id}` | `Edit(ctx, domain, id, input)` |
| `/dns/delete/{domain}/{id}` | `Delete(ctx, domain, id)` |

Operational behavior:
- **Rate limiting**: serializes calls with a configurable minimum gap (default 1.1s) to stay under Porkbun's 1 req/sec per-key limit.
- **Retries**: HTTP 5xx, 429, and transient network errors are retried with exponential backoff + ±25% jitter. 4xx errors fail fast.
- **Timeouts**: 30s per HTTP call by default (configurable via `WithHTTPClient`).

### `provider/`

Implements the External-DNS [`Provider`](https://pkg.go.dev/sigs.k8s.io/external-dns/provider) interface:

- `Records(ctx)` — pulls all records for the domain, filters to managed types (A/AAAA/CNAME/TXT/MX/SRV/CAA), collapses same-name+type into multi-target endpoints.
- `ApplyChanges(ctx, plan)` — runs `Delete` then `Update`-as-delete-and-create then `Create`. Reuses an index of current records for content matching.
- `AdjustEndpoints(eps)` — bumps any TTL below 600s up to 600 (Porkbun's effective minimum).
- `GetDomainFilter()` — returns the configured filter so External-DNS can pre-narrow.

Caching: optional in-memory cache (`CACHE_TTL`, default 1m) avoids hammering Porkbun on every reconcile. Invalidated on every `ApplyChanges`.

### `webhook/`

HTTP server implementing the External-DNS webhook protocol (Content-Type `application/external.dns.webhook+json;version=1`):

- `GET /` — domain filter negotiation
- `GET /records` — current state
- `POST /records` — apply changes
- `POST /adjustendpoints` — pre-store canonicalisation

Plus an ops server (separate port) for `/healthz`, `/readyz`, and `/metrics`. Readiness probes credentials and connectivity every 15s.

### `main.go`

Reads env vars, configures logging, constructs the provider + webhook, runs until SIGINT/SIGTERM. Pre-flight credential check at startup (non-fatal — readiness reflects the result).

## Trade-offs and decisions

**Why no batched API?** Porkbun has no bulk endpoint — each create/edit/delete is one HTTP call. We serialize calls (rate limit) but parallelism wouldn't help anyway.

**Why a separate ops port?** Probes and metrics shouldn't compete with the webhook server's connection pool, and they shouldn't be exposed if/when the webhook gets fronted by a sidecar pattern.

**Why distroless static?** Smallest possible image (~15MB total), no shell, no package manager, runs as nonroot UID 65532.

**Why no `/recordspec` support?** External-DNS's webhook v1 protocol doesn't include it; we follow v1 strictly. v2 is in proposal stage upstream — we'll add support when it lands.

## Failure modes

| Failure | Behavior |
|---|---|
| Porkbun API down (5xx) | Retry with backoff, then fail the apply; external-dns will retry next reconcile |
| Porkbun rate limit hit | Internal serialization keeps us below limit; if hit anyway, treat as transient |
| Credentials wrong | Readiness probe goes red; external-dns sees readiness failure on the Service endpoints |
| TXT record collision | external-dns's TXT registry handles ownership; we don't touch records without the owner-id TXT marker (when `policy=sync`) |
| Two webhooks managing same zone | First one wins; second one's apply will see drift. Use `domainFilter` + non-overlapping zones to avoid. |
