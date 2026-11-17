# Demo plugin: dashboard focus

A working ABI v1 analyzer plugin that tests multi-plugin coexistence and
pushes the dashboard's text-descriptor UI as far as plugin-only rules allow.
It publishes a Focus layout descriptor and eight bounded descriptor rows with
unicode meters, traffic summaries, and a composed OpenCode quota panel.

## What it does

- Aggregates request count, errors, rate limits, tokens, cost, providers,
  clients, and models from `request.recorded` events.
- Publishes one `view=focus` layout descriptor naming
  `opencode-usage-limits` as the included plugin/panel.
- Publishes eight core-rendered panels: Focus layout, traffic, health, tokens,
  models, clients, rate, and quota composition.
- Uses unicode meters and sparkline characters as escaped text. It supplies no
  HTML, CSS, JavaScript, colors, or DOM instructions.
- The OpenCode plugin is neither called nor modified. Core composes its
  descriptor into the Focus view.

## Capabilities

`log`, `emit_metric`, `read:records`, `read:config`, `declare_tool`,
`declare_ui_panel`.

No `http:fetch` capability: this plugin only observes local events.

## Files

| File | Purpose |
| --- | --- |
| `focus.go` | Guest logic, meters, compact lists, layout descriptor. |
| `focus_test.go.example` | Behavior tests; copy as `focus_test.go` into the SDK before running Go tests. |
| `cmd/focus-wasm/main.go` | WASI adapter exporting the v1 lifecycle. |
| `cmd/focus-wasm/main_nonwasi.go` | Native stub so `go test` works. |
| `go.mod` | Self-contained nested SDK module for local builds. |
| `manifest.json` | Signed 0.2.0 manifest. |
| `module.wasm` | Built WASI module. |
| `dashboard-focus-0.2.0.mvplugin` | Signed installable package. |

## Install and observe

Local upload:

```sh
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -F "file=@dashboard-focus-0.2.0.mvplugin" \
  http://127.0.0.1:8080/admin/plugins/install
```

Or paste a local package into Settings → Plugins. Restart, then check:

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/admin/plugins
```

Expected result:

- two loaded modules with `failures: 0` when OpenCode is installed too;
- `dashboard-focus` descriptor with eight panels;
- Focus tab auto-activates on first descriptor arrival;
- Focus view shows layout fields, quota bars composed from OpenCode, loaded
  plugins, and the layout note.

Settings → Plugins lists every descriptor row. The plugin does not replace the
core Plugins card; it moved there from the dashboard in the core UI change.

## UI boundary

This demo proves what is possible with plugins only:

- escaped text values;
- unicode progress meters (`▰`, `▱`) and sparkline steps (`▁`…`█`);
- bounded panel titles and data maps;
- a core-owned Focus renderer that composes another plugin's data.

It cannot:

- change CSS variables, colors, spacing, or widget order by itself;
- inject markup, URLs, or event handlers;
- replace the core dashboard design without core changes;
- hot-reload a new WASM module without restart.

For a genuine redesign, add a whitelisted layout ABI or change core UI code.
See [plugin-ecosystem.md](../../docs/plugin-ecosystem.md).

## Build from source

This folder is a self-contained nested Go module. Copy `focus_test.go.example`
to `focus_test.go` to run the behavior tests locally, then:

```sh
go test ./...
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
  -o module.wasm ./cmd/focus-wasm
```

Recompute `module_sha256`, re-sign the manifest with a trusted signer, and
repackage exactly `manifest.json` and `module.wasm`.

## Verified behavior

- Coexists with unchanged `opencode-usage-limits 0.7.x`.
- Focus view composes real OpenCode quota data without changing that plugin.
- Eight visual descriptor rows render in Settings → Plugins and Focus detail.
- SDK behavior tests pass.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Focus tab does not appear | Confirm descriptor panel has `data.view = "focus"`. |
| Included quota missing | Check `include_plugin` / `include_panel` and OpenCode module status. |
| Rows look like plain text | Expected: core escapes all guest strings; unicode meters are text. |
| Module fails at boot | Rebuild with `-buildmode=c-shared`; verify signature and pin. |

See the root [plugin ecosystem guide](../../docs/plugin-ecosystem.md) and
[authoring guide](../../docs/plugin-authoring.md).
