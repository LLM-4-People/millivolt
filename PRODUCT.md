# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

The operator of a self-hosted millivolt instance - one person, occasionally a
small team sharing one deployment. They open the dashboard to see whether
traffic is healthy right now (throughput, latency, rate limits, errors, spend),
to audit and debug individual requests after the fact, and secondarily to
configure proxy behavior (pauses, limits, routing, debug sessions).

## Product Purpose

A self-hosted, OpenAI/Anthropic-compatible LLM proxy with the dashboard
embedded in the same binary. It routes, translates and measures every request
that crosses it, so the operator sees every AI request and keeps their own
routing. Success means the dashboard answers "is my proxy healthy, what is
slow or failing, what did a specific request do, and what is it costing"
without leaving the instance.

## Positioning

One Go binary is the whole product: proxy, SQLite history, dashboard, MCP
surface. It stays a relay, not a platform - no separate provider registry, no
API-key vault, no user accounts, no external analytics. Per-attempt
observability is the differentiator: retries, absorbed failures, and in-band
statuses a committed 200 hid are first-class facts.

## Operating Context

Operators watch a live dashboard (health, chart, explorer, request log) with
drill-down into a request drawer, filter by provider, model, client and key,
and manage pauses, limits, storm protection and debug sessions. A local or
LAN-reachable instance (for example behind an internal hostname) is the normal
deployment; the dashboard is used both at a desktop and on a phone.

## Capabilities and Constraints

- Go owns data semantics and the server-authoritative aggregates; browser JS
  mirrors are pinned to them by tests.
- The UI ships as static assets from the binary: vanilla JavaScript, no
  framework, no CDN, no external fonts; it must work on an isolated network.
- Existing HTTP/JSON contracts (dashboard, MCP, prometheus) are consumed by
  other tools and stay stable.
- The owner's redesign constraint, quoted: "sentence case, no em dashes, must
  fit with the rest".

## Brand Commitments

Name: millivolt. Copy voice: sentence case, lowercase labels, no em dashes,
identifiers, protocol names and acronyms preserved. The dashboard keeps its
dense, dark, accessible identity, and any redesigned component must read as
part of the existing system rather than a bolted-on style.

## Evidence on Hand

- README.md and docs/dashboard.md, docs/operations.md, docs/architecture.md
  (dashboard design language, invariants, operations).
- The live instance and its rendered history; the UI harness
  (tests/ui_harness/**) and Go web tests pin current behavior and geometry.

## Product Principles

1. Measure every crossing; never mask what the provider said - the proxy
   discloses, it does not rewrite provider truth.
2. The dashboard is the operator plane: dense, scannable, accessible, and
   usable at any viewport, desktop and phone alike.
3. One binary, no external dependencies: nothing in the UI may require a
   network it cannot assume.
4. One owner per fact: numbers, vocabularies and semantics come from the Go
   owner; the UI mirrors them and tests pin the mirror.
5. Fast in the hand: live data stays live, controls respond immediately, and
   selection, scroll and focus survive updates.

## Accessibility & Inclusion

The repo requires the dense, dark, accessible dashboard style: readable
contrast in the dark palette, visible focus, keyboard-operable menus, touch
targets that work on phones, reduced-motion respect, and no contrast or motion
regression in redesigned components.
