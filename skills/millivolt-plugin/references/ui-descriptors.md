# UI descriptor rules

Plugins do not own the DOM. A `UIPanel` is a bounded map of strings that core
may render as fixed fields, progress bars, or a composed Focus view.

## Safe pattern

```go
v1.UIPanel{
    ID:    "dashboard-focus",
    Title: "Focus layout",
    Data: map[string]string{
        "view":           "focus",
        "include_plugin": "opencode-usage-limits",
        "include_panel":  "opencode-usage-limits",
        "heading":        "Quota and traffic",
        "arrange":        "quota,traffic,plugins",
        "requests":       "12",
        "note":           "▰ REQ 12 · ▰ ERR 0",
    },
}
```

Unicode meters (`▰`, `▱`, `▁`…`█`) are plain text and render safely as escaped
values. They are the right way to make a plugin-driven view more visual within
v1.

## Core-rendered surfaces

- **Settings → Plugins** lists installed modules, capabilities, failures,
  tools, and panel titles.
- **OpenCode tab** renders the OpenCode demo's fixed fields and quota bars.
- **Focus view** activates when a panel has `data.view = "focus"`. Core reads
  `include_plugin` / `include_panel` and composes that descriptor data.

## Prohibited

Do not propose or implement:

- guest HTML, CSS, JavaScript, URLs, or event handlers;
- plugin-defined colors, spacing, chart types, or widget placement;
- direct DOM manipulation from WASM;
- content that depends on core trusting a guest-supplied string as markup.

Guest strings are escaped. If a value must be a structured hint, use a small
closed vocabulary (for example `view=focus`, `arrange=quota,traffic`) and let
core interpret it.

## Compatibility expectation

Multiple plugins can coexist. Descriptors are keyed by plugin id and panel id.
A layout plugin composes another plugin's data without calling or modifying it.
A missing included plugin should render an honest "waiting" or "not installed"
state, not fabricated data.
