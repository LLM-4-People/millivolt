# Author a Millivolt plugin

This guide walks from an idea to a signed, installable ABI v1 plugin. Read
[plugin-ecosystem.md](plugin-ecosystem.md) first for the security model and
component map.

## Choose the plugin class

Use a **rule pack** if you only need to contribute config fragments: aliases,
model rules, providers, request overrides, or sub-conversation rules. Rule packs
are data-only and need no WASM.

Use an **analyzer plugin** if you need to observe requests, compute bounded
metrics, declare MCP tools, publish dashboard panel data, or make an
operator-allowlisted outbound fetch. This guide covers analyzer plugins.

A v1 analyzer cannot rewrite traffic, read bodies, access the filesystem, use
the operator token, or render its own UI.

## 1. Set up the SDK

The SDK is a separate Go module:

```sh
git clone https://github.com/LLM-4-People/millivolt-plugin-sdk
cd millivolt-plugin-sdk
go test ./...
```

Important paths:

| Path | Use |
| --- | --- |
| `abi/v1/messages.go` | Source of truth for event/response shapes and caps. |
| `abi/v1/contract.md` | Export/import names and wire rules. |
| `guest/go/api.go` | Go guest interfaces and response validation. |
| `manifest/manifest.go` | Manifest validation and Ed25519 signing. |
| `mock/host.go` | In-process host for tests. |
| `conformance/conformance.go` | The behavioral suite your guest adapter must pass. |
| `cmd/usage-wasm/` | Reference WASI adapter. |
| `examples/usage-limits/` | Full-featured plugin using all seven capabilities. |
| `examples/analyzer/` | Minimal read-only analyzer. |

## 2. Implement the guest

A guest implements three lifecycle methods:

```go
type Guest interface {
    Init() error
    Handle(v1.Event) (*v1.Response, error)
    Shutdown() error
}
```

`Handle` receives exactly one event type:

- `request.recorded` with a redacted `Record`
- `config.changed` with a non-secret `ConfigView`
- `operator.command` with a bounded command

Return a `v1.Response` containing only what you need:

```go
resp := &v1.Response{}
resp.Metrics = append(resp.Metrics, v1.Metric{
    Name:   "my_plugin_requests",
    Value:  1,
    Labels: map[string]string{"model": rec.Model},
})
resp.Panels = append(resp.Panels, v1.UIPanel{
    ID:    "my-plugin",
    Title: "My plugin",
    Data: map[string]string{
        "requests": "1",
        "view":     "my-view",
    },
})
```

Guest output caps are strict: 64 logs, 64 metrics, 16 tools, 16 panels, 256 KiB
message size. The host rejects an over-cap response; it does not truncate it.

### Request-scoped credentials

If you declare `http:fetch`, the host can attach the credential of the request
that produced the event:

```go
v1.HTTPFetchRequest{
    URL:              "https://example.com/usage",
    CredentialID:     "request",
    CredentialHeader: "Authorization",
}
```

The value already includes the request's auth prefix. It never enters guest
memory. URL `${env:NAME}` placeholders are likewise expanded by the host.

## 3. Write the manifest

```json
{
  "id": "my-plugin",
  "version": "0.1.0",
  "abi": "v1",
  "min_millivolt": "0.3.26",
  "capabilities": ["log", "emit_metric", "read:records", "declare_ui_panel"],
  "module": "module.wasm",
  "module_sha256": "<sha256 of module.wasm>",
  "license": "MIT"
}
```

Rules:

- `id`: lowercase identifier, at most 64 characters.
- `version`: semantic version, optional leading `v`.
- `abi`: exactly `v1`.
- `capabilities`: only the closed vocabulary in `abi/v1/schema.json`.
- `module`: exactly `module.wasm`.
- `module_sha256`: lowercase hex SHA-256 of the WASM bytes.
- Unknown fields and trailing JSON are rejected.

Add only capabilities the guest actually needs. Extra capabilities are a
security and review cost.

## 4. Test before packaging

Use the mock host to prove behavior:

```go
func TestPublishesPanel(t *testing.T) {
    plugin := New()
    host := guest.NewMockHost("log", "emit_metric", "read:records", "declare_ui_panel")
    // drive an event through guest.Run and assert the panel fields
}
```

Then run the shared conformance suite with your adapter. The suite should pass
with no changes to `conformance/conformance.go`.

```sh
go test ./...
```

A plausible bug must fail a test: missing secret exposure, wrong provider
filter, wrong cap, wrong panel data, failed denial handling, or bad version
reporting are good tests. Mock echoes and source-text assertions are not.

## 5. Build the WASM module

Use WASI Preview 1 and c-shared mode:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
  -o dist/my-plugin.wasm ./cmd/my-plugin-wasm
```

A normal wasm build exports `_start`; Millivolt's host expects the reactor-style
`mv_init`, `mv_handle`, and `mv_shutdown` exports. Starting a module with an
unexpected `_start` fails with:

```text
_start: module closed with exit_code(0)
```

Always check the built module with `strings module.wasm | grep '^_start$'`;
there should be no `_start` export.

## 6. Sign and package

For local development, unsigned manifests are accepted only when no trust file
is configured. For distribution, generate an Ed25519 key pair, put the public
key in the operator's `plugin_trust_file`, and sign:

```go
m.Sign("my-signer", privateKey)
```

Then create the archive with exactly two required entries:

```sh
zip dist/my-plugin-0.1.0.mvplugin manifest.json module.wasm
```

Do not add loose files, directories, `ui.json`, symlinks, or extra entries.
Never commit the private key.

## 7. Install locally

Settings → Plugins supports local upload, inbox scan, and GitHub source. The API
equivalent is:

```sh
curl -X POST http://127.0.0.1:8080/admin/plugins/install \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -F "file=@dist/my-plugin-0.1.0.mvplugin"
```

Restart Millivolt, then inspect:

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/admin/plugins
```

Look for `failures: 0`, expected capabilities, and the panel/tool descriptors.
Send a request that emits the expected event and check the dashboard.

## 8. Publish on GitHub

Create a public repo and a semantic-versioned release with the `.mvplugin`
asset. Then install with:

```text
github:owner/repo@v0.1.0
github:owner/repo@latest
```

Requirements:

- `plugin_downloads_enabled: true`
- configured `plugin_trust_file`
- signed manifest with a signer in that trust file
- asset filename ending in `.mvplugin`
- release tag equal to a semantic version

Update flow after publishing a newer release:

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  'http://127.0.0.1:8080/admin/plugins/updates/check?id=my-plugin'

curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"my-plugin","source":"github:owner/repo@latest"}' \
  http://127.0.0.1:8080/admin/plugins/update
```

Restart to load the staged artifact. Use rollback if the new module fails.

## 9. UI descriptor contract

Panels carry bounded string data. Use them as data, not as markup:

```go
v1.UIPanel{
    ID:    "my-plugin",
    Title: "My plugin",
    Data: map[string]string{
        "view":      "focus",
        "heading":   "Traffic at a glance",
        "requests":  "12",
        "arrange":   "quota,traffic,plugins",
        "note":      "▰ REQ 12 · ▰ ERR 0",
    },
}
```

Core may render fixed fields, progress bars, and a Focus view that composes a
named plugin/panel. It will never execute guest HTML or CSS. Unicode meters are
plain text and are safe; colors, spacing, chart types, and widget placement are
core-owned.

## 10. Author checklist

- [ ] Least capability set.
- [ ] No secrets, bodies, raw URLs with credentials, or operator token in output.
- [ ] Response caps respected.
- [ ] Denied host calls handled gracefully.
- [ ] Conformance suite passes.
- [ ] Manifest digest matches module bytes.
- [ ] Package contains only allowed entries.
- [ ] Signature verified by trust file.
- [ ] Local install and restart tested.
- [ ] GitHub tag install and `@latest` update tested if distributed.
- [ ] README explains operator config and expected dashboard output.

## 11. Where to look next

- [plugin-ecosystem.md](plugin-ecosystem.md) - full architecture and limits.
- [plugin-package.md](plugin-package.md) - package/update/rollback contract.
- [demo-plugins](../demo-plugins/) - two working examples.
- [SKILL.md](../skills/millivolt-plugin/SKILL.md) - agent workflow.
