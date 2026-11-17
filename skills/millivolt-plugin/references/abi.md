# ABI v1 quick reference

Source of truth: `millivolt-plugin-sdk/abi/v1/messages.go` and
`abi/v1/contract.md`. This file is a routing aid, not a substitute.

## Exports

- `mv_init() -> i32`
- `mv_handle() -> i32`
- `mv_shutdown() -> i32`

Nonzero returns are guest failures; the host records them and does not retry.

## Imports and capabilities

| Import | Capability | Payload |
| --- | --- | --- |
| `mv_log` | `log` | `LogEntry` |
| `mv_emit_metric` | `emit_metric` | `Metric` |
| `mv_read_record` | `read:records` | current `Record` |
| `mv_read_config_view` | `read:config` | current `ConfigView` |
| `mv_declare_tool` | `declare_tool` | `ToolDescriptor` |
| `mv_declare_ui_panel` | `declare_ui_panel` | `UIPanel` |
| `mv_http_fetch_request` | `http:fetch` | `HTTPFetchRequest` |
| `mv_http_fetch_response` | `http:fetch` | `HTTPFetchResponse` |

Every call is capability-checked at runtime. Denial returns false; guests must
tolerate it.

## Events

Exactly one payload field is set:

- `request.recorded`: redacted `Record` with provider, model, client, status,
  duration, tokens, cost, rate-limit remaining, and error type.
- `config.changed`: non-secret `ConfigView`.
- `operator.command`: bounded `OperatorCommand`.

## Response caps

- 256 KiB message size
- 64 logs, each <= 4 KiB
- 64 metrics, names <= 128 bytes
- 16 tools
- 16 panels
- UI panel data is a bounded `map[string]string`

Over-cap responses are rejected, not truncated.

## Credentials

`credential_id: "request"` means "attach the upstream credential from the
inference request that produced this event". The host applies the request's
auth header and prefix. The guest never sees the value.

`${env:NAME}` URL placeholders are expanded by the host from its own
environment. Do not put secret values in URLs, logs, metrics, descriptors, or
panel data.

## Versioning

Manifest must declare `abi: "v1"` and `min_millivolt`. Adding an optional field
can be backward-compatible; renaming a field or export requires v2.
