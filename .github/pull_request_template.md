## Summary

What changed, and which existing shared owner it updates.

## Related issue

Refs #

Use `Closes #` only in a promotion pull request to `main`. Closing keywords are
ignored when the base branch is `testing`.

## Target branch

Open feature pull requests against `testing`. GitHub's default base is `main`,
so change it before creating the pull request. `main` is promotion-only and
accepts merge commits only.

## Verification

List the commands actually run and any check that did not run. CI is the
authority for the full gate.

- `scripts/check.sh`:
- Browser, live MCP, container, or site checks, when affected:

## Breaking change

Describe the migration, or write "None".

## Documentation and safety

- [ ] User-visible behavior, configuration, and operations docs are updated.
- [ ] No credentials, local configuration, private history, or unauthorized captures are included.
