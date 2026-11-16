# Install and first run

Install millivolt from the published image or a source checkout, set the
operator credential, and send a first request on each client protocol. The
[quickstart](../README.md#quickstart) is the short form of the same procedure;
this page carries the full path.

## Before you start

The container path needs Linux with Docker Engine and the Compose plugin. No Go,
Node or source checkout is needed for it. The source path needs the Go toolchain
required by [go.mod](../go.mod).

> [!IMPORTANT]
> One shared operator credential protects the dashboard and operator plane only.
> Inference is transparent: it forwards your upstream key and does not use the
> operator token. There are no user accounts. Keep the listener on loopback, or
> put the network or ingress boundary in front. With the token unset, the
> dashboard denies every request. Read [Security](../SECURITY.md) before network
> exposure.

## Install with Docker Compose

```sh
curl -fsSL https://raw.githubusercontent.com/LLM-4-People/millivolt/main/compose.yaml -o compose.yaml
export MILLIVOLT_OPERATOR_TOKEN='replace-with-a-long-random-value'
docker compose up -d
```

Open [http://127.0.0.1:8080/](http://127.0.0.1:8080/) and sign in with that
token. If the container exits, run `docker compose logs --tail 50`.

The Compose file publishes on loopback only. It keeps history and settings in
named volumes, runs non-root with a read-only root filesystem, and accepts
`MILLIVOLT_PORT` for an unused host port. For bind mounts, pre-create
directories writable by UID/GID `65532:65532`; see
[persistent state](containers.md#persistent-state-and-configuration). For image
selection, backups, updates, shutdown, or protected ingress, see
[container operation](containers.md#containers) and the
[reverse-proxy guide](reverse-proxy.md).

The published image is `ghcr.io/llm-4-people/millivolt:main` for amd64 and
arm64. Version tags identify releases; pin a digest when exact bytes matter. See
[versions and images](containers.md#versions-and-images).

## Install from a source checkout

Install the Go toolchain required by [go.mod](../go.mod), then:

```sh
git clone https://github.com/LLM-4-People/millivolt.git
cd millivolt
test -e proxy.yaml || cp proxy.example.yaml proxy.yaml
export MILLIVOLT_OPERATOR_TOKEN='replace-with-a-long-random-value'
go run ./cmd/proxy -config proxy.yaml -listen 127.0.0.1:8080
```

For isolated development and checks, see [Contributing](../CONTRIBUTING.md).

## Environment and defaults

The Compose inputs are environment variables; their defaults live in
[compose.yaml](../compose.yaml), not server configuration. Supply them
consistently through the environment or an ignored local `.env` file.

| Variable | Meaning |
| --- | --- |
| `MILLIVOLT_OPERATOR_TOKEN` | The single operator credential. It is read once at boot from the process environment and never from `proxy.yaml`, so rotating it requires a real process restart. A nonempty value shorter than 16 or longer than 512 bytes fails the boot. Unset or empty leaves the dashboard denied. |
| `MILLIVOLT_IMAGE` | Image reference; defaults to `ghcr.io/llm-4-people/millivolt:main`. |
| `MILLIVOLT_PORT` | Loopback host port; default `8080`. |
| `MILLIVOLT_STOP_GRACE_PERIOD` | Container stop grace; default `30s`. |
| `MILLIVOLT_MEM_LIMIT` | Container memory ceiling; default `1g`. |
| `MILLIVOLT_PIDS_LIMIT` | Process and thread ceiling; default `512`. |

`docker compose config` shows the resolved deployment. A source deployment
configures the same service through `-config PATH` (default local `proxy.yaml`),
`-listen ADDRESS` and `-db-path PATH`; the generated example and the Settings
schema are the field reference. See [configuration](configuration.md).

## Send a first request

Point a client at the proxy and supply the upstream base URL and key on each
request. The proxy registers no providers or keys. `X-Proxy-Format` selects a
different upstream protocol; absent, the request passes through. See
[protocol](protocol.md) and [adapters](adapters.md).

OpenAI Chat Completions:

```sh
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H "X-Proxy-Base-URL: $UPSTREAM_BASE_URL" \
  -H "Authorization: Bearer $UPSTREAM_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$UPSTREAM_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
```

OpenAI Responses:

```sh
curl -sS http://127.0.0.1:8080/v1/responses \
  -H "X-Proxy-Base-URL: $UPSTREAM_BASE_URL" \
  -H "Authorization: Bearer $UPSTREAM_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$UPSTREAM_MODEL\",\"input\":\"hello\"}"
```

Anthropic Messages, using the native upstream's own auth header, prefix and API
version:

```sh
curl -sS http://127.0.0.1:8080/v1/messages \
  -H "X-Proxy-Base-URL: $UPSTREAM_BASE_URL" \
  -H "X-Proxy-Key: $UPSTREAM_API_KEY" \
  -H 'X-Proxy-Auth-Header: x-api-key' \
  -H 'X-Proxy-Auth-Prefix;' \
  -H "anthropic-version: $ANTHROPIC_VERSION" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$UPSTREAM_MODEL\",\"max_tokens\":1024,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
```

The semicolon in `-H 'X-Proxy-Auth-Prefix;'` is important: curl sends a present
empty header. `-H 'X-Proxy-Auth-Prefix:'` instead omits it, leaving the proxy's
`Bearer ` default. A response means the proxy answered; check the status code
for the upstream outcome. The request appears in the dashboard.

On a shared Compose network, use `http://millivolt:8080/v1` instead of loopback.
`/v1/chat/completions`, `/v1/responses` and `/v1/messages` are all available.
Each can reach Chat, Responses or Anthropic upstreams; only a Chat client can
target the Cursor bridge. Read
[upstream compatibility](adapters.md#client-and-upstream-compatibility) and
[cross-protocol translation](adapters.md#cross-protocol-translation).

## The dashboard

The dashboard is the site root, [http://127.0.0.1:8080/](http://127.0.0.1:8080/).
It asks for the operator token once per browser tab, keeps it in sessionStorage,
attaches it to gated calls only and drops it as soon as the server rejects it.
The same credential gates `/metrics/*`, `/admin/*` and `/mcp`; transparent
inference does not use it. See [operator access](routes.md#operator-access) for
the canonical open-path list.

## What to read next

[Documentation](README.md) routes by task. For the next step, read
[configuration](configuration.md) before changing settings, or
[the dashboard tour](dashboard.md) to find the request you just sent.
