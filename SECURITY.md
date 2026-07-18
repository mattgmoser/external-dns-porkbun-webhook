# Security policy

## Supported versions

The latest tagged release is the only supported version. Older versions may receive security patches at the maintainer's discretion.

## Reporting a vulnerability

**Please do not report security issues via public GitHub issues.**

Instead, [open a private security advisory](https://github.com/mattgmoser/external-dns-porkbun-webhook/security/advisories/new) on the repository.

When reporting, include:
- A description of the issue and its impact
- Steps to reproduce (or a proof-of-concept)
- Affected versions
- Any mitigations you've identified

You'll get acknowledgement within 72 hours and a target resolution within 14 days for high-severity issues.

## What's in scope

- The webhook binary and its handling of Porkbun credentials
- The Helm chart (RBAC, secret handling, container security)
- Container image (build process, supply chain)

## Deployment security boundary

The ExternalDNS webhook protocol does not authenticate callers. The supported deployment runs the webhook in the ExternalDNS Pod and binds its provider endpoint to `127.0.0.1:8888`; the separate `:8080` endpoint is for probes and metrics. Treat any deployment that exposes port `8888` to other Pods or networks as security-sensitive. The repository's old standalone chart is deprecated for this reason.

## What's out of scope

- Vulnerabilities in upstream External-DNS - report those at https://github.com/kubernetes-sigs/external-dns/security
- Vulnerabilities in Porkbun's API itself - report those to porkbun
- Issues that require a malicious operator with `cluster-admin` permission already
