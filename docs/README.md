# Documentation

millivolt is one self-hosted service for routing OpenAI-compatible traffic,
seeing what happened, and controlling the operator surface. Start with the path
that matches what you need to do.

## Start here

- [Run it locally](../README.md#quickstart) - Docker Compose first, source second.
- [Connect a client](../README.md#connect-a-client) - base URL, routing headers, and a working curl.
- [Tour the dashboard](dashboard.md) - the views, charts, request drawer, and Settings.
- [Read the protocol](protocol.md) - endpoints, upstream routing, retries, model discovery, and conversations.

## Guides

- [Upstream compatibility and adapters](adapters.md) - OpenAI-compatible, Anthropic Messages, Cursor Connect, and token refresh.
- [Plugin ecosystem](plugin-ecosystem.md) - what plugins can do, how packages, trust, updates, and the dashboard surfaces work.
- [Author a plugin](plugin-authoring.md) - build, test, sign, package, install, and publish an ABI v1 plugin.
- [Reverse proxy](reverse-proxy.md) - protected ingress, credentials, and NGINX.
- [MCP server](mcp.md) - the 22-tool operator API for LLM-driven analysis.
- [Container operation](operations.md#containers) - image selection, volumes, backups, and shutdown.

## Reference

- [Plugin system](plugin-system.md) - rule packs, scoped credentials, the embedded WASM host and the management API.
- [Operations](operations.md) - configuration, persistence, routes, storage, performance, and limits.
- [Performance and resource usage](performance.md) - current measured tables, method, commands, and limitations.
- [Architecture](architecture.md) - ownership, invariants, and where each behavior lives.
- [Versions and images](operations.md#versions-and-images) - version identity, tags, and verification.

## Project

- [What you get](../README.md#what-you-get) - the short feature overview.
- [Contributing](../CONTRIBUTING.md) - development, checks, screenshots, and release policy.
- [Security](../SECURITY.md) - exposure boundary, reporting, and supported versions.
- [Releases](https://github.com/LLM-4-People/millivolt/releases) - published versions and notes.
- [License](../LICENSE) - MIT terms and dependency notices.

- Agent guide (`AGENTS.md`) - repository invariants and plugin task routing.
- [Plugin author skill](../skills/millivolt-plugin/SKILL.md) - reusable agent workflow for plugin work.
- [Demo plugins](../demo-plugins/README.md) - two working signed plugins with source and READMEs.

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
