# AGENTS.md - millivolt

OpenAI-compatible inference proxy and embedded dashboard. Read
[README.md](README.md) for the product contract and
[docs/architecture.md](docs/architecture.md) for canonical owners.

## Mandatory working rules

- Delegate independent discovery, research, and bounded implementation by
  default; give complete prompts and do not duplicate delegated work. Keep one
  repository writer at a time when tasks overlap. Readers may work in parallel.
- Research first: inspect actual code and current official/installed dependency
  documentation. Use available Context7, DeepWiki, Kagi, and Scrapling tools as
  appropriate; if unavailable, state that and use official sources directly.
  Verify uncertain claims against more than one source, not model memory.
- Re-read files before editing. Preserve unrelated user changes. After the final
  edit, rerun relevant checks: earlier verification no longer verifies the result.
- DRY / KISS / YAGNI: edit existing shared owners at the lowest common boundary.
  One canonical fact/behavior, not per-caller patches, duplicate pipelines,
  compatibility shims, or hypothetical abstractions. Remove dead code.
- Deny by default at trust boundaries. Reject malformed types, ranges, unknown
  fields and ambiguous destructive inputs; never silently broaden a filter.
- Every user-tunable server value belongs in `internal/config.Config`, with its
  default only in `Default()`, validation, schema metadata, and the generated
  [proxy.example.yaml](proxy.example.yaml). Document type/range/reload behavior.
  Never add a consumer fallback default. Internal safety/implementation constants
  remain named and explained in the owning file.
  Deployment examples use `config.Example()`, which reuses `Default()` and owns
  enabled compatibility profiles; regenerate them with `-print-example-config`.
  `-print-config` remains the neutral built-in default document.
- Prefer idiomatic maintained APIs; confirm the installed version's behavior.
- Provider-agnostic mechanisms and neutral fixtures are mandatory. Provider data
  belongs in config mappings, not label branches or static model-price tables.
  Protocol-specific adapters remain explicit and separate from generic routing.
- Reproduce bugs and add a failing-before/passing-after regression where feasible.
  Treat correctness, performance, memory, latency and UX as acceptance criteria.
- Use sentence case or lowercase for prose, headings and UI labels. Do not use
  em dashes; use appropriate sentence punctuation or a hyphen placeholder.
  Preserve identifiers, user data, protocol names, acronyms and license wording.
- Never claim lossless storage, universal transparency, instantaneous boot, or
  an absolute capacity ceiling without evidence supporting that precise scope.

## Production and development safety

Never restart, reconfigure, purge, stress, or otherwise mutate the user's main
instance on `:8080` for development. Do not edit their local `proxy.yaml` or
database as a test fixture.

[scripts/dev.sh](scripts/dev.sh) is the sole dev-process lifecycle owner:
`scripts/dev.sh`, `scripts/dev.sh status`, `scripts/dev.sh stop`.
It uses a private loopback port, scratch database/config and PID-file identity.
Do not replace it with `pkill`, `fuser`, broad kills or background `go run`.
Development starts reset disposable state; see
[CONTRIBUTING.md](CONTRIBUTING.md#isolated-development) before using overrides.

`scripts/check.sh container` owns only its separate, disposable Docker fixture:
an image ID, a unique container and fresh named volumes, with no network or
published ports. It never starts a host proxy or uses deployment volumes.

Use temporary databases and neutral local upstreams. Real-browser/stress tools
target only a verified private instance and clean up their unique fixture scope.
Use scoped ownership for helper processes; no paid-provider or production calls
unless the user explicitly authorizes them. Never copy a live SQLite database
as a plain file; use the existing online-backup helper.

Documentation history requires explicit authorization and the separate redacted
copy workflow in [Contributing](CONTRIBUTING.md#documentation-screenshots).
Never serve raw copied history for publication or weaken the synthetic fixture
guard to accept it. Captures are static, not evidence of current live status.

## Core invariants

- `readRequest` owns bounded request bytes and the single metadata decode.
  Timing starts after upload, before metadata work. Preserve invalid-JSON
  passthrough and opt-in-only content capture.
- Proxy-stream work must stay bounded and cheap. Durable enqueue is nonblocking;
  do not add a history scan or SQL wait to request accounting.
- `Buffer.PublishLive` owns pending views; `Buffer.Record` atomically finalizes,
  retires pending state and publishes completion. Sequence, feed epoch and
  pending revisions must remain coherent. Client row/gauge/snapshot ordering
  gates are separate necessary protections, not redundant checks.
- `Store.Clear` owns the accepted-record deletion fence and transactional
  durable/live consistency. New completions survive; old queued records must
  not resurrect. Administrative JSON has one strict decoder.
- SQLite is authoritative. One shared analytical projection and two bounded
  last-result slots serve aggregates. Preserve destructive invalidation and
  fail-closed reads; never retain per-view history copies.
- Go owns model canonicalization and scope time buckets. Observer dictionaries
  and revisions carry that meaning to the browser; no JS regex authority or
  client-inferred ancestry. Conversation filters select exact leaves.
- Chart buckets/period totals are server-authoritative at configured cadence;
  no optimistic event arithmetic. Live rows and the gauge remain event-driven.
- Health counts are distinct affected requests: `Record.IsError` and
  `Record.HasRateLimit` have different meanings. 429 alone is not an error.
  Keep raw/projected/ring/storage predicates and duplicate memberships aligned.
- Preserve checked finite numeric arithmetic and shared exact R7 percentile
  math. Store money in USD; `fmtMoney` alone owns display units.
- Keep the dense, dark, accessible dashboard style. Shared registries,
  formatters, delegated handlers, dialog/menu lifecycle and render gates own
  repeated UI behavior. Preserve saved views, scroll/selection and focus.
  Never put JS comments inside an HTML template-literal body.
- Secrets must not enter retained headers. Debug bodies remain sensitive even
  after header redaction. Keep operator protection off transparent inference.

## Where to read and verify

[Architecture](docs/architecture.md) maps each invariant to its source and tests.
[Protocol](docs/protocol.md), [operations](docs/operations.md), and
[adapters](docs/adapters.md) own the detailed behavior; update their existing
sections rather than appending conflicting overrides here.

Use [CONTRIBUTING.md](CONTRIBUTING.md) and `scripts/check.sh` for setup and checks.
Verify proportionally: targeted regressions after edits, then the core suite;
race checks for concurrency, browser checks for layout/canvas/focus, and isolated
benchmarks for performance changes. Report what actually ran and any limitations.
Preserve vendored license headers and [third-party notices](THIRD_PARTY_NOTICES.md).
