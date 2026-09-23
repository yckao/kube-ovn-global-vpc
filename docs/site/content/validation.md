# Validation and known limitations

v0.1.0 is an experimental engineering release. Validation must be matched to the claim: API acceptance, native acknowledgement, actual forwarding, physical availability and throughput are different properties.

## Test layers

| Layer | What it can establish | What it cannot establish |
|---|---|---|
| Go unit and race tests | Planner behavior, reconciliation, allocation, ownership and concurrency cases | Actual OVN execution or packets |
| Python tests | Gateway configuration/runtime helpers and validation tooling | Hardware throughput or an installed CNI |
| Real API integration | Kubernetes schema, UID/status/CAS, lifecycle and simulated acknowledgements | Native/gateway behavior when those components are doubles |
| Native extension tests | Route planning, OVSDB ownership guards and transaction behavior | A running controller's complete dataplane path |
| Isolated OVSDB/northd tests | Real wire compatibility and logical forwarding with controlled liveness | Physical BFD exchange or hardware failure |
| Endpoint packet tests | The measured source/destination/protocol/MTU path | Every failure mode or peak capacity |
| Physical fault and load tests | The exact hardware, topology and offered load exercised | Unmeasured deployments or a general SLA |

## Reproducible repository checks

```sh
make build
make test
make test-race
make check
```

API-server integration requires `KUBEBUILDER_ASSETS` and `make test-integration`. Native tests require the exact source target and toolchain described in the [extension reference](../../../integration/kube-ovn/README.md). CI results belong to a commit and build, not to an arbitrary installed environment.

For a deployment, start with the [quick start](../../managed-quickstart.md), then define explicit acceptance cases for bidirectional traffic, overlapping-tenant isolation, source preservation, MTU, cold join, add/delete/rejoin, recovery and failure containment.

## Qualification boundaries

| Area | v0.1.0 boundary |
|---|---|
| Transport | WireGuard/BGP, Geneve/BGP and VXLAN/EVPN implementations are available; validate each chosen profile on its actual path |
| Lifecycle | Do not assume packet-loss-free updates or deletion/rejoin under every outage |
| Native dependency | Source-matched extension binary and schema are required for cross-location ECMP/BFD |
| Hardware HA | Separate failure-domain placement and real host/NIC/site tests are required |
| Capacity | No guaranteed 25/50/400 Gb/s throughput, member-count or production site-count envelope |
| Scale | API object storage and schema admission are not gateway convergence or packet-scale benchmarks |
| Recovery | Node replacement, changed member count and lost allocator/key state require explicit fenced recovery |
| Identity | Renewable issuer integration and automatic credential renewal are not implemented |
| Product integration | Compute attachment adapter, tenant UI and production alert/SLO package are not included |
| API evolution | Alpha API; no guaranteed backward-compatible upgrade or migration contract |

## Keep measurements interpretable

Report the commit, build, topology, member count, traffic workload, fault boundary and acceptance criterion with every result. Record packet errors and response gaps as well as eventual recovery. A surviving held TCP connection does not imply that fresh connections or UDP had no loss.

Use enough load to measure the intended property. A capped functional workload is not a peak-throughput benchmark. Distinguish one flow from aggregate flows, port line rate from tenant goodput, and hardware capacity from control-plane object limits.

## Separate networking from other platform guarantees

Global VPC does not choose a workload Kubernetes control-plane quorum, prove cross-Infra compute placement, or provide storage mobility and VM live migration. Those require their own implementation and acceptance plan.

The public project contains generic examples and source-level validation guidance. Deployment credentials, private inventories and environment-specific test records do not belong in its public documentation or release artifacts.
