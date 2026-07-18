# external-dns-porkbun-webhook (deprecated standalone chart)

> **Security warning:** this chart is deprecated. The ExternalDNS webhook protocol has no authentication, and this topology exposes its DNS mutation endpoint through a ClusterIP Service. A compromised workload with network access to that Service could ask the webhook to change records using the webhook's Porkbun credentials.

For new installations, run this image as a same-Pod sidecar through the official ExternalDNS chart. The canonical, version-pinned values are in [`docs/external-dns-values.yaml`](../../docs/external-dns-values.yaml), and the project [README](../../README.md#quickstart-helm) contains the complete installation procedure. That topology binds the provider endpoint to `127.0.0.1:8888` and exposes only the separate ops endpoint.

This legacy chart remains available so existing installations have a migration window. Installing or upgrading it requires an explicit risk acknowledgement.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.8+
- A Porkbun domain with API access enabled
- An existing Secret containing `PORKBUN_API_KEY` and `PORKBUN_SECRET_API_KEY`

## Legacy installation

```sh
helm repo add edns-porkbun https://mattgmoser.github.io/external-dns-porkbun-webhook
helm repo update

helm upgrade --install porkbun-webhook edns-porkbun/external-dns-porkbun-webhook \
  --version 0.3.0 \
  --namespace external-dns \
  --set legacyStandalone.acceptRisk=true \
  --set porkbun.domain=example.com \
  --set porkbun.existingSecret.name=porkbun-creds
```

Do not expose the webhook port through an Ingress, Gateway, LoadBalancer, or NodePort. If this topology must remain, enforce a NetworkPolicy that permits port `8888` only from the intended ExternalDNS Pod and port `8080` only from kubelet/monitoring traffic. NetworkPolicy support depends on the cluster CNI and does not make the protocol authenticated.

## Values

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `legacyStandalone.acceptRisk` | bool | `false` | Required acknowledgement for the deprecated topology |
| `porkbun.domain` | string | `""` | Apex Porkbun zone (required) |
| `porkbun.domainFilter` | list | `[]` | Subdomain filter; defaults to the Porkbun zone |
| `porkbun.existingSecret.name` | string | `""` | Existing credential Secret (recommended) |
| `porkbun.apiKey` | string | `""` | Inline key for disposable testing only |
| `porkbun.secretApiKey` | string | `""` | Inline secret key for disposable testing only |
| `dryRun` | bool | `false` | Log changes without applying them |
| `cacheTTL` | string | `1m` | In-memory record cache duration |
| `containerPorts.webhook` | int | `8888` | Provider listener and target port |
| `containerPorts.ops` | int | `8080` | Health and metrics listener and target port |
| `replicaCount` | int | `1` | Must be 0 or 1 because rate limiting is process-local |
| `serviceMonitor.enabled` | bool | `false` | Create a Prometheus Operator ServiceMonitor |

See [`values.yaml`](values.yaml) for logging, resources, security contexts, probes, scheduling, and ServiceMonitor settings.

The chart fails before installation when the risk acknowledgement, domain, or a complete credential source is missing. Its Pod does not mount a Kubernetes ServiceAccount token because the standalone webhook never calls the Kubernetes API. Inline Secret changes trigger a Pod rollout; externally managed Secret rotation still requires a rollout or a Secret-reloader controller because the process reads credentials from environment variables at startup.

## Uninstall

```sh
helm uninstall porkbun-webhook -n external-dns
```

An externally created credential Secret is not managed by this chart and is not removed.
