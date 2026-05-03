# external-dns-porkbun-webhook

Helm chart that deploys the [external-dns Porkbun webhook provider](https://github.com/mattgmoser/external-dns-porkbun-webhook) as a sidecar to upstream [external-dns](https://kubernetes-sigs.github.io/external-dns/), letting external-dns manage DNS records on [Porkbun](https://porkbun.com/) automatically.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.8+
- Porkbun account with API access enabled (free, but you must opt in per domain in the Porkbun dashboard)
- A Porkbun **API Key** and **Secret API Key**

## Install

Add the repo:

```sh
helm repo add edns-porkbun https://mattgmoser.github.io/external-dns-porkbun-webhook
helm repo update
```

Create a Secret with your Porkbun credentials:

```sh
kubectl create namespace external-dns
kubectl -n external-dns create secret generic porkbun-creds \
  --from-literal=PORKBUN_API_KEY="$YOUR_API_KEY" \
  --from-literal=PORKBUN_SECRET_API_KEY="$YOUR_SECRET_KEY"
```

Install the webhook:

```sh
helm install porkbun-webhook edns-porkbun/external-dns-porkbun-webhook \
  --namespace external-dns \
  --set porkbun.domain=example.com \
  --set porkbun.existingSecret.name=porkbun-creds
```

Then install upstream external-dns and point it at this webhook's Service. See the project README for the full example.

## Values

The most commonly changed values:

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `porkbun.domain` | string | `""` | Apex zone managed by Porkbun (required) |
| `porkbun.domainFilter` | list | `[]` | Subdomain filter; defaults to `[porkbun.domain]` |
| `porkbun.existingSecret.name` | string | `""` | Name of an existing Secret with `PORKBUN_API_KEY` and `PORKBUN_SECRET_API_KEY` |
| `porkbun.apiKey` | string | `""` | Inline API key (use `existingSecret` instead in production) |
| `porkbun.secretApiKey` | string | `""` | Inline secret API key |
| `dryRun` | bool | `false` | Log changes without applying |
| `cacheTTL` | string | `1m` | In-memory cache for `Records()` calls |
| `logLevel` | string | `info` | `debug`, `info`, `warn`, `error` |
| `logFormat` | string | `text` | `text` or `json` |
| `replicaCount` | int | `1` | Should stay at 1 (Porkbun rate limits to 1 req/sec) |
| `serviceMonitor.enabled` | bool | `false` | Create a Prometheus Operator ServiceMonitor |

See [`values.yaml`](values.yaml) for the full list including pod security context, resource limits, probes, and ServiceMonitor options.

## Probes

Health and readiness probes are exposed on the **ops** port (`8080` by default), not the webhook port (`8888`). The chart's Service exposes both. If you write a custom Service, do not omit the ops port or the kubelet probes will fail.

## Uninstall

```sh
helm uninstall porkbun-webhook -n external-dns
```

The Secret you created with credentials is not managed by this chart and will not be removed automatically.

## Source

- Project repository: https://github.com/mattgmoser/external-dns-porkbun-webhook
- Issue tracker: https://github.com/mattgmoser/external-dns-porkbun-webhook/issues
- License: Apache-2.0
