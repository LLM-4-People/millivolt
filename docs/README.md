# Documentation

millivolt is one self-hosted service for routing OpenAI-compatible traffic,
seeing what happened, and controlling the operator surface. Start with the path
that matches what you need to do.

## First ten minutes

1. [Run the Compose quickstart](../README.md#quickstart).
2. [Connect a client](../README.md#connect-a-client) and send one request.
3. Open the [dashboard](dashboard.md) and find that request.
4. Read [security](../SECURITY.md) before exposing the listener.

## Choose a path

| Goal | Start with |
| --- | --- |
| Evaluate the proxy | [Protocol](protocol.md), [dashboard tour](dashboard.md) |
| Operate a deployment | [Operations](operations.md), [reverse proxy](reverse-proxy.md), [performance](performance.md) |
| Integrate a client or agent | [Adapters](adapters.md), [MCP server](mcp.md) |
| Contribute a change | [Contributing](../CONTRIBUTING.md), [support](../SUPPORT.md) |

## Guides

- [Upstream compatibility and adapters](adapters.md) - OpenAI-compatible, Anthropic Messages, Cursor Connect, and token refresh.
- [Reverse proxy](reverse-proxy.md) - protected ingress, credentials, and NGINX.
- [MCP server](mcp.md) - the 22-tool operator API for LLM-driven analysis.
- [Container operation](operations.md#containers) - image selection, volumes, backups, and shutdown.

## Reference

- [Operations](operations.md) - configuration, persistence, routes, storage, performance, and limits.
- [Performance and resource usage](performance.md) - current measured tables, method, commands, and limitations.
- [Architecture](architecture.md) - ownership, invariants, and where each behavior lives.
- [Versions and images](operations.md#versions-and-images) - version identity, promotion, tags, and verification.

## Project

- [What you get](../README.md#what-you-get) - the short feature overview.
- [Contributing](../CONTRIBUTING.md) - development, checks, branch promotion, screenshots, and release policy.
- [Security](../SECURITY.md) - exposure boundary, reporting, and supported versions.
- [Releases](https://github.com/LLM-4-People/millivolt/releases) - published versions and notes.
- [License](../LICENSE) - MIT terms and dependency notices.

## Verification and limitations

The generated [configuration example](../proxy.example.yaml) documents settings.
`internal/config` owns defaults, validation and Settings metadata; local
`proxy.yaml` is not a public reference or a test fixture.

Current commands live in [Contributing](../CONTRIBUTING.md#checks). Runtime
limits live in [operations](operations.md#known-limits); measurement caveats
live in [performance](performance.md#limitations). The
[architecture map](architecture.md) connects invariants to their shared
implementation and regression coverage. Past development notes are not an
operating guide or a capacity guarantee.
