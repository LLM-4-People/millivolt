# millivolt plugin ABI v1

ABI v1 is deliberately narrow. A guest receives redacted event metadata and may
emit bounded observations and declarations. It cannot execute arbitrary requests,
read bodies, or reach the operator plane directly.

## Wire format

All messages are UTF-8 JSON objects with the field names in `messages.go`.
A host must refuse any message larger than `MaxMessageBytes` (256 KiB) before
decoding it, and must enforce every repeated-field cap.

The core-WASM import/export names are fixed. A guest:

- exports `mv_init() -> i32`: called once at load.
- exports `mv_handle() -> i32`: called for each event.
- exports `mv_shutdown() -> i32`: called once at unload.
- imports `mv_read_input(ptr: i32, cap: i32) -> i32`: copies the pending event
  JSON into guest memory; returns the byte count or a negative error.
- imports `mv_write_output(ptr: i32, len: i32) -> i32`: hands a response JSON
  document to the host; returns 0 or a negative error.
- imports host calls named in the capability table below, each taking a JSON
  argument document and returning a status i32.

A nonzero return from any export is a guest failure. The host records it and
does not retry the event. The guest never receives a pointer to host memory.

## Events

| Type | Payload | Meaning |
| --- | --- | --- |
| `request.recorded` | `record` | A request has been finalized and recorded. |
| `config.changed` | `config` | A non-secret config view changed. |
| `operator.command` | `command` | An operator invoked a plugin command. |

Exactly one payload field is set per event. Unknown event types are refused by
the host.

## Host calls and capabilities

| Import | Capability | Purpose |
| --- | --- | --- |
| `mv_log` | `log` | Emit a bounded log line. |
| `mv_emit_metric` | `emit_metric` | Emit one bounded numeric observation. |
| `mv_read_record` | `read:records` | Read the current record payload. |
| `mv_read_config_view` | `read:config` | Read the current non-secret config view. |
| `mv_declare_tool` | `declare_tool` | Declare an MCP tool descriptor. |
| `mv_declare_ui_panel` | `declare_ui_panel` | Declare a core-rendered panel. |
| `mv_http_fetch_request` | `http:fetch` | Request a bounded, allowlisted outbound GET. |
| `mv_http_fetch_response` | `http:fetch` | Read the bounded fetch result for the current request. |

A capability is checked by the host on every call, not inferred from an SDK
annotation. A denied call returns a negative status; the guest must tolerate it.

A fetch URL may contain `${env:NAME}` placeholders, and a request may name a
`credential_id` plus the header it should populate. The host expands both from
its own environment; values never enter guest memory, logs, or descriptors.
Credential values come from `MILLIVOLT_PLUGIN_HTTP_CREDENTIAL_<ID>`.

The reserved credential id `request` reuses the upstream credential of the
inference request that produced the current event. The host attaches it with
the request's own auth header and prefix; the guest still never sees the value.

## Non-goals for v1

- Request or response body access (including opt-in previews).
- Direct network, filesystem, or clock access from the guest.
- Guest-supplied HTML, JavaScript, or CSS.
- Request-path mutation. Guests observe; they do not rewrite traffic.

## Versioning

A manifest declares `abi: "v1"` and `min_millivolt`. The host refuses a module
whose ABI it does not implement. Adding a field is a minor, backward-compatible
change; changing a field name, type, or export name requires `v2`.
