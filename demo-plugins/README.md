# Demo plugins

Two working ABI v1 plugins are included here. They are real, signed, installable
packages with source and tests, not placeholders.

| Plugin | Purpose | README |
| --- | --- | --- |
| `opencode-usage-limits` | All seven v1 capabilities; real OpenCode quota; GitHub install and `@latest` update. | [README](opencode-usage-limits/README.md) |
| `dashboard-focus` | Multi-plugin compatibility; bounded visual descriptors; Focus view composition. | [README](dashboard-focus/README.md) |

Each folder contains:

- guest source and behavior tests;
- the WASI adapter source;
- signed manifest;
- built `module.wasm`;
- signed `.mvplugin` package;
- an author/operator README.

Use the root [plugin ecosystem guide](../docs/plugin-ecosystem.md) for the
architecture and the [authoring guide](../docs/plugin-authoring.md) for a
step-by-step build guide. Do not commit private signing keys; trust files carry
public keys only.
