# Operator flow

## Required config

```yaml
plugins_enabled: true
plugin_dir: "/plugins"
plugin_trust_file: "/config/trust.json"
plugin_lock_file: "/plugins/plugins.lock.json"
plugin_downloads_enabled: true     # only needed for GitHub/source installs
plugin_http_allow_hosts:           # only needed for http:fetch plugins
  - "opencode.ai"
```

The lock file must exist before boot. An empty valid lock is:

```json
{"pins":{},"installs":{}}
```

## Status

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/admin/plugins
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8080/metrics/bootstrap
```

Check module version, `failures`, descriptors, and non-secret config. A module
can be installed without producing descriptors until its first event.

## Local install

```sh
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -F "file=@my-plugin-0.1.0.mvplugin" \
  http://127.0.0.1:8080/admin/plugins/install
```

Then restart and re-check status.

## GitHub install

```sh
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"source":"github:owner/repo@v0.1.0"}' \
  http://127.0.0.1:8080/admin/plugins/install
```

Failure signatures:

| Message | Meaning |
| --- | --- |
| `plugin downloads are disabled` | `plugin_downloads_enabled` is false. |
| `plugin_trust_file is required` | Remote install has no trust file. |
| `plugin source redirect is not allowed` | Disallowed redirect host/query scheme, or old core before CDN query support. |
| `signature verification failed` | Signer not trusted or manifest changed. |

## Update

```sh
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  'http://127.0.0.1:8080/admin/plugins/updates/check?id=my-plugin'

curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"my-plugin","source":"github:owner/repo@latest"}' \
  http://127.0.0.1:8080/admin/plugins/update
```

A successful report includes `signature: verified`, `pin: updated`, and
`restart_required: true`. Adding capabilities is refused unless explicitly
confirmed.

## Rollback and uninstall

```sh
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"my-plugin"}' \
  http://127.0.0.1:8080/admin/plugins/rollback
```

Uninstall uses the same body on `/admin/plugins/uninstall`. Both require a
restart to change the loaded module set.

## Dashboard path

Open Settings → Plugins. The tab publishes install controls, module rows,
per-plugin actions, and descriptor rows. It is the dashboard equivalent of the
`/admin/plugins` surface; every control is core-owned.
