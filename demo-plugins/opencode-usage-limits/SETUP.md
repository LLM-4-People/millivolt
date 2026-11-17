# OpenCode Go quota setup

This plugin reads the real OpenCode Go subscription limits from OpenCode's
key-authenticated usage API. It does not infer quota from proxy traffic, use the
desktop app, require a browser cookie, or ask for a separate secret.

Endpoint:

```text
GET https://opencode.ai/zen/go/v1/usage
Authorization: Bearer <OpenCode Go API key>
```

The API key is the same credential already present on the OpenCode Go inference
request that Millivolt tracks. The host attaches it; the guest never sees it.

## What Millivolt core needs

The core provides only the generic `http:fetch` capability with a reserved
`credential_id: request`, meaning "attach the credential of the request that
produced this plugin event". No OpenCode-specific URL or parser lives in core.

Allow the endpoint host:

```yaml
plugins_enabled: true
plugin_http_allow_hosts:
  - "opencode.ai"
```

Install the signed package:

```sh
/admin/plugins/install   # upload opencode-usage-limits-0.7.1.mvplugin
```

Then restart Millivolt so the allowlist and plugin are active.

## How it works in practice

1. Send an OpenCode Go inference request through Millivolt with its normal
   `Authorization: Bearer sk-...` header (or `X-Proxy-Key` / configured auth
   header).
2. Millivolt records the request and invokes the plugin.
3. The plugin asks for the usage endpoint with `credential_id: request`.
4. The host attaches the exact auth header and prefix from that tracked request,
   calls the allowlisted endpoint, and returns only the JSON response body.
5. The plugin parses `usage.rolling`, `usage.weekly`, and `usage.monthly`
   percentages and reset times, then publishes them as escaped panel fields.

The raw API key is never stored in a record, log, descriptor, response body, or
dashboard JavaScript. It lives only for the duration of the plugin call.

## Notes

- The allowlist is exact: only `opencode.ai` can receive the request credential.
- If the tracked request used a custom auth header or prefix via
  `X-Proxy-Auth-Header` / `X-Proxy-Auth-Prefix`, the host reuses that exact
  header and value.
- If no request credential is available, the dashboard shows
  `quota status: unavailable`.
- A fresh tracked request triggers a fresh quota read.
