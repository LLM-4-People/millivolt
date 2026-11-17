# Plugin system

> Status: **implemented** in millivolt core. The ABI contract and SDK live in
> the separate `millivolt-plugin-sdk` repository; the bb plugin is not part of
> this system.

millivolt now includes a data-only rule-pack loader, scoped plugin credentials,
an embedded WASM analyzer runtime, a gated management API, and a core-rendered
dashboard view. The decisions below remain the contract those pieces honor.

## Context

The constraints come from the existing service, not from preference:

- The image is a CGO-free static binary on a nonroot distroless base
  ([Dockerfile](../Dockerfile), lines 16, 20 and 28). Go `.so` plugins are not a
  viable target.
- Configuration already has one registry that the dashboard, YAML writer,
  `GET /admin/config` and `Apply` all read
  ([internal/config/schema.go](../internal/config/schema.go), lines 31-52), with
  validation and caps at the load boundary
  ([internal/config/config.go](../internal/config/config.go), lines 635 and
  2228-2280) and credential-owned header protection (lines 2358-2362).
- The operator plane is one shared credential, with no per-user accounts
  ([SECURITY.md](../SECURITY.md), lines 5-9). The MCP server's
  `readOnly`/`mutating`/`destructive` labels are SDK annotations, not
  authorization ([internal/mcp/server.go](../internal/mcp/server.go), lines
  97-260).
- Operator namespaces are explicitly reserved so look-alikes cannot fall
  through to the inference catch-all
  ([cmd/proxy/main.go](../cmd/proxy/main.go), lines 85-124). The dashboard is
  embedded static content behind an allowlisted `/dash/` prefix with
  `frame-ancestors 'none'`
  ([internal/web/web.go](../internal/web/web.go), lines 25-29, 302 and 384-387).
- The request path already owns bounded body reads, overrides, translation and
  admission ([internal/proxy/proxy.go](../internal/proxy/proxy.go), lines
  464-831). A synchronous plugin call there would add latency and allocation to
  every inference request.

## Decision

### 1. Scope

The plugin system belongs to millivolt core. A separate repository,
`millivolt-plugin-sdk`, holds generated guest bindings, a mock host, the
manifest schema and a conformance suite. The existing bb plugin is not part of
this work; core will expose a stable `/admin/plugins` surface that a bb plugin
can wrap later.

### 2. Plugin classes

Version 1 supports two classes only:

1. **Rule pack** - declarative fragments contributed to the existing config
   schema. Allowed keys: `aliases`, `model_rules`, `providers`,
   `request_overrides` and `sub_conversations`. No code execution.
2. **Analyzer** - a read-only WASM guest that consumes events after a request
   has been recorded. It may emit metrics, logs and descriptors; it may not
   alter a request or response.

Deferred: UI panels that render arbitrary guest HTML, request-path mutation,
and sidecar plugins that need a filesystem or network. Those are separate
decisions with their own threat models.

### 3. Runtime

Embed [wazero](https://github.com/wazero/wazero) in the proxy process. It is a
pure-Go, zero-dependency, CGO-free WebAssembly runtime, which fits the current
build. Modules are verified by digest and pre-instantiated at boot with bounded
fuel, memory and wall-clock time. Host functions are named imports; there is no
blanket WASI filesystem or network grant.

Version 1 uses core WebAssembly with an explicit, versioned host ABI, not the
Component Model. wazero's maintainers have
[declined WASI Preview 2 and Component Model support](https://github.com/wazero/wazero/issues/2289)
until the standard reaches W3C recommendation. Component Model support would be
a second host adapter, not a change to the public SDK API.

Docker's WASM workload mode is explicitly not a foundation: it is Beta and
deprecated in Docker Desktop. The
[runwasi precompiled-layer advisory](https://github.com/containerd/runwasi/security/advisories/GHSA-cc25-vq59-rcjx)
is a further reason to verify raw module bytes and never trust precompiled
artifacts from plugin images.

### 4. Contract and SDK

The contract is the source of truth; SDKs are generated from it. It defines:

- lifecycle exports `init`, `handle` and `shutdown`;
- events `request.recorded`, `config.changed` and `operator.command`;
- bounded host imports `log`, `emit_metric`, `read_record`, `read_config_view`,
  `declare_tool` and `declare_ui_panel`;
- a manifest with `id`, `version`, `abi`, `min_millivolt`, capabilities,
  module `sha256` and license.

Every host import checks the plugin's granted capability and message size. The
SDK ships a mock host so authors can test without a proxy, plus a conformance
suite the core also runs.

### 5. Security boundary

Plugins are untrusted. Before any plugin can call an operator API, credentials
gain capability scopes; the existing full operator token remains the superset.
Scopes filter the registered MCP tools, and the per-credential server cache key
must include the scope. Plugin credentials are stored hashed and outside
`proxy.yaml`, so backup and restore do not export them.

Plugins never receive the operator token, upstream keys, or raw request and
response bodies by default. A plugin that needs a body preview is a later,
explicit operator decision.

### 6. Host surface

- Rule packs merge through the existing `Validate()` path and inherit all its
  caps and protected-header rules. They cannot change boot-bound fields such as
  `listen`, `db_path` or storage settings.
- Analyzer events are published after `s.rec.Record(rec)`
  ([internal/proxy/proxy.go](../internal/proxy/proxy.go), line 729). There is no
  synchronous hook in `ServeHTTP` in version 1.
- `/plugins` and `/plugins/` are reserved alongside `/admin`, `/metrics` and
  `/mcp`. Management lives on gated `/admin/plugins` routes.
- Plugin settings are added to the single config schema registry as
  restart-required keys.

## Consequences

- A new dependency (wazero) enters the core build; the static, CGO-free build
  must remain intact.
- The host imports become the attack surface. WebAssembly sandboxes execution,
  not the functions we expose.
- ABI versioning is a permanent maintenance obligation. A plugin declaring an
  unknown ABI version must be rejected with a clear error.
- Event delivery must stay off the inference hot path until there is evidence
  that a bounded, pre-instantiated call is cheap enough.

## Alternatives rejected

- **Go `.so` plugins** - require CGO, matching toolchain build IDs and shared
  libraries; incompatible with the static distroless image.
- **Docker WASM containers** - Beta/deprecated on this platform, with a shim
  supply-chain history.
- **Component Model now** - not supported by the chosen runtime and still
  moving between preview releases.
- **Synchronous webhooks in the request path** - couples inference availability
  to plugin latency and breaks the existing allocation discipline.

## Verification

- Targeted Go checks via `scripts/check.sh go test`, with race coverage for the
  event bus (see [CONTRIBUTING](../CONTRIBUTING.md), lines 62-79).
- The SDK conformance suite runs against the core host in CI.
- Live smoke: rule pack reload, scoped-token denial, analyzer event delivery,
  fuel timeout, disable/re-enable, and restart.

## Implemented surface

- Rule packs contribute only allowlisted config fields and pass the normal
  `Validate()` boundary.
- Scoped plugin credentials are Bearer-only on `/mcp`; every MCP tool and
  internal operator route is capability-checked.
- The embedded wazero host enforces module size, memory, call-time, digest and
  export bounds. Host imports are capability-checked and never expose bodies,
  keys or filesystem; the generic http:fetch import is deny-by-default and
  restricted to exact operator-allowlisted hosts.
- `GET /admin/plugins` reports loaded modules, descriptors and non-secret
  config. `POST /admin/plugins` accepts `verify` and `pin`.
- `plugin_trust_file` supplies Ed25519 verifiers. When configured, every
  manifest must carry a valid `signer`/`signature`; without it, unsigned
  development manifests still load.
- `plugin_lock_file` pins module filenames to digests. Pins are written by
  `action=pin` and enforced at the next boot.
- The Settings sheet's Plugins tab owns install, verify, pin, update, rollback
  and uninstall controls. It renders only escaped, core-controlled text from
  guest descriptors.
- `millivolt-plugin-sdk` ships ABI v1 types, manifest signing, a mock host,
  conformance checks and a reference analyzer.
- Panel descriptors carry bounded string `data`. The core exposes it through
  `/admin/plugins` and the bootstrap status, and the dashboard renders it only
  as escaped fixed fields.
- The OpenCode Go reference plugin uses all seven ABI capabilities, including
  allowlisted `http:fetch` with host-owned credential attachment. Its signed
  package and trust file are built in `millivolt-plugin-sdk/dist`, and the
  dashboard has a dedicated OpenCode tab for its panel data.
- A panel descriptor with `data.view = "focus"` may name another plugin and
  panel in `include_plugin`/`include_panel`. The core composes that bounded
  descriptor data into a read-only Focus view; the OpenCode plugin stays
  unchanged and guest markup is never rendered.

Distribution, install, update, rollback and boot recovery are implemented as
specified in [Plugin package](plugin-package.md). The final gate covers the full
signed-release lifecycle: source install, verify, pin, restart, update check,
capability confirmation, update, rollback, tamper rejection, boot recovery and
uninstall. A real public GitHub release was used to verify tag install and
`@latest` update. Publishing your own signed release remains a release-operator
step; see [Plugin ecosystem](plugin-ecosystem.md) and the working
[demo plugins](../demo-plugins/).

## Not included

- Custom views that need more than core-rendered descriptor data.
- Registry distribution beyond a GitHub release asset.
- Non-Go guest bindings and sidecar plugins.
- A future bb plugin wrapper around `/admin/plugins` (separate project).
