# Documentation

| If you want to… | Read |
| --- | --- |
| Start a local instance | [README](../README.md#quickstart) |
| Run Docker Compose or manage persistent volumes | [Container operation](operations.md#containers) |
| Choose an image or understand version identity | [Versions and images](operations.md#versions-and-images) |
| Connect a client or declare parent conversations | [Protocol](protocol.md) |
| Configure, operate, back up or restart | [Operations](operations.md) |
| Understand code ownership and invariants | [Architecture](architecture.md) |
| Use a native adapter or subscription-token refresh | [Adapters](adapters.md) |
| Change code and run isolated verification | [Contributing](../CONTRIBUTING.md) |
| Assess exposure or report a vulnerability | [Security](../SECURITY.md) |

The generated [configuration example](../proxy.example.yaml) documents settings.
`internal/config` owns defaults, validation and Settings metadata; local
`proxy.yaml` is not a public reference or a test fixture.

## Historical evidence

- [Codebase audit and isolated load tests - 2026-09-05](audit-2026-09-05.md)
- [Explorer and storage follow-up - 2026-09-05](explorer-storage-2026-09-05.md)
- [GitHub-readiness passes - 2026-09-05](github-readiness-2026-09-05.md)

These reports describe specific review stages and local synthetic workloads,
not a guarantee about every later checkout or deployment. Their `/tmp` paths
identify author-local evidence, not artifacts supplied with a clone. Preserve
the workload, failed stages and limitations when citing numbers. The later
storage follow-up records an unresolved sustained-storage overload failure.
