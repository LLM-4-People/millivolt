# Documentation

| If you want to… | Read |
| --- | --- |
| Start a local instance | [README](../README.md#quickstart) |
| Explore the dashboard visually | [Dashboard guide](dashboard.md) |
| Run Docker Compose or manage persistent volumes | [Container operation](operations.md#containers) |
| Add NGINX or another protected ingress | [Reverse proxy](reverse-proxy.md) |
| Choose an image or understand version identity | [Versions and images](operations.md#versions-and-images) |
| Connect a client or declare parent conversations | [Protocol](protocol.md) |
| Configure, operate, back up or restart | [Operations](operations.md) |
| Understand code ownership and invariants | [Architecture](architecture.md) |
| Use a native adapter or subscription-token refresh | [Adapters](adapters.md) |
| Change code and run isolated verification | [Contributing](../CONTRIBUTING.md) |
| Refresh the product screenshots | [Screenshot workflow](../CONTRIBUTING.md#documentation-screenshots) |
| Assess exposure or report a vulnerability | [Security](../SECURITY.md) |

The generated [configuration example](../proxy.example.yaml) documents settings.
`internal/config` owns defaults, validation and Settings metadata; local
`proxy.yaml` is not a public reference or a test fixture.

## Verification and limitations

Current commands live in [Contributing](../CONTRIBUTING.md#checks); runtime
limits and measurement caveats live in [operations](operations.md#known-limits).
The [architecture map](architecture.md) connects invariants to their shared
implementation and regression coverage. Past development notes are not an
operating guide or a capacity guarantee.
