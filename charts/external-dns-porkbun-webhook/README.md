# external-dns-porkbun-webhook

This chart installs the official ExternalDNS chart with the Porkbun provider as
a same-Pod webhook sidecar. The provider protocol has no authentication, so its
mutation API binds only to `127.0.0.1:8888`. The Service exposes the separate
`:8080` health and metrics endpoint, not the provider API.

Chart `0.4.0` replaces the deprecated standalone topology from `0.3.0` and
earlier. It depends on ExternalDNS chart `1.21.1` (ExternalDNS `0.21.0`).

## Prerequisites

- Kubernetes 1.24+
- Helm 3.8+
- A Porkbun domain with API access enabled
- An existing Secret containing `PORKBUN_API_KEY` and
  `PORKBUN_SECRET_API_KEY`

Use a dedicated Porkbun key restricted to the managed domain. If the cluster
has a stable egress address, restrict the key to that IP as well.

## Install

```sh
kubectl create namespace external-dns --dry-run=client -o yaml | kubectl apply -f -
kubectl -n external-dns create secret generic porkbun-creds \
  --from-literal=PORKBUN_API_KEY="$YOUR_API_KEY" \
  --from-literal=PORKBUN_SECRET_API_KEY="$YOUR_SECRET_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -

helm repo add edns-porkbun https://mattgmoser.github.io/external-dns-porkbun-webhook
helm repo update
helm show values edns-porkbun/external-dns-porkbun-webhook \
  --version 0.4.0 > external-dns-porkbun-values.yaml
```

Edit every example value in `external-dns-porkbun-values.yaml`, especially:

- the credential Secret name;
- `PORKBUN_DOMAIN`, `DOMAIN_FILTER`, and `domainFilters`;
- `txtOwnerId`, which must be stable and unique per cluster;
- `txtPrefix`, which must not collide with another ExternalDNS instance.

Then install:

```sh
helm upgrade --install external-dns \
  edns-porkbun/external-dns-porkbun-webhook \
  --version 0.4.0 \
  --namespace external-dns \
  --values external-dns-porkbun-values.yaml
```

The default policy is `upsert-only`. Confirm the ownership TXT records before
choosing `sync`, which also deletes records no longer desired by Kubernetes.

## Values

All provider and controller settings are nested under `external-dns` and are
passed to the upstream chart. See this chart's
[`values.yaml`](https://github.com/mattgmoser/external-dns-porkbun-webhook/blob/main/charts/external-dns-porkbun-webhook/values.yaml)
and the upstream
[`external-dns` values](https://artifacthub.io/packages/helm/external-dns/external-dns?modal=values).

The supplied values isolate the Kubernetes API token to the ExternalDNS
container. The webhook does not receive that token. Credential Secrets are not
created by this chart and are never deleted with the release.

## Migrating from chart 0.3.0 or earlier

The chart rejects an in-place upgrade that still carries legacy standalone
values: older versions installed only the webhook and expected a separate
ExternalDNS controller. First record the existing controller's `txtOwnerId`,
`txtPrefix`, domain filters, policy, and Secret name. Remove the old standalone
webhook and ensure only one writable ExternalDNS controller remains, then
install this chart with those ownership values preserved.

If ExternalDNS is already managed directly through the official upstream
chart, it is also valid to keep that release and use this project's
[version-pinned sidecar values](https://github.com/mattgmoser/external-dns-porkbun-webhook/blob/main/docs/external-dns-values.yaml)
instead of adopting the wrapper.

## Operations

```sh
kubectl -n external-dns get pods
kubectl -n external-dns logs deployment/external-dns -c external-dns
kubectl -n external-dns logs deployment/external-dns -c webhook
kubectl -n external-dns rollout restart deployment/external-dns
```

Restart after rotating Porkbun credentials because the webhook reads its
environment at process start.

## Uninstall

```sh
helm uninstall external-dns -n external-dns
```

Externally managed credential Secrets remain in place.
