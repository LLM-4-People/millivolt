# Security

## Deployment boundary

millivolt is a trusted-operator tool, not a multi-user security boundary. It
has no authorization roles or tenant isolation. The whole dashboard is gated
by the shared `MILLIVOLT_OPERATOR_TOKEN` environment credential and denied
while it is unset (see [operator access](docs/operations.md#operator-access)):
there is one operator identity, no per-user accounts and no audit trail of
who acted. Only the `GET /healthz` liveness probe, the `POST /session` login
handshake, origin-root brand/PWA assets and transparent inference stay open at
the application gate; the canonical open-path list is
[operator access](docs/operations.md#operator-access). The upstream API key is
forwarded for inference; it does not secure the dashboard or administration.

The supplied listen address binds all interfaces. Prefer an explicit loopback
listen for local use. Before network exposure, restrict access with network
controls and an appropriately authenticated ingress. Protect the entire operator
surface, including dashboard/bootstrap/live records, SQL, Debug, exports,
Settings, Pause, Limits, purge and restart. Inference clients can also change
provider-wide limits through request headers.

For Docker, publish on host loopback, for example `-p 127.0.0.1:8080:8080`.
A non-root container and a public image do not authenticate callers or make a
publicly reachable operator listener safe. Protect persistent config/data
volumes as sensitive state, including backups and debug captures.

Browser same-origin mutation checks and framing denial are defense in depth
behind the operator credential, not a substitute for it. The live metrics feed
does not opt into cross-origin
browser sharing; the dashboard uses a same-origin EventSource authenticated
by the HttpOnly session cookie. This does not
prevent direct non-browser reads or secure a publicly reachable listener.
Explicit non-browser calls remain supported; gated endpoints need the
`Authorization: Bearer` credential or a session cookie minted through
`POST /session`. The `/mcp` endpoint is the exception: it requires the
operator credential as a Bearer header and refuses a session cookie alone with
`403`, because the caller's credential is forwarded on its internal calls.

## Upstream destinations and credentials

`X-Proxy-Base-URL` selects an HTTP(S) destination. An unrestricted caller can
therefore make the proxy contact destinations reachable from its host.
`allowed_base_urls` constrains accepted URL prefixes; use trusted destinations
and appropriate egress controls. This is not a DNS/IP isolation sandbox.
The proxy does not follow upstream redirects.

Ordinary metrics group credentials by SHA256 rather than retaining the raw key.
Known secret and configured authentication headers are redacted in retained
metadata/captures without changing forwarded headers. This does not sanitize
arbitrary text: request/response bodies, URLs, error messages, model names or
user-supplied labels can contain sensitive data. Preview capture and Debug
capture are opt-in, but enabling them can persist that data.

Token-refresh handback responses contain credentials in the error body for the client to adopt.
Do not publish them, browser exports, databases, backups, local config,
login-script output or screenshots containing private history.

Provider favicons can make browser requests to Google's favicon service, exposing
the provider hostname and the browser's network address to that service.
`referrerpolicy="no-referrer"` omits the dashboard referrer, not the requested
hostname. Hostname syntax checks do not establish that every qualified provider
label is public. The documentation capture workflow blocks all external browser
requests; that isolation is not the ordinary dashboard's network policy.

## Administrative data and resource limits

SQL is restricted to read-only queries with execution/output limits, not a
sandbox for adversarial resource consumption. Debug has bounded capture sizes
and retention, but captured bodies remain sensitive. Exports and backups can
outlive those retention settings.

Metrics storage is asynchronous and best-effort. Queue overflow/write failures
can lose records; this is not a tamper-proof, complete compliance audit log.
Do not rely on the dashboard as the sole security or billing record. See
[operations and limitations](docs/operations.md#storage-and-accounting).

## Code scanning

GitHub code scanning (CodeQL) reports on pull requests. Alerts are triaged
against the [deployment boundary](#deployment-boundary), not dismissed for
convenience. The current categories and their trust-boundary evidence:

- Operator-only read-only SQL (`internal/storage/query_admin.go`). The query
  console sits behind the operator gate, accepts one SELECT, and enforces
  execution time, row, column and result-byte limits. The operator is already
  trusted with the database, so this is not an unauthenticated SQL surface.
- JSON, SSE and gzip response writers. The operator surfaces behind the gate
  encode the proxy's own output, and framing and compression state are bounded
  by the response-size guard. Two writers are intentionally reachable without
  the operator gate: the `/healthz` liveness JSON and the transparent inference
  SSE relay, which streams the upstream response to a client that supplied its
  own provider credentials. Neither exposes operator data, so this category is
  not an authentication bypass.
- Bounded allocations (`cmd/proxy/mcp.go`, `internal/sse/frames.go`). The MCP
  recorder cap is the fixed `mcp.MaxResponseBytes` read budget (64 MiB) plus one
  byte, not a configurable limit. The SSE frame capacity is the marshaled
  payload length, bounded by the relay's scanner and encoder frame caps and the
  response-size guard. Input is rejected at those bounds before the sized
  allocation, so neither is an attacker-controlled unbounded size.

Two alerts were true positives and are fixed: the operator session cookie now
sets `Secure` when the request arrived over TLS or through an https-forwarding
ingress, and integer configuration values are range-checked against the
platform's `int` width before conversion. Dismissing or resolving a
code-scanning alert is a maintainer action in the GitHub security UI; this note
records triage rationale and does not dismiss any alert automatically.

## Scope

In scope: the proxy, dashboard, MCP server, container image, and repository
automation. Out of scope: findings that require a compromised host, an operator
who already holds the credential, a misconfigured ingress, or a third-party
provider. A report that depends on the documented trusted-operator boundary must
show how that boundary is crossed.

## Safe harbor

The maintainers will not pursue or support legal action for good-faith research
that stays within this scope, avoids privacy violations, service disruption, and
data destruction, and allows reasonable time to fix the issue before disclosure.

## Reporting a vulnerability

Do not publish credentials, private request content, or actionable exploit
details in a public issue.

Use the repository's enabled
[private vulnerability reporting](https://github.com/LLM-4-People/millivolt/security/advisories/new)
channel.

Include the affected revision, deployment assumptions, impact, and a minimal
reproduction using neutral local fixtures. Test only systems you own or are
explicitly authorized to assess.

## Supported versions

Security fixes target the latest published release. The `0.x` series is in
active development; older releases are not backported.

| Version | Supported |
| --- | --- |
| Latest published 0.x release | Yes |
| Older releases | No |

## Disclosure process

Reports are acknowledged on a best-effort basis; no response-time SLA is
promised. Maintainers will validate the report, prepare a fix, publish a GitHub
security advisory, and credit the reporter unless they prefer otherwise. Please
allow time for a coordinated fix before public disclosure.

Dependency review and a scheduled Go vulnerability scan run in CI. A clean scan
is not a security guarantee, and neither replaces a private report.

GitHub documents [private vulnerability reporting](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/configure-vulnerability-reporting/configure-for-a-repository).
