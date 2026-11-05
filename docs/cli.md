# Operator CLI (mvctl)

`mvctl` is the human command-line client for millivolt's operator plane. It
calls the same documented `/admin/*` and `/metrics/*` routes as the dashboard
and reads them through the shared MCP operator client, so the origin check,
credential handling, redirect refusal, response bound and credential redaction
have one owner. It adds no server route, no server setting and no second
listener.

`mvctl` is a sibling of `cmd/proxy`, `cmd/stress` and `cmd/release`. It is
source-only: the container image still ships the proxy, and deployment
lifecycle stays with Compose.

## Build and run

```sh
go run ./cmd/mvctl status --url http://127.0.0.1:8081
go build -o mvctl ./cmd/mvctl
./mvctl --url http://127.0.0.1:8081 status
```

The origin is required: pass `--url` or set `MILLIVOLT_URL`. There is no
production default, so a command cannot silently address `127.0.0.1:8080`. A
trailing slash is normalized away. Only `http` and `https` origins are
accepted; userinfo, a path, a query and a fragment are rejected.

## Authentication

`mvctl` reads the operator credential from `MILLIVOLT_OPERATOR_TOKEN`, the one
variable the proxy's boot gate reads. It never accepts the token on argv,
because a command-line value is visible in `ps` and shell history. The shared
client validates the token's length band before sending a request.

```sh
export MILLIVOLT_OPERATOR_TOKEN='the-value-the-proxy-started-with'
mvctl status
```

Every request presents `Authorization: Bearer`. No cookie jar is kept: a
session cookie minted by one call is deliberately not reused, so a rotated
token takes effect on the next command. A missing or wrong credential is
reported with the operator gate's own status. A non-loopback origin should use
`https`; the credential is otherwise sent in cleartext.

## Output and exit codes

Human output is the default. `--json` (or `-j`) prints one JSON document, and
command output is stable enough for a pipeline. Diagnostics go to stderr as
`mvctl: ...`. After argument parsing, `--json` turns a command failure into
`{"error": "..."}` on stdout so a caller can parse one stream. A usage error
found before `--json` is parsed still goes to stderr and exits 2 because JSON
mode is not established yet; after parsing, a usage error also follows
`--json`.

| Exit | Meaning |
| --- | --- |
| 0 | success, including a preview that made no change |
| 1 | transport, HTTP or proxy-reported failure |
| 2 | usage error, unknown flag or invalid input |
| 130 | interrupted during a request, stream or watch |

`status` and `overview` print an allowlisted state projection with `--json`:
request rows and the full effective config are omitted, so use `requests` and
`config get` for those. Request rows in that projection would carry previews,
response headers and retry metadata that the dedicated commands keep out.

ANSI color is off when stdout is not a terminal. `--color`, `--no-color` and
`NO_COLOR` override the default. Request labels, model ids, error types and
proxy error text are sanitized before a terminal sees them, so a captured value
cannot inject an escape sequence.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--url URL` | proxy origin; required unless `MILLIVOLT_URL` is set |
| `--timeout DURATION` | bound one request, default the shipped `mcp_timeout`; 0 disables the deadline |
| `--json`, `-j` | emit one JSON document |
| `--yes` | confirm a state-changing command |
| `--color` / `--no-color` | override terminal color detection |
| `--help`, `-h` | command or global help |
| `--version` | print the mvctl version and source revision |

`--` ends flag parsing; when no command has been named yet, the next word may still name one. Boolean flags accept `--flag`, `--flag=true`,
`--flag=1`, `--flag=yes`, `--flag=on`, and the false spellings `false`, `0`,
`no`, `off`.

## Commands

### View

| Command | Purpose |
| --- | --- |
| `mvctl status` | health, origin, dashboard build and headline metrics |
| `mvctl overview` | KPI, storage, pause, throttle and debug state |
| `mvctl requests [N]` | one page of the request log, default and bounds from the shipped `dash_log_rows` |
| `mvctl query "SELECT ..."` | bounded SQL through `/metrics/query` |
| `mvctl prometheus` | the Prometheus text exposition |
| `mvctl watch` | sample live counters and print a sparkline |
| `mvctl health` | check `/healthz` |
| `mvctl url` | print the normalized origin |

`requests` accepts `--before-ms MS` and `--before-id ID` together to fetch the
next page; the id is an opaque keyset position and may be empty, so it is sent
exactly as given. The command prints the next cursor when the page has more
rows. Its
JSON rows intentionally omit captured content previews, response headers and
retry attempts. Use the dashboard capture drawer when full bodies are needed.

`query` accepts `--query-timeout DURATION`; the default is the shipped MCP
query bound, and an explicit value must be greater than zero. The statement
must be a single `SELECT` without a semicolon, using the same pre-check as the
MCP query tool. The SQL text is sent as one `q` parameter and is never logged.

`watch` accepts `--samples N`, bounded by the command's settings surface, and
`--interval DURATION` (default the shipped `dash_poll_interval`). Interrupting
any request or stream exits 130.

### Config

| Command | Purpose |
| --- | --- |
| `mvctl config get [KEY]` | show one setting or every saved setting |
| `mvctl config keys [SUBSTRING]` | list schema keys |
| `mvctl config diff` | compare saved file values with built-in defaults |
| `mvctl config set KEY=VALUE ... [--yes]` | revision-checked save |
| `mvctl config reload [--yes]` | re-read the config file |

`config set` reads `GET /admin/config` for the current revision and the schema.
The schema's `kind` decides coercion: `bool`, `int`, `duration`, `bytes` and
`string` take their natural text form; `strings`, `providers`, `aliases`,
`model_rules`, `request_overrides` and `sub_conversations` take a JSON value.
A numeric-looking string stays a string. An empty value is never a boolean. An
unknown key, an empty boolean or an undecodable composite value is a usage
error before any write.

The server writes the file and then answers 500 when the follow-up reload
fails. `mvctl` treats that committed document as a warning, not a failure, and
prints the reload error without inviting a retry that would fail with a stale
revision.

Credential-class provider header values are redacted from `config get`, `config
diff` and the post-save document of `config set`, in human output and `--json`,
using the shared header-name vocabulary. `--reveal` prints them. A historical
`last_reload` error is replaced by the redaction marker because a rejected value
can be quoted there. Token counters, token windows and rate-limit values are
measurements, not credentials, and are never redacted.

### Pause

| Command | Purpose |
| --- | --- |
| `mvctl pause [show]` | list holds and queued requests |
| `mvctl pause on --client X --provider P --yes` | create a scoped hold |
| `mvctl pause on --new --yes` | hold clients first seen after the hold |
| `mvctl pause on --all --yes` | hold every client and provider |
| `mvctl pause on --id ID [flags] --yes` | edit one hold in place |
| `mvctl pause off --id ID --yes` | remove one hold |
| `mvctl pause off --all --yes` | remove every hold |

`pause on` requires a scope: `--client`, `--provider`, `--new` or `--all`,
unless `--id` names an existing hold to edit. `--duration` and
`--max-queued` are optional. `pause off` deliberately rejects scope flags: the
proxy identifies a hold by `id`, and a scope-looking command that cleared every
hold would be unsafe. Use exactly one of `--id` or `--all`. A resume carries
only the hold id because the proxy has no scope-selective resume.

### Throttle and quota

| Command | Purpose |
| --- | --- |
| `mvctl throttle [show]` | list provider limits |
| `mvctl throttle set --provider P [flags] [--yes]` | merge limit dimensions |
| `mvctl throttle clear --provider P [--yes]` | drop a provider limit |
| `mvctl quota [show]` | show storm and quota state |
| `mvctl quota resume --provider P [--yes]` | close a retry-mode quota gate |

`throttle set` flags are `--concurrency`, `--requests`, `--request-window`,
`--tokens`, `--token-window`, `--anti-burst` and `--ramp-duration`. Windows and
ramps take a duration such as `1m`. At least one dimension is required.

### Queue and capture

| Command | Purpose |
| --- | --- |
| `mvctl drain --all [--yes]` | release every parked request |
| `mvctl drain --provider P [--yes]` | release one provider's parked requests |
| `mvctl drain --model M [--yes]` | release one model's parked requests |
| `mvctl drain --client C [--yes]` | release one client's parked requests |
| `mvctl debug [show]` | list capture sessions and their bounds |
| `mvctl debug on --client C --yes` | start a scoped capture |
| `mvctl debug off --id ID --yes` | stop one session |
| `mvctl debug off --all --yes` | stop every session |

Exactly one drain scope is required. `debug on` requires a scope and has no
unrestricted all-traffic form, matching the MCP tool: a bare enable would
capture every request body. Captures are sensitive even after header redaction,
and a capture create needs durable storage; the proxy answers 503 without it.
`debug off` identifies a session by `id` or `--all`, never by a scope flag,
and the two are mutually exclusive. `debug on` refuses a scope name the proxy
has not seen, because such a session reports enabled but captures nothing.

### History, backup and restart

| Command | Purpose |
| --- | --- |
| `mvctl purge [filters] [--all] [--yes]` | count first, then delete |
| `mvctl backup --out FILE` | download an archive |
| `mvctl restore --file FILE [--yes]` | inspect by default, apply with `--yes` |
| `mvctl restart --yes` | request a graceful restart |

`purge` filters are `--client`, `--provider`, `--model`, `--conversation-id`,
`--error-type`, `--status`, `--before-ms`, `--after-ms`, `--has-error` and
`--debug`. Every value must parse, and a zero or unparsable numeric filter is a
usage error rather than an omitted constraint. `--all` cannot be combined with
a filter. The command always calls `/admin/purge/count` first and prints the
count; only `--yes` calls `/admin/purge`, and an unfiltered purge also needs
`--all`. Before deleting, the command re-counts and refuses when the total
changed since the preview. That compares counts, not row identities, and rows
committed after the re-count are still deleted: the check catches a population
change, not every concurrent insert. A missing, fractional, negative or non-finite count is a failure,
never zero. A genuinely bodyless purge is the proxy's delete-everything form.

`backup --out FILE` is required; there is no default path. `--config` and
`--database` default to true (the command's settings surface) and are always
sent explicitly, so "both off" cannot silently become the server's both-on
default. The download streams to a private temporary file, checks the `MVB1`
magic, syncs the file, renames, and syncs the directory entry only on success.
An existing file is refused unless `--overwrite` is passed. A short HTTP body
is a failed transfer, not a published archive. Backup and restore are bounded
by cancellation, not by `--timeout`.

`restore` defaults to `inspect=1`, which validates the archive without applying
it; `--inspect` forces that mode even with `--yes`. `--yes` applies it. The member selection (`--config`, `--database`) and the
mode (`--config-mode`, `--database-mode`; `merge` or `replace`) are always sent
explicitly, and both the inspect and apply output name the selected members and
modes. The default mode is `replace`, the proxy's documented default, so the
preview names the broader action instead of hiding it. An inspect document's
archived provider headers are redacted; `--reveal` prints them. The apply
response's `restart_required` keys are called out in the output.

## Scope and non-goals

`mvctl` does not install, start, stop or update containers. It does not read or
write `proxy.yaml` directly, does not add a listener, and does not keep a second
copy of the proxy's routes, config schema, credential rules or backup grammar.
Those facts stay with `internal/mcp`, `internal/config` and
`internal/backup`. The command uses only the Go standard library and those
repository packages. Because the shared operator client lives in
`internal/mcp`, the resulting binary links the MCP Go SDK and its OAuth2 and
jsonschema dependencies. The backup grammar and archive magic live in the
dependency-free `internal/backup/params` leaf, so `mvctl` does not link SQLite
or zstd and never opens a database.
