# Architecture

## Overview

This project is one of the [ExternalDNS webhook providers](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/) - an HTTP service that ExternalDNS calls to translate desired Kubernetes-DNS state into Porkbun API calls.

```
┌──────────────────────────────────────────────────────────────────┐
│ Kubernetes cluster                                               │
│                                                                  │
│  ┌────────────────────── ExternalDNS Pod ─────────────────────┐  │
│  │                                                            │  │
│  │  ┌───────────────────┐  localhost:8888  ┌───────────────┐ │  │
│  │  │ external-dns      │◄────────────────►│ webhook       │ │  │
│  │  │ watches resources │                  │ provider      │ │  │
│  │  │ computes Plan     │                  │ Porkbun client│ │  │
│  │  └───────────────────┘                  └───────┬───────┘ │  │
│  │                                 ops :8080       │         │  │
│  │                              probes / metrics   │         │  │
│  └────────────────────────────────────────────────┼─────────┘  │
└───────────────────────────────────────────────────┼────────────┘
                                                    │ HTTPS
                                                    ▼
                                         ┌────────────────────┐
                                         │ api.porkbun.com    │
                                         └────────────────────┘
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
- **Rate limiting**: serializes calls with a conservative configurable minimum gap (default 1.1s).
- **Retries**: by default, transient network failures, HTTP 408/429/5xx responses, `409 IDEMPOTENCY_KEY_IN_USE`, and Porkbun rate-limit errors get at most two retries with exponential backoff + ±25% jitter. One idempotency key is reused across attempts so retrying a write cannot duplicate the mutation. `IDEMPOTENCY_KEY_MISMATCH` remains a permanent error. Server retry hints are honored up to 10 seconds; longer delays return control to ExternalDNS instead of exhausting its webhook deadline.
- **Timeouts**: 10s per HTTP attempt by default (configurable via `WithHTTPClient`). The caller's context remains the overall request deadline.

### `provider/`

Implements the External-DNS [`Provider`](https://pkg.go.dev/sigs.k8s.io/external-dns/provider) interface:

- `Records(ctx)` - pulls all records for the domain; round-trips A, AAAA, CNAME, TXT, MX, NS, SRV, TLSA, CAA, SSHFP, HTTPS, and SVCB; maps Porkbun ALIAS to ExternalDNS CNAME; and collapses same-name+type records into multi-target endpoints without hiding duplicate drift.
- `ApplyChanges(ctx, plan)` - validates the complete batch and its zone/filter scope before any API call, then converges deletes, updates, duplicates, and creates using an index of current Porkbun records.
- `AdjustEndpoints(eps)` - canonicalises names and targets, maps apex CNAME to ALIAS, and bumps an unset or sub-600s TTL to Porkbun's effective 600s minimum.
- `GetDomainFilter()` - returns the configured filter so External-DNS can pre-narrow.

Caching: optional in-memory cache (`CACHE_TTL`, default 1m) avoids hammering Porkbun on every reconcile. Invalidated on every `ApplyChanges`.

### `webhook/`

HTTP server implementing the External-DNS webhook protocol (Content-Type `application/external.dns.webhook+json;version=1`):

- `GET /` - domain filter negotiation
- `GET /records` - current state
- `POST /records` - apply changes
- `POST /adjustendpoints` - pre-store canonicalisation

Plus an ops server (separate port) for `/healthz`, `/readyz`, and `/metrics`. The provider port binds to loopback in the canonical deployment; the ops port binds to all interfaces so kubelet and Prometheus can reach it. Readiness probes credentials and connectivity every 15s.

### `main.go`

Reads env vars, configures logging, constructs the provider + webhook, and starts the protocol listener immediately. Background readiness verifies credentials and access to the configured zone without delaying ExternalDNS negotiation. The process runs until SIGINT/SIGTERM.

## Trade-offs and decisions

**Why no batched API?** Porkbun has no bulk endpoint - each create/edit/delete is one HTTP call. We serialize calls (rate limit) but parallelism wouldn't help anyway.

**Why a separate ops port?** The webhook protocol has no authentication. Keeping probes and metrics on `:8080` lets the mutating provider API bind only to `127.0.0.1:8888` while kubelet and Prometheus still reach operational endpoints.

**Why the official ExternalDNS sidecar chart?** Loopback is the protocol's security boundary. A standalone Service makes the provider's create/update/delete routes reachable by other workloads and is not the recommended upstream topology. Chart 0.4.0 wraps the official ExternalDNS chart; immutable standalone releases through 0.3.0 remain deprecated for migration history.

**Why distroless static?** Smallest possible image (~15MB total), no shell, no package manager, runs as nonroot UID 65532.

**Why no `/recordspec` support?** External-DNS's webhook v1 protocol doesn't include it; we follow v1 strictly. v2 is in proposal stage upstream - we'll add support when it lands.

## Failure modes

| Failure | Behavior |
|---|---|
| Porkbun API down (5xx) | Retry with backoff, then fail the apply; external-dns will retry next reconcile |
| Porkbun rate limit hit | Internal serialization keeps us below limit; if hit anyway, treat as transient |
| Credentials wrong | The webhook sidecar's `/readyz` probe goes red; logs and `edns_porkbun_ready` identify the failure |
| TXT record collision | external-dns's TXT registry handles ownership; we don't touch records without the owner-id TXT marker (when `policy=sync`) |
| Two webhooks managing overlapping names | The controllers can continuously undo each other's changes. Use non-overlapping `domainFilter` scopes and unique TXT ownership identifiers. |
