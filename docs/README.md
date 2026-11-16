# Documentation

millivolt is one self-hosted service for routing model traffic across OpenAI
Chat Completions, OpenAI Responses and Anthropic Messages, seeing what happened,
and controlling the operator surface. Start with the path
that matches what you need to do.

## First ten minutes

1. [Install and first run](install.md) from Compose or a source checkout.
2. Send one request on a client protocol and find it in the
   [dashboard](dashboard.md).
3. Read [configuration](configuration.md) before changing the example or a
   setting.
4. Read [security](../SECURITY.md) before exposing the listener.
5. Keep [operations](operations.md) nearby for health, storage and the known
   limits.

## If you want to ...

| Task | Start with |
| --- | --- |
| Install and send a first request | [Install and first run](install.md) |
| Configure a setting, request override or provider alias | [Configuration](configuration.md) |
| Look up a setting's default or a CLI flag | [Settings reference](settings.md) |
| Expose the listener behind TLS or a network boundary | [Reverse proxy](reverse-proxy.md), [security](../SECURITY.md) |
| Debug a 429 or a 5xx | [Routes and debugging](routes.md#error-storm-protection), [pause and limits](routes.md#pause-and-limits) |
| Back up or restore | [Settings backup and restore](containers.md#settings-backup-and-restore), [image-only backup](containers.md#image-only-backup-and-restore), [online database backup](operations.md#storage-and-accounting) |
| Upgrade or restart | [Versions and images](containers.md#versions-and-images), [reload, restart and shutdown](containers.md#reload-restart-and-shutdown) |
| Integrate a client or translate between protocols | [Protocol](protocol.md), [adapters](adapters.md) |
| Drive the operator plane from an LLM | [MCP server](mcp.md) |
| Drive it from a terminal | [Operator CLI](cli.md) |
| Contribute a change | [Contributing](../CONTRIBUTING.md), [architecture](architecture.md), [support](../SUPPORT.md) |

The [operations overview](operations.md) owns health, storage and the runtime
cost model. Measurements live in [performance](performance.md); ownership and
invariants live in [architecture](architecture.md). Project pages are
[support](../SUPPORT.md), [releases](https://github.com/LLM-4-People/millivolt/releases),
[license](../LICENSE) and [third-party notices](../THIRD_PARTY_NOTICES.md).

## Verification and limitations

The generated [settings reference](settings.md) lists every setting and proxy
flag with its type, default, accepted values and reload behavior.
[proxy.example.yaml](../proxy.example.yaml) is the commented deployment example.
`internal/config` owns defaults, validation and Settings metadata; local
`proxy.yaml` is not a public reference or a test fixture.

Current commands live in [Contributing](../CONTRIBUTING.md#checks). Runtime
limits live in [operations](operations.md#known-limits); measurement caveats
live in [performance](performance.md#limitations). The
[architecture map](architecture.md) connects invariants to their shared
implementation and regression coverage. Past development notes are not an
operating guide or a capacity guarantee.
