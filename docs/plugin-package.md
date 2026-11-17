# Plugin package contract
> Status: **implemented and end-to-end verified** with local signed releases,
> a public GitHub release install, and a GitHub `@latest` update. Publishing
> your own signed release remains a release-operator step. The ABI contract
> and SDK live in the separate `millivolt-plugin-sdk` repository.


See [Plugin system](plugin-system.md) for the core runtime contract.

millivolt plugins are distributed as exactly one archive file with a
`.mvplugin` suffix. The archive is the installable unit, the stored active
artifact and the update artifact. millivolt reads the manifest and module from
the archive; it does not extract a plugin tree into the plugin directory.

## Archive format

A `.mvplugin` file is a ZIP archive with these exact entries:

```text
manifest.json
module.wasm
LICENSE        optional, informational only
README.md      optional, informational only
```

Rules:

- `manifest.json` and `module.wasm` are required.
- No `ui.json` exists in this contract.
- No other entries are accepted.
- Entry names are exact and case-sensitive.
- Directories, symlinks, absolute paths, `..`, backslashes and duplicate names
  are rejected.
- `LICENSE` and `README.md` are never loaded by the runtime.
- The added ABI surface is unchanged: this contract uses ABI `v1`, whose
  manifest and signature payload are fixed by the existing plugin system.

The manifest keeps its current fields: `id`, `version`, `abi`, `min_millivolt`,
`capabilities`, `module`, `module_sha256`, `license`, and optional `signer` and
`signature`. A manifest with an unknown field, trailing JSON, an unknown
capability or an ABI other than `v1` is rejected.

## Bounds

These are internal guardrails, not operator tunables:

| Item | Limit |
| --- | --- |
| Archive file | 32 MiB |
| Uncompressed `module.wasm` | 16 MiB |
| Archive entries | 8 |
| Manifest | 16 KiB |
| Active plugins | 16, shared with legacy modules |
| Inbox entries scanned | 16 |
| Previous archives per plugin | 1 |

The archive reader stops as soon as a bound is exceeded. No streaming decoder
may continue after a failed limit check.

## Names and digests

The active installed filename is:

```text
<plugin-id>.mvplugin
```

The one retained rollback artifact is:

```text
<plugin-id>.mvplugin.prev
```

A distributed archive may carry a version in its filename:

```text
opencode-usage-limits-0.1.0.mvplugin
```

After install, the active copy is stored under the stable plugin id name.

Integrity uses two digests:

1. `manifest.json:module_sha256` covers the bytes of `module.wasm`.
2. The lock file pins the SHA-256 of the whole active `.mvplugin` archive.

The Ed25519 manifest signature covers the existing fixed manifest payload. It
does not cover ZIP metadata or optional informational files; the archive pin
covers those bytes at rest and at boot.

## Trust policy

- Local unsigned development installs remain allowed only while
  `plugin_trust_file` is empty.
- When `plugin_trust_file` is configured, every archive must carry a valid
  signer and signature.
- Remote installs always require `plugin_trust_file` and a valid signature.
- A pin only changes through a verified install, update, rollback or explicit
  pin operation.
- An invalid signature, module digest or archive pin rejects the archive before
  instantiation.

## Install sources

Installation accepts one source at a time:

1. one `.mvplugin` file uploaded through the operator-gated management API
2. one `.mvplugin` file placed in the optional inbox directory, then scanned on
   demand
3. a GitHub release reference pasted into the dashboard

Accepted GitHub forms:

```text
github:owner/repo@vX.Y.Z
github:owner/repo@latest
https://github.com/owner/repo/releases/download/<tag>/<asset>.mvplugin
```

Rejected forms:

- branch names, including `main` and `master`
- raw or blob URLs
- non-GitHub hosts
- HTTP URLs
- userinfo, query strings or fragments
- redirects to hosts outside the verified GitHub allowlist

The implementation verifies the current GitHub API and asset CDN hosts before
hardcoding the network allowlist. Downloads are bounded by time and size, use
plain HTTP client calls, and never shell out to `git`, `curl` or a package
manager.

## Restart model

Install, update and rollback validate and stage files first. They do not swap a
running WASM instance. Every successful mutation returns:

```json
{
  "restart_required": true
}
```

The next boot loads the active archive under the normal trust and pin checks.
Plugin changes remain boot-bound.

## Update contract

An update is a verified replacement of one active archive.

A candidate must:

- have the same plugin `id` as the installed plugin
- have a higher semantic version, unless a downgrade is explicitly allowed
- declare a supported ABI, currently `v1`
- declare `min_millivolt` no newer than the running millivolt version
- carry a valid signature under the effective trust policy
- match its own `module_sha256`
- pass every archive, manifest and capability check used by install

Version comparison normalizes an optional leading `v` in plugin versions.
millivolt's own release version has no leading `v`. Prereleases are not selected
by `@latest` unless explicitly requested.

Adding a capability is a privilege change. The update response reports added
and removed capabilities, and the update is refused without explicit operator
confirmation when capabilities are added. Removing capabilities is reported but
does not block.

The commit sequence is:

```text
resolve candidate
 -> validate archive
 -> compare version, ABI, min_millivolt and capabilities
 -> stage temp archive
 -> move current active archive to .prev
 -> move staged archive to active .mvplugin
 -> update pin and install metadata
 -> report restart_required
```

A failed commit leaves the active archive, pin and install metadata unchanged.
At most one previous archive is retained.

## Rollback and boot recovery

Rollback restores the recorded `.prev` archive after verifying its digest,
signature and manifest. It then restores the matching pin and install metadata,
and reports `restart_required`.

On boot, if the active archive fails validation and a recorded previous archive
exists, millivolt may promote the previous archive only when it passes the same
signature, digest and state checks. An invalid previous archive is never
promoted. If no valid recovery artifact exists, boot fails closed.

Rollback and boot recovery never bypass trust.

## Management surface

The existing `GET /admin/plugins` remains the status surface. Existing
`action=verify` and `action=pin` remain unchanged.

The distribution phases add these operator-gated routes:

```text
POST /admin/plugins/install
POST /admin/plugins/uninstall
GET  /admin/plugins/updates/check?id=<plugin>
POST /admin/plugins/update
POST /admin/plugins/rollback
```
Install accepts a multipart file, a JSON `source`, or JSON
`{"action":"install_inbox"}`. The inbox action scans at most 16 entries,
refuses symlinks and non-regular files, and leaves invalid archives in place
with a per-file failure report. The other routes use bounded JSON. All new
routes inherit the existing `/admin/` operator gate. No route accepts
plugin-supplied HTML, JavaScript or CSS.

## Non-goals

This contract does not add:

- plugin-controlled UI components or actions
- hot swapping of WASM modules
- automatic background updates
- arbitrary package registries
- sidecar plugins
- ABI `v2`
- extraction of archive contents into loose files

Those require separate decisions and their own threat models.
