# millivolt

An OpenAI-compatible inference proxy with a built-in observability dashboard.
Clients choose the upstream URL and credentials on each request; no endpoint or
API-key registry is required. Optional provider mappings enrich usage, cost,
headers, and model discovery.

## Before you run

millivolt has **no operator authentication or user isolation**. Inference,
dashboard, SQL, Debug, Settings, deletion, and restart share one listener. Use a
trusted machine/network or an appropriately protected ingress. An upstream API
key does not authenticate an operator. The supplied listen setting binds all
interfaces; both quickstarts below restrict the host port to loopback.
See [Security](SECURITY.md) before exposing it.

Durable metrics are **best-effort**: sustained overload can drop records even
when inference succeeds. The dashboard warns about process-local storage drops.
The [storage follow-up](docs/explorer-storage-2026-09-05.md#remaining-capacity-failure)
records an unresolved end-to-end overload failure, not a lossless guarantee.

## Quickstart

Linux is the supported platform. Clone the
[repository](https://github.com/LLM-4-People/millivolt), then choose a container
or source deployment from the checkout root.

### Docker Compose

With Docker Engine and the Compose plugin installed, pull the published image
and start it without installing Go on the host:

```sh
docker compose pull
docker compose up -d --no-build
docker compose logs --tail 50
```

[compose.yaml](compose.yaml) publishes only on host loopback. Set
`MILLIVOLT_PORT` to an unused host port if needed. Its named volumes retain
history and private Settings configuration when the container is replaced; an
initially empty config volume uses built-in defaults. The runtime is non-root
with a read-only root filesystem. Dashboard rebuild is unavailable in an image.

To build the current checkout instead, including before its image is published:

```sh
MILLIVOLT_IMAGE=millivolt:local docker compose up -d --build --pull never
```

See [container operation](docs/operations.md#containers) for image selection,
configuration, updates, backups and shutdown. Stop with `docker compose stop`.

### From source

Install the Go toolchain required by [go.mod](go.mod):

```sh
# Create a local config only if one does not already exist.
test -e proxy.yaml || cp proxy.example.yaml proxy.yaml
go run ./cmd/proxy -config proxy.yaml -listen 127.0.0.1:8080
```

Choose an unused port for a new deployment. For development alongside an existing
instance, use [the isolated development workflow](CONTRIBUTING.md#isolated-development),
not another process on its port.

Open [http://127.0.0.1:8080/](http://127.0.0.1:8080/) for the dashboard. Point your
OpenAI-compatible client at this address and supply `X-Proxy-Base-URL`; the
upstream credential comes from `X-Proxy-Key` or `Authorization`.

For example, with your own upstream URL, key, and supported model:

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "X-Proxy-Base-URL: $UPSTREAM_BASE_URL" \
  -H "Authorization: Bearer $UPSTREAM_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$UPSTREAM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
```

The URL variables above are supplied by you, not millivolt settings. For a model
name containing JSON-special characters, use your client's JSON encoder.
The [protocol guide](docs/protocol.md) covers path joining, all routing headers,
native-format adapters, and model listing.

## What you get

- Live request rows, retry attempts, in-flight state, and durable-history
  exploration. The KPI band is global; chart and explorer use the selected scope.
- Traffic, tokens, speed/latency, errors, and cost charts with longer ranges and
  All time. One percentile control owns the speed/latency selection.
- Provider-reported usage and cost, never a per-model price table. Totals include
  only detected provider costs; missing cost is not evidence of a zero bill.
  USD amounts below $1 display in cents throughout the dashboard.
- Separate affected-request error and HTTP 429 counts. Zero badges are hidden;
  a request with both a genuine failure and 429 can appear in both counts.
- Explicit main/sub-conversation relationships and exact parent links. Clients
  must declare parents; historical ancestry is not inferred.
- Scoped Pause, provider-wide Limits, opt-in Debug captures, filtered export and
  deletion, revision-checked Settings, and source-build rebuild/restart progress.

Normal relay paths preserve request content and upstream response content, with
documented exceptions: bounded request/quality buffering, retries, SSE keepalive
comments and failure signaling, optional format translation, and constructed
model lists. This is not an unconditional byte-for-byte or exactly-once contract.

## Configuration and documentation

[proxy.example.yaml](proxy.example.yaml) is the generated, documented example.
`proxy.yaml` is your ignored local configuration; do not commit it. Defaults and
validation belong to `internal/config`, and Settings uses that same schema.
See [operations](docs/operations.md) for overrides, reloads, backups, and restart
requirements.

[VERSION](VERSION) owns the application version. See
[image and release usage](docs/operations.md#versions-and-images) and
[contribution guidelines](CONTRIBUTING.md) before publishing a change.

- [Documentation index](docs/README.md)
- [Protocol and client integration](docs/protocol.md)
- [Operations and limitations](docs/operations.md)
- [Architecture and ownership](docs/architecture.md)
- [Optional adapters and token refresh](docs/adapters.md)
- [Contributing and verification](CONTRIBUTING.md)
- [Security](SECURITY.md)

The application is [MIT licensed](LICENSE). Dependency notices are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
