# Contributing

Thanks for your interest! This is a small but actively-maintained project, and contributions are welcome.

## Reporting bugs

Open an [issue](https://github.com/mattgmoser/external-dns-porkbun-webhook/issues) using the **Bug report** template. Include:
- What you tried (the Helm values, env vars, manifests, exact commands)
- What happened (logs, especially webhook + external-dns logs)
- What you expected
- Webhook version (`docker image inspect ghcr.io/mattgmoser/external-dns-porkbun-webhook | grep -i version`) and External-DNS version
- Kubernetes flavor + version (`kubectl version`)

If you can reproduce in [DRY_RUN mode](README.md#configuration), please include those logs — they're the most useful diagnostic.

## Asking for features

Feature requests via the **Feature request** issue template. Be specific about the use case — "X would be useful" beats "support X" every time. PRs always welcome.

## Submitting changes

1. Fork the repo and create a feature branch
2. `make lint test` should pass
3. Add tests for behavior changes — `provider/provider_test.go` uses an in-memory mock of the Porkbun API, so most tests don't need real credentials
4. Open a PR against `main`. The CI workflow will run `vet`, `gofmt`, `golangci-lint`, full test suite, and `helm lint`.

## Coding style

- `gofmt` enforced
- Public APIs need doc comments
- Logging via `github.com/sirupsen/logrus` with structured fields, not string concatenation
- New environment variables documented in [README.md](README.md) AND [main.go](main.go) header
- Don't add dependencies casually; prefer the standard library

## Running locally against real Porkbun

```sh
export PORKBUN_API_KEY=pk1_...
export PORKBUN_SECRET_API_KEY=sk1_...
export PORKBUN_DOMAIN=example.com
export DRY_RUN=true             # safe — won't apply changes
export LOG_LEVEL=debug
go run ./
```

Then in another terminal:

```sh
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz   # validates creds
curl -H "Content-Type: application/external.dns.webhook+json;version=1" http://localhost:8888/records
```

## Releasing

Maintainers tag a SemVer release (`vX.Y.Z`) on `main`. The `release.yaml` workflow:
1. Builds + pushes a multi-arch image to `ghcr.io/mattgmoser/external-dns-porkbun-webhook`
2. Releases the Helm chart from `charts/external-dns-porkbun-webhook` to GitHub Pages

Bump the chart's `version` and `appVersion` together with the binary version.

## Project structure

```
.
├── main.go                  # CLI entry; env-based config
├── porkbun/                 # Thin Porkbun JSON API client
├── provider/                # External-DNS Provider implementation
├── webhook/                 # HTTP server + webhook protocol + metrics
├── charts/                  # Helm chart
├── .github/workflows/       # CI and release
└── docs/                    # Architecture, reference
```

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be kind. Disagreements are fine; personal attacks are not.
