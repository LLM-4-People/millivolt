# millivolt

An OpenAI-compatible inference proxy with a built-in observability dashboard.
Clients choose the upstream URL and credentials on each request; no endpoint or
API-key registry is required. Optional provider mappings enrich usage, cost,
headers, and model discovery.

![Dashboard showing request charts, usage and a live request log](docs/images/dashboard.png)

## What you get

- Live streaming rows, retries and in-flight state, with traffic, token,
  speed/latency and cost charts from short windows through All time.
- History exploration by provider, model, client, conversation, tool, time,
  status, error and key. Explicit parent links connect main/sub-conversations.
- Provider-reported usage and cost, with cents below $1 and separate error/429
  counts. Zero health badges stay hidden; missing cost is not a zero bill.
- Scoped Pause, provider-wide Limits, opt-in Debug, filtered export/deletion
  and revision-checked Settings. Source builds also support rebuild/restart.

![Explorer showing main and sub-conversation cards with parent links](docs/images/explorer.png)

Screenshots use synthetic local fixtures, not private requests or provider data.

## Before you run

millivolt has **no operator authentication or user isolation**. Inference and
administration share one listener. Both quickstarts restrict the host port to
loopback; use a trusted machine or protected ingress. An upstream API key does
not secure the dashboard. Read [Security](SECURITY.md) before network exposure.

Durable metrics are **best-effort**: sustained overload can drop records even
when inference succeeds. The dashboard warns about process-local storage drops.
See [storage and accounting](docs/operations.md#storage-and-accounting) for
capacity, memory and backup limitations.

## Quickstart

Linux is supported. Normal Docker deployment needs only the Compose file,
not Go, Node, a source checkout or an image build. Use an unused host port;
for development alongside an existing instance, use
[isolated development](CONTRIBUTING.md#isolated-development).

### Docker Compose

With Docker Engine and the Compose plugin installed, run these commands from
a new, empty deployment directory:

```sh
curl -fsSL https://raw.githubusercontent.com/LLM-4-People/millivolt/main/compose.yaml -o compose.yaml
docker compose pull
docker compose up -d
docker compose logs --tail 50
```

[compose.yaml](compose.yaml) publishes only on host loopback. Set
`MILLIVOLT_PORT` to an unused host port if needed. Its named volumes retain
history and private Settings configuration when the container is replaced; an
initially empty config volume uses built-in defaults. The runtime is non-root
with a read-only root filesystem. Dashboard rebuild is unavailable in an image.

See [container operation](docs/operations.md#containers) for image selection,
configuration, updates, backups and shutdown, or the
[reverse-proxy guide](docs/reverse-proxy.md) for NGINX and protected ingress.
Stop with `docker compose stop`. Local image builds use a separate
[development Compose overlay](CONTRIBUTING.md#container-development).

### From source

Install the Go toolchain required by [go.mod](go.mod):

```sh
git clone https://github.com/LLM-4-People/millivolt.git
cd millivolt
# Create a local config only if one does not already exist.
test -e proxy.yaml || cp proxy.example.yaml proxy.yaml
go run ./cmd/proxy -config proxy.yaml -listen 127.0.0.1:8080
```

### Connect a client

Open [http://127.0.0.1:8080/](http://127.0.0.1:8080/) for the dashboard, then set
your OpenAI-compatible client's connection options:

| Client setting | Value |
| --- | --- |
| API base URL | `http://127.0.0.1:8080/v1` |
| API key | Your actual upstream key, forwarded through `Authorization`; it does not authenticate dashboard access. |
| Custom header `X-Proxy-Base-URL` | Your upstream API base URL, including its path, such as `/v1`. |
| Optional header `X-Proxy-Client` | A label for this client in the dashboard. |

The client must support custom headers, or middleware that adds them. Select a
model supported by your upstream; millivolt does not register providers or keys.
If the client is another container, its `localhost` is not the proxy: on a shared
Compose network, use the proxy service URL `http://millivolt:8080/v1` instead.
For protected remote access, follow the
[reverse-proxy authentication guidance](docs/reverse-proxy.md#credentials-and-access-control).

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
