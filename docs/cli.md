# Operator CLI (`mvctl`)

`mvctl` is a small command-line client over millivolt's existing operator
plane. It reads the same routes as the dashboard and drives the same actions as
the `/mcp` tools, but for a human at a terminal. It adds no server endpoints,
manages no containers, and does not change the proxy's configuration schema.

The command is a sibling of `cmd/proxy`, `cmd/stress` and `cmd/release`. It is
optional: the dashboard, the HTTP admin API and the MCP server remain unchanged.

## Build and run

From a source checkout:

```sh
go run ./cmd/mvctl status --url http://127.0.0.1:8080
```

Or build a binary:

```sh
go build -o mvctl ./cmd/mvctl
./mvctl status
```

The default base URL is `http://127.0.0.1:8080`. Override it with `--url` or
`MILLIVOLT_URL`.

## Authentication

`mvctl` uses the same operator credential as the dashboard:
`MILLIVOLT_OPERATOR_TOKEN`, passed as `Authorization: Bearer`. The token is read
from the environment or `--token`, is never printed, and is redacted from
`--json` output. A missing or wrong credential is reported by the operator gate
as usual. No new credential path is introduced.

All `/admin/*` and `/metrics/*` routes used here are already documented in
[operator and data routes](operations.md#operator-and-data-routes). Bare
namespace roots such as `/admin` are reserved and answer 404.

## Commands

View:

| Command | Purpose |
| --- | --- |
| `mvctl status` | Health, base URL, dashboard build and headline metrics |
| `mvctl overview` | Full KPI, storage and operator state |
| `mvctl requests [N]` | A page of the durable request log |
| `mvctl query "SELECT ..."` | Bounded SQL through `/metrics/query` |
| `mvctl prometheus` | The Prometheus text exposition |
| `mvctl watch` | Sample live counters and print a sparkline |
| `mvctl url` | Print the configured base URL |
| `mvctl health` | Exit nonzero when `/healthz` does not answer |

Control:

| Command | Purpose |
| --- | --- |
| `mvctl config get\|set\|reload\|keys\|diff` | Read and revision-check live config |
| `mvctl pause [on\|off]` | Inspect or change pause holds |
| `mvctl throttle set\|clear` | Inspect or change provider limits |
| `mvctl quota resume --provider X` | Close a retry-mode quota gate |
| `mvctl drain --all\|--provider X` | Release parked requests |
| `mvctl debug [on\|off]` | Inspect or change capture sessions |
| `mvctl purge` | Preview with `/admin/purge/count`, then delete |
| `mvctl backup --out FILE` | Download the settings and database archive |
| `mvctl restore --file FILE` | Inspect an archive, apply with `--yes` |
| `mvctl restart --yes` | Request a graceful restart |

Run `mvctl help` for the full list. Every command accepts `--json` for
scripting, and `--no-color` or `NO_COLOR` disables ANSI styling.

## Safety

Every state-changing command is explicit. `config set` and `config reload`,
`pause on|off`, `throttle set|clear`, `quota resume`, `drain`, `debug on|off`
and `restart` print a preview and perform no request until `--yes` is supplied.
The same guard covers one-shot inference in the standalone distribution.

`purge` calls `/admin/purge/count` first and prints the preview; it deletes
history only with `--yes`. An unfiltered purge additionally requires `--all`.
`restore` sends `inspect=1` by default and applies the archive only with
`--yes`; the archive is still uploaded for validation, and the output says so.
`backup` refuses to overwrite an existing file unless `--overwrite=true` and
streams large archives to a temporary file, so a truncated download is an
error rather than a corrupt "successful" backup.

`config set` echoes the revision returned by `GET /admin/config`; a stale
revision is rejected by the server with 409. `--json` output redacts any field
whose name looks like a credential, including values inside typed structs and
typed maps. Error bodies are scrubbed of the operator token before printing.
`--yes=false` (and `=0`, `=no`, `=off`) disables a flag, and `--` ends flag
parsing, so a confirmation cannot be triggered accidentally.

## Scope

`mvctl` does not install, start, stop or update containers, does not read or
write `proxy.yaml` directly, and does not add a second listener. Deployment
lifecycle remains the operator's responsibility through Compose. The command
depends only on the Go standard library.
