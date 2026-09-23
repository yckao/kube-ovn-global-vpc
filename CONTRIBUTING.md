# Contributing

Contributions to the managed `platform.globalvpc.io/v1alpha2` path are welcome.
Discuss changes to ownership, networking contracts, credentials, cleanup or
public API behavior in an issue before undertaking a large implementation.
Use the [security process](SECURITY.md) for vulnerabilities.

## Development and pull requests

1. Branch from `main`. Use the Go toolchain in `go.mod` and Python 3.11 or newer.
2. Run `make build`, `make test`, `make check` and relevant `make test-race` checks.
3. Run API tests with `KUBEBUILDER_ASSETS` and `make test-integration` when needed.
   Native source-pinned adapter checks have separate prerequisites under
   `integration/kube-ovn/`. Never replace a controller binary based on version
   similarity alone.
4. Explain the problem, resulting behavior, compatibility and recovery impact.
   Update examples and documentation when interfaces change. See
   [website instructions](docs/site/README.md) for documentation builds.
5. Preserve single-writer ownership, UID fences, allocation anchors, conditional
   cleanup and fail-closed behavior.

Keep tests focused on observable behavior. Unit, API, native/OVSDB and live
packet tests support different claims. State which checks ran, which did not,
and why. Configuration acceptance is not packet/failover evidence.

Do not commit credentials, private topology, hostnames, local absolute paths,
deployment inventories, packet captures or environment-specific output. Use
invented examples and sanitized fixtures.

Source, comments and maintained technical documentation use English. The
Traditional Chinese engineering briefing is a localized artifact. Keep generated
diagrams synchronized with their source.

## Contribution terms

Submit only material you have the right to contribute. Contributions are under
the project Apache-2.0 license unless clearly identified third-party material
has a compatible license and retained attribution. Sign off commits with
`git commit -s` to certify the [Developer Certificate of Origin](https://developercertificate.org/).
No separate CLA is currently required. Retain upstream notices and identify
modifications when changing upstream-derived integration patches.
