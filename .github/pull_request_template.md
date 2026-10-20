## Summary

Describe the change and the existing shared owner it updates.

## Linked issue

Closes #

## Target branch

- [ ] `testing` (default)
- [ ] `main` promotion, with the `testing` history already merged forward

## Type of change

- [ ] Bug fix
- [ ] Feature
- [ ] Documentation or site
- [ ] Refactor, tests, or tooling
- [ ] Breaking change, with the migration described below

## Verification

List the checks actually run and any check that did not run. For performance
changes, include the workload and durable-accounting result.

- [ ] `scripts/check.sh` core checks
- [ ] Browser fixtures against the private dev instance
- [ ] Live MCP integration when the operator or MCP surface changed
- [ ] Container fixture when Docker, Compose, or startup changed
- [ ] Static site build and link check when docs, README, or `site-src` changed
- [ ] A regression covers a reproduced defect

## Documentation and safety

- [ ] User-visible behavior, configuration, and operations docs are updated.
- [ ] The change does not include credentials, local configuration, private history, or unauthorized captures.
- [ ] The `CONTRIBUTING.md` change checklist was reviewed.
