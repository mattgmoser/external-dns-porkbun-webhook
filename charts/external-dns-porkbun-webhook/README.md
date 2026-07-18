# external-dns-porkbun-webhook

This chart installs the official ExternalDNS chart with the Porkbun provider as
a same-Pod webhook sidecar. The provider protocol has no authentication, so its
mutation API binds only to `127.0.0.1:8888`. The Service exposes the separate
`:8080` health and metrics endpoint, not the provider API.

Chart `0.4.0` replaces the unsupported standalone topology from earlier
releases; `0.3.0` is explicitly marked deprecated. It depends on ExternalDNS
chart `1.21.1` (ExternalDNS `0.21.0`).

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
passed to the upstream chart. Use `helm show values ... --version 0.4.0` as
shown above for this chart's immutable defaults. The dependency's exact
[`external-dns` values](https://github.com/kubernetes-sigs/external-dns/blob/external-dns-helm-chart-1.21.1/charts/external-dns/values.yaml)
are also version-pinned.

The supplied values isolate the Kubernetes API token to the ExternalDNS
container. The webhook does not receive that token. Credential Secrets are not
created by this chart and are never deleted with the release.

## TXT representation

Porkbun stores TXT content as one unquoted string. The webhook removes one
matching outer pair of double quotes from ExternalDNS input. Common SPF, DKIM,
and verification values work, but multi-string segment boundaries are not
preserved as distinct segments and escaped embedded quotes may be normalized.
Verify records that rely on those presentation details after reconciliation.

## Migrating from chart 0.3.0 or earlier

Older charts installed only the webhook and expected a separately managed
ExternalDNS controller. Do not point the generic install command at that old
release. First record the controller's `txtOwnerId`, `txtPrefix`, domain
filters, and policy.

If the old chart created credentials from inline `porkbun.apiKey` values,
create an independently managed Secret before uninstalling or upgrading it,
preferably with a rotated key. Either operation removes that chart-owned Secret
from the release. If it used `porkbun.existingSecret`, verify the Secret remains
present and is not owned by the legacy Helm release.

Then choose one path:

- Keep an ExternalDNS release already managed through the official chart, add
  this project's
  [version-pinned sidecar values](https://github.com/mattgmoser/external-dns-porkbun-webhook/blob/v0.4.0/docs/external-dns-values.yaml),
  roll it out, and remove the old standalone webhook release.
- To adopt this wrapper, stop and remove the separately managed controller and
  old webhook release, then install `0.4.0` with the preserved ownership values
  and independent Secret.

Never overlap writable controllers for the same names. As a final guard,
`0.4.0` rejects every in-place upgrade unless
`migration.acknowledgeControllerReplacement=true` is explicitly set. The
acknowledgement confirms that you completed the handoff; it does not perform
the migration.

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
