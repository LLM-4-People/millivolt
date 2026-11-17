# Demo plugin: OpenCode Go usage limits

A working ABI v1 analyzer plugin that tracks OpenCode Go requests and reads the
real subscription limits from OpenCode's authenticated usage API. It is the
reference for all seven v1 capabilities and the host-owned request-credential
pattern.

## What it does

- Filters `request.recorded` events to provider `opencode-go`.
- Aggregates requests, models, input/output tokens, cost, rate limits, and the
  last model.
- Reads non-secret config through `read:config` and reports schema/value counts.
- Declares the `opencode_usage_summary` MCP tool.
- Publishes a core-rendered panel with request metrics and quota windows.
- Uses `http:fetch` with `credential_id: "request"` against
  `https://opencode.ai/zen/go/v1/usage`. The host attaches the API key from the
  tracked inference request; the guest never sees it.
- Publishes `plugin_version` in its panel data so updates are observable.

## Capabilities

`log`, `emit_metric`, `read:records`, `read:config`, `declare_tool`,
`declare_ui_panel`, `http:fetch`.

Do not add capabilities without operator confirmation on update.

## Files

| File | Purpose |
| --- | --- |
| `usage.go` | Guest logic and panel data. |
| `usage_test.go.example` | Behavior tests; copy as `usage_test.go` into the SDK before running Go tests. |
| `cmd/usage-wasm/main.go` | WASI adapter exporting `mv_init`, `mv_handle`, `mv_shutdown`. |
| `cmd/usage-wasm/main_nonwasi.go` | Native stub so `go test` works. |
| `go.mod` | Self-contained nested SDK module for local builds. |
| `manifest.json` | Signed 0.7.1 manifest. |
| `module.wasm` | Built WASI module. |
| `opencode-usage-limits-0.7.1.mvplugin` | Signed installable package. |
| `SETUP.md` | Original operator setup notes. |

## Operator config

```yaml
plugins_enabled: true
plugin_dir: "/plugins"
plugin_trust_file: "/config/trust.json"
plugin_lock_file: "/plugins/plugins.lock.json"
plugin_http_allow_hosts:
  - "opencode.ai"
```

For GitHub installs also set `plugin_downloads_enabled: true`.

## Install and observe

Local upload:

```sh
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -F "file=@opencode-usage-limits-0.7.1.mvplugin" \
  http://127.0.0.1:8080/admin/plugins/install
```

GitHub release:

```text
github:hv-V6/millivolt-opencode-usage-limits@v0.7.1
github:hv-V6/millivolt-opencode-usage-limits@latest
```

Restart, then send one OpenCode Go request through the proxy with its normal
Authorization header. Check:

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/admin/plugins
```

Expected panel data includes `quota_status: ok`, rolling/weekly/monthly quota
strings, and `plugin_version: 0.7.1`. If no credential is available the panel
honestly reports `quota_status: unavailable`.

## Build from source

This folder is a self-contained nested Go module. Copy `usage_test.go.example`
to `usage_test.go` to run the behavior tests locally, then:

```sh
go test ./...
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
  -o module.wasm ./cmd/usage-wasm
```

The `-buildmode=c-shared` flag is required. A plain wasm build exports `_start`
and fails host init.

After changing the module, recompute `module_sha256`, re-sign the manifest,
and repackage exactly `manifest.json` and `module.wasm`.

## Verified behavior

- Local signed install: verified.
- GitHub release install by tag: verified.
- GitHub `@latest` update 0.7.0 → 0.7.1: verified with signature check, pin
  update, restart, and `plugin_version: 0.7.1` in live descriptor data.
- Real quota from OpenCode: rolling, weekly, and monthly values observed.

## Security notes

- No API key is stored in records, logs, descriptors, package files, or this
  README.
- The URL is core-allowlisted; the guest cannot fetch arbitrary hosts.
- The guest never reads request or response bodies.
- Panel fields are escaped fixed text; no HTML or JavaScript is supplied.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `quota_status: unavailable` | Was there a tracked request with an OpenCode credential? Is `opencode.ai` allowlisted? |
| `plugin downloads are disabled` | Set `plugin_downloads_enabled: true`. |
| `signature verification failed` | Add the release signer public key to `plugin_trust_file`. |
| Module missing after restart | Inspect `/admin/plugins`, host logs, pin, and previous archive. |
| `_start` init failure | Rebuild with `-buildmode=c-shared`. |

See the root [plugin ecosystem guide](../../docs/plugin-ecosystem.md) and
[authoring guide](../../docs/plugin-authoring.md).
