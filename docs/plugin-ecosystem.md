# Millivolt plugin ecosystem

This is the operator and author map for Millivolt's plugin system. It explains
what exists, what a plugin is allowed to do, how packages are verified and
installed, how the dashboard renders plugin output, and where the working demo
plugins live.

> Status: ABI v1 is implemented and end-to-end verified for local upload,
> GitHub release install by tag, `@latest` update, pinning, signature
> verification, rollback staging, and restart-bound loading. The SDK is a
> separate Go module; this repository owns the host, the management API, the
> dashboard surfaces, and the demos.

## 1. What problems this solves

Millivolt must remain a static, CGO-free, nonroot proxy. Go `.so` plugins,
sidecar processes, and browser-supplied guest code do not fit that shape. The
plugin system instead uses a small WebAssembly guest that observes finalized
requests and emits bounded, typed observations:

- **Rule packs** contribute declarative config fragments with no code execution.
- **Analyzer plugins** are WASM modules that receive redacted events after a
  request is recorded. They can emit logs, metrics, tool descriptors, and
  core-rendered UI panel descriptors.
- **Host-owned capabilities** gate every import. Guests never receive secrets,
  bodies, the operator token, or arbitrary filesystem/network access.
- **Signed packages** make distribution auditable. Remote installs require a
  trust file; pins bind the active archive bytes at rest and at boot.

The system is intentionally asymmetric: Millivolt core owns all rendering and
control; guests only supply data.

## 2. Component map

| Component | Location | Responsibility |
| --- | --- | --- |
| Core runtime | `internal/plugins/` | wazero host, capabilities, event bus, descriptors, credentials, fetch, rule packs |
| Package/install/update | `internal/plugins/source.go`, `install.go`, `update.go`, `trust.go` | ZIP validation, signatures, pins, GitHub source, rollback |
| Operator API | `cmd/proxy/plugins.go` | `/admin/plugins` GET/POST: verify, pin, install, uninstall, update check, update, rollback |
| Config surface | `internal/config/schema.go`, `rulepacks.go` | restart-required plugin settings and allowlisted rule-pack keys |
| Dashboard | `internal/web/static/js/live.js`, `settings.js`, `index.html` | Settings → Plugins tab, OpenCode tab, Focus view, escaped fixed fields |
| ABI contract | `millivolt-plugin-sdk/abi/v1/` | message shapes, caps, lifecycle exports/imports, bounds |
| SDK | `millivolt-plugin-sdk/` | Go bindings, mock host, manifest signing, conformance suite, examples |
| Demos | `demo-plugins/` | Two working plugins with source, package, and author/operator READMEs |
| Agent guide | `AGENTS.md` | repository invariants and task routing for coding agents |
| Skill | `skills/millivolt-plugin/SKILL.md` | reusable agent workflow for authoring, reviewing, and installing plugins |

The SDK is deliberately a separate module. Core never imports it; the wire
contract and manifest decoder are mirrored and checked by conformance tests.

## 3. Runtime lifecycle

1. **Boot** - core reads `plugin_dir`, `plugin_trust_file`, and
   `plugin_lock_file`. It validates every active `.mvplugin` archive, checks
   its signature and pins, then pre-instantiates up to 16 modules.
2. **Event** - after a request is recorded, the host emits
   `request.recorded` with a redacted `Record`. Config writes emit
   `config.changed`; operator commands emit `operator.command`.
3. **Handle** - the guest exports `mv_init`, `mv_handle`, and `mv_shutdown`.
   It reads the pending event with `mv_read_input`, returns a bounded JSON
   response with `mv_write_output`, and may call capability-gated host imports.
4. **Observation** - the host applies logs, metrics, tool descriptors, and
   panel descriptors. No plugin runs synchronously in the inference path.
5. **Rendering** - dashboard code renders only escaped, core-owned fields from
   descriptors. Guests cannot supply HTML, CSS, JavaScript, or event handlers.

Unknown ABI versions, unknown capabilities, malformed responses, oversized
messages, failed capability checks, and fuel/time/memory overruns are rejected.

## 4. Capabilities

| Capability | Host import | What a guest can do |
| --- | --- | --- |
| `log` | `mv_log` | Emit bounded host log lines. |
| `emit_metric` | `mv_emit_metric` | Emit bounded numeric observations with labels. |
| `read:records` | `mv_read_record` | Read the current redacted request record. |
| `read:config` | `mv_read_config_view` | Read the current non-secret config view. |
| `declare_tool` | `mv_declare_tool` | Declare an MCP tool descriptor. |
| `declare_ui_panel` | `mv_declare_ui_panel` | Declare a core-rendered panel of string fields. |
| `http:fetch` | `mv_http_fetch_request`, `mv_http_fetch_response` | Make a bounded GET to an exact operator-allowlisted host. |

Important limits:

- `http:fetch` is deny-by-default and only for HTTPS hosts in
  `plugin_http_allow_hosts`.
- `${env:NAME}` placeholders are expanded by the host, never by the guest.
- `credential_id: "request"` reuses the inference request's upstream credential
  with its own auth header and prefix. The secret value never enters guest
  memory, logs, records, descriptors, or responses.
- A denied call returns failure; a correct guest tolerates denial.

## 5. Manifest and package

A plugin is distributed exactly as one ZIP archive with `.mvplugin` suffix.

```text
manifest.json
module.wasm
LICENSE        optional
README.md      optional
```

The manifest is strict JSON with no unknown fields or trailing content:

```json
{
  "id": "opencode-usage-limits",
  "version": "0.7.1",
  "abi": "v1",
  "min_millivolt": "0.3.26",
  "capabilities": ["log", "emit_metric", "read:records", "read:config", "declare_tool", "declare_ui_panel", "http:fetch"],
  "module": "module.wasm",
  "module_sha256": "<sha256 of module.wasm>",
  "license": "MIT",
  "signer": "release",
  "signature": "<hex ed25519 signature>"
}
```

Bounds are host-owned: 32 MiB archive, 16 MiB module, 8 entries, 16 KiB
manifest, 16 active plugins, 16 inbox entries, one previous archive.

Two integrity layers apply:

1. `module_sha256` binds the WASM bytes inside the manifest.
2. `plugin_lock_file` pins the whole active archive.

The Ed25519 signature covers a fixed manifest payload. It does not cover ZIP
metadata; the pin covers archive bytes at rest.

See [plugin-package.md](plugin-package.md) for the full package contract.

## 6. Trust, install, update, rollback

### Local development

With no `plugin_trust_file`, unsigned development manifests can load. This is
for local experimentation only.

### Production-like installs

With `plugin_trust_file` configured, every archive must carry a valid signer
and signature. Remote installs always require a trust file.

Accepted install sources:

```text
github:owner/repo@vX.Y.Z
github:owner/repo@latest
https://github.com/owner/repo/releases/download/<tag>/<asset>.mvplugin
```

Branches, raw/blob URLs, non-GitHub hosts, HTTP, userinfo, query strings, and
fragments are rejected. GitHub API and asset CDN hosts are allowlisted; signed
CDN redirects with query parameters are accepted only on those asset hosts.

Install/update/rollback stage validated files and return
`restart_required: true`. The running WASM instance is never hot-swapped.

Update checks resolve `@latest`, compare semantic versions, ABI,
`min_millivolt`, signature, digest, and capabilities. Adding a capability
requires explicit operator confirmation.

See [plugin-package.md](plugin-package.md#update-contract) and the live demo in
[demo-plugins/opencode-usage-limits/README.md](../demo-plugins/opencode-usage-limits/README.md).

## 7. Dashboard surfaces

### Settings → Plugins

The install controls, module list, actions, and descriptor rows live in the
Settings sheet's Plugins tab. This is the only management surface in the
dashboard. The right API remains `/admin/plugins`.

### OpenCode Go tab

The OpenCode demo publishes a bounded panel with request, token, cost, rate
limit, and quota fields. Core renders it as fixed fields and progress bars.
The guest never supplies markup.

### Focus view

Any panel with `data.view = "focus"` can name another plugin and panel in
`include_plugin` / `include_panel`. Core composes that descriptor data into a
read-only Focus view. The included plugin is not modified or called by the
layout plugin.

### Hard UI limits

- No guest HTML, JS, CSS, URLs, event handlers, or scriptable components.
- All values are escaped and bounded.
- A plugin cannot restyle the dashboard or move core widgets by itself.
- Rich visual output is limited to text, unicode meters, and core-rendered
  progress bars.

## 8. Authoring workflow

1. Read [plugin-authoring.md](plugin-authoring.md) and the ABI contract.
2. Implement a guest against `millivolt-plugin-sdk/abi/v1`.
3. Test with the SDK mock host and `conformance.Run`.
4. Build WASI with
   `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`.
   A normal wasm build exports `_start` and fails host init.
5. Sign the manifest with Ed25519 when distributing.
6. ZIP exactly `manifest.json` and `module.wasm` into `.mvplugin`.
7. Install locally, restart, send an event, and inspect `/admin/plugins`.
8. For GitHub distribution, create a public repo and a semantic-versioned
   release with the `.mvplugin` asset.

The [SKILL.md](../skills/millivolt-plugin/SKILL.md) gives agents a compact
decision procedure for these steps.

## 9. Demo plugins

| Demo | Path | What it proves |
| --- | --- | --- |
| OpenCode Go usage limits | [demo-plugins/opencode-usage-limits](../demo-plugins/opencode-usage-limits/README.md) | Seven capabilities, request credential reuse, real quota, GitHub install and `@latest` update |
| Dashboard Focus | [demo-plugins/dashboard-focus](../demo-plugins/dashboard-focus/README.md) | Multi-plugin compatibility, bounded visual descriptors, Focus view composition |

Both demos are included with source, manifest, package, and an author/operator
README. They are working examples, not stubs.

## 10. Verification and troubleshooting

Author checks:

```sh
go test ./...
go run ./examples/analyzer/cmd
```

Core checks:

```sh
scripts/check.sh go test ./internal/plugins ./cmd/proxy
scripts/check.sh go test ./...
python3 -B -O -m tests.python.repository
```

Runtime diagnostics:

- `GET /admin/plugins` - modules, descriptors, failures, non-secret config.
- `GET /metrics/bootstrap` - dashboard-visible plugin status.
- `POST /admin/plugins?action=verify` - re-read and verify installed files.
- Host logs - capability denials, fetch failures, module errors.

Common failures:

| Symptom | Cause |
| --- | --- |
| `_start: module closed with exit_code(0)` | Built without `-buildmode=c-shared`. |
| `plugin source redirect is not allowed` | Query-bearing redirect from a non-CDN host, or old core before CDN query support. |
| `quota_status: unavailable` | No tracked request credential, or `opencode.ai` not allowlisted. |
| `plugin downloads are disabled` | `plugin_downloads_enabled` is false. |
| Update refused on capability change | Operator confirmation missing. |
| Module disappears after restart | Pin, signature, or archive validation failed; check logs and rollback state. |

## 11. Non-goals

ABI v1 does not include request/response bodies, request-path mutation,
arbitrary UI components, sidecars, automatic updates, registries beyond GitHub
releases, or ABI v2. Those need explicit design decisions and threat models.
