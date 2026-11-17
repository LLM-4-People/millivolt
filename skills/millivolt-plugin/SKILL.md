---
name: millivolt-plugin
description: Author, review, install, update, or debug Millivolt ABI v1 plugins and plugin packages; use for .mvplugin, WASM analyzer guests, plugin manifests, GitHub release installs, plugin descriptors, and Settings → Plugins tasks.
---

# Millivolt plugin workflow

Use this skill for Millivolt plugin work. It covers rule packs, WASM analyzer
guests, signed `.mvplugin` packages, the operator API, and core-rendered
descriptor output. It does **not** authorize external writes or publishing by
itself; preserve the user's scope.

## Essential constraints

- ABI is v1. Guests observe after a request is recorded; they never mutate
  traffic or read request/response bodies.
- Capabilities are closed: `log`, `emit_metric`, `read:records`, `read:config`,
  `declare_tool`, `declare_ui_panel`, `http:fetch`.
- Guests never supply HTML, CSS, JavaScript, URLs, or event handlers. UI data
  is bounded, escaped text rendered by core.
- Remote installs require `plugin_downloads_enabled`, a `plugin_trust_file`, a
  signed manifest, and a GitHub semantic-version release asset.
- Install, update, and rollback are boot-bound: always restart and re-check
  `/admin/plugins`.
- WASI builds must use `-buildmode=c-shared`. A plain wasm build exports
  `_start` and fails host init.

## Route by task

- Authoring a guest: read `references/build-and-publish.md` and copy the
  closest demo under `demo-plugins/`. Run SDK tests and conformance.
- ABI/import questions: read `references/abi.md`; treat `abi/v1/messages.go`
  and `abi/v1/contract.md` in the SDK as the source of truth.
- UI/descriptor questions: read `references/ui-descriptors.md`. Do not propose
  guest HTML or custom CSS.
- Install/update/debug in a running proxy: read
  `references/operator-flow.md`; verify with `/admin/plugins` and
  `/metrics/bootstrap` after restart.

## Completion criteria

A plugin task is done only when:

1. capability set is minimal and correct;
2. response caps and secret boundaries hold;
3. manifest digest and package entries are valid;
4. tests include observable behavior, not wiring echoes;
5. local install or update was staged, restarted, and inspected;
6. distributed plugins have a signed release and a verified trust key;
7. docs and demo READMEs reflect any new behavior.

If a task needs UI restyling, request-path mutation, ABI v2, sidecars, or
arbitrary registries, state that it is outside v1 and stop for a design
decision.
