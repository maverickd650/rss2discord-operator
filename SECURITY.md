# Security Policy

## Supported Versions

This project releases continuously from `main` (see [Releases](https://github.com/maverickd650/rss2discord-operator/releases)).
Only the latest released version is supported — please upgrade before reporting an issue.

## Reporting a Vulnerability

Please report security vulnerabilities privately using
[GitHub's private vulnerability reporting](https://github.com/maverickd650/rss2discord-operator/security/advisories/new)
(Security tab → "Report a vulnerability"). Do not open a public issue for security reports.

You should receive an initial response within a few days. If the issue is confirmed, we'll work
with you on a fix and coordinate disclosure timing before a public advisory/release.

## Scope

As noted in the [README](README.md), this project is built with heavy AI assistance and has not
had a third-party security review — read the code before trusting it with anything sensitive.
Areas of particular interest for reports:

- SSRF guards in [`internal/rss/client.go`](internal/rss/client.go) (feed fetching) and
  [`internal/discord/client.go`](internal/discord/client.go) (webhook delivery) — these resolve
  and validate destination IPs/hosts before making requests.
- Webhook secret handling and log/error redaction.
- Discord message sanitization (`@everyone`/`@here` suppression, `javascript:`/`data:` URI
  stripping, length clamping).

## Supply Chain

Dependencies are kept current via Renovate, GitHub Actions are pinned to commit SHAs, and the
repo runs CodeQL and `govulncheck` on every push/PR plus a weekly schedule. Release artifacts
include a generated SBOM and are signed with cosign.
