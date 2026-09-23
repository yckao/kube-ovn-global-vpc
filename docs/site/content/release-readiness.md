# Adoption and release readiness

The v0.1.0 release establishes a reviewable managed implementation, documentation and contribution surface. Evaluate it in an isolated environment with explicit acceptance criteria before considering production use.

## Before an evaluation

1. Review the [validation boundaries](validation.md), especially lifecycle loss, hardware and scale qualification requirements.
2. Prepare a routed underlay, approved IPAM reservations and at least two eligible gateway nodes per location.
3. Match the native Kube-OVN extension to the actual controller source and prepare rollback.
4. Configure separate project and location identities; keep internal APIs administrator-only.
5. Follow the [quick start](../../managed-quickstart.md), then verify bidirectional tenant packets, isolation and MTU.

## Before a production decision

| Workstream | Acceptance needed |
|---|---|
| Security | Independent review of privileged gateway runtime, isolation, scoped credentials and recovery paths |
| Identity | Renewable issuer integration and a tested expiry/rotation workflow |
| Availability | Real physical failure-domain tests, sufficient survivor capacity and a management quorum/DR design |
| Performance | Hardware-specific packet/crypto benchmarks, saturation behavior and broad-site topology tests |
| Lifecycle | Completed transport-specific add/delete/rejoin tests under agreed packet-loss targets |
| Recovery | Protected allocation/key backups and fenced restore, node replacement and expansion procedures |
| Platform integration | Native-subnet compute adapter, user authorization model and operational ownership |
| Distribution | Reproducible release artifacts, dependency/license review, image provenance and documented upgrade/rollback policy |
| Operations | Alerts, SLOs, incident diagnostics, support ownership and retention policy |

## Compatibility expectations

The public API is still `v1alpha2` despite the project release number being v0.1.0. These version axes serve different purposes. Do not infer backward-compatible storage upgrades or rolling migrations from the version label. The native integration is source pinned and must be reviewed alongside the deployed Kube-OVN version.

## Contribution and disclosure

Use the repository's contribution, security and release documents for the current project process. Public issue reports must omit kubeconfigs, access tokens, private keys, internal host inventories and raw packet captures containing tenant data. Keep security-sensitive findings out of a public issue until a suitable private reporting channel is established.

The [release guide](../../releasing.md) describes the packaging and publication gates. Repository publication, a version tag, container publication and a GitHub Pages deployment are separate actions; preparing this website does not imply that any of them has already happened.
