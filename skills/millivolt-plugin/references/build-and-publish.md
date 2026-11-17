# Build, sign, and publish

## Build

From the SDK module:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
  -o dist/my-plugin.wasm ./cmd/my-plugin-wasm
```

Verify no `_start` export:

```sh
strings dist/my-plugin.wasm | grep '^_start$' || echo OK
```

A normal wasm build produces `_start`; the host init then fails with
`_start: module closed with exit_code(0)`.

## Manifest

Compute `module_sha256` from the exact WASM bytes. Keep the manifest strict:
no unknown fields, no trailing JSON, only closed capabilities.

```json
{
  "id": "my-plugin",
  "version": "0.1.0",
  "abi": "v1",
  "min_millivolt": "0.3.26",
  "capabilities": ["log", "emit_metric", "read:records", "declare_ui_panel"],
  "module": "module.wasm",
  "module_sha256": "<sha256>",
  "license": "MIT"
}
```

For distribution, add `signer` and `signature` from
`manifest.Manifest.Sign(signer, ed25519.PrivateKey)`. Never commit the private
key. The operator trust file carries only public keys.

## Package

The archive contains exactly:

```text
manifest.json
module.wasm
```

Optional `LICENSE` and `README.md` are informational only. No directories,
symlinks, absolute paths, duplicate names, or `ui.json`.

```sh
zip dist/my-plugin-0.1.0.mvplugin manifest.json module.wasm
```

## Tests before release

```sh
go test ./...
go run ./examples/analyzer/cmd
```

Assert observable behavior: correct provider filtering, bounded output, secret
non-exposure, denial tolerance, panel fields, version data, and package digest.
Do not add tests that only echo mocks or inspect source text.

## GitHub release

Create a public repo and a semantic-versioned release with the `.mvplugin`
asset. Accepted install forms:

```text
github:owner/repo@v0.1.0
github:owner/repo@latest
https://github.com/owner/repo/releases/download/v0.1.0/my-plugin-0.1.0.mvplugin
```

Requirements: `plugin_downloads_enabled: true`, configured `plugin_trust_file`,
valid signer, matching module digest, and an asset ending in `.mvplugin`.

Do not use branch names, raw/blob URLs, non-GitHub hosts, HTTP, userinfo, query
strings, or fragments.

## After publishing

Install or update in a clean proxy, restart, then check:

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/admin/plugins
```

Confirm `failures: 0`, expected version/capabilities, and descriptor data. For
`@latest`, also check `GET /admin/plugins/updates/check?id=...` and the staged
update response before restart.
