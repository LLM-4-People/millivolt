# Security

## Deployment boundary

millivolt is a trusted-operator tool, not a multi-user security boundary. It has
no built-in operator authentication, authorization roles or tenant isolation.
The upstream API key is forwarded for inference; it does not secure the
dashboard or administration.

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

Browser same-origin mutation checks and framing denial are defense in depth,
not authentication. The live metrics feed does not opt into cross-origin
browser sharing; the dashboard uses a same-origin EventSource. This does not
prevent direct non-browser reads or secure a publicly reachable listener.
Explicit non-browser calls remain supported.

## Upstream destinations and credentials

`X-Proxy-Base-URL` selects an HTTP(S) destination. An unrestricted caller can
therefore make the proxy contact destinations reachable from its host.
`allowed_base_urls` constrains accepted URL prefixes; use trusted destinations
and appropriate egress controls. This is not a DNS/IP isolation sandbox.
Upstream redirects are returned without following them.

Ordinary metrics group credentials by SHA256 rather than retaining the raw key.
Known secret and configured authentication headers are redacted in retained
metadata/captures without changing forwarded headers. This does not sanitize
arbitrary text: request/response bodies, URLs, error messages, model names or
user-supplied labels can contain sensitive data. Preview capture and Debug
capture are opt-in, but enabling them can persist that data.

Token-refresh response headers contain credentials for the client to adopt.
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

## Reporting a vulnerability

Do not publish credentials, private request content, or actionable exploit
details in a public issue.

Use the repository's enabled
[private vulnerability reporting](https://github.com/LLM-4-People/millivolt/security/advisories/new)
channel. No supported release matrix or response-time commitment has been
established here.

Include the affected revision, deployment assumptions, impact, and a minimal
reproduction using neutral local fixtures. Test only systems you own or are
explicitly authorized to assess.

GitHub documents [private vulnerability reporting](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/configure-vulnerability-reporting/configure-for-a-repository).
