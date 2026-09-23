# Validation and maturity

The managed `platform.globalvpc.io/v1alpha2` implementation is experimental. This page describes test categories and acceptance boundaries,
not any maintainer's private infrastructure.

## Automated checks

| Layer | Entry point | Scope |
|---|---|---|
| Go/Python unit tests | `make test` | Planning, validation, allocation, reconciliation, ownership, snapshots and gateway behavior with isolated fixtures/doubles. |
| Race/static checks | `make test-race`, `make check` | Go race detector and vet within executed code paths. |
| Kubernetes API integration | `make test-integration` with `KUBEBUILDER_ASSETS` | Real API validation, concurrency, ownership, lifecycle and recovery contracts. |
| Native adapter | `integration/kube-ovn/` procedures | Source-pinned planner, native controller and OVSDB behavior on the specified runner. |
| Documentation | Website build and link checks | A self-contained public site without private evidence dependencies. |

Read each setup before interpreting a result. Fake clients, simulated runtime
ACKs and injected BFD state are not physical failover or end-to-end packet tests.
API-scale checks do not establish supported real-site scale.

## Dataplane acceptance for a deployment

Use synthetic workloads in a separately authorized environment to verify:

1. Native capability and exact generation/hash acknowledgements.
2. Bidirectional ICMP, HTTP, held TCP and sequenced UDP, source identity and MTU/PMTU.
3. Overlapping-CIDR isolation, including receiver-side absence of forbidden traffic.
4. Multiple gateways carrying distinct flows; report offered load separately
   from maximum throughput and single-flow limits.
5. Controller/API outage, gateway crash, local OVN and WAN path loss, with explicit
   fault scope, restoration checks and traffic-error accounting.
6. All-local-BFD-down discard without unrelated default/NAT fallback.
7. Join, add-Subnet, in-use guards, withdrawal, leave and fresh-UID rejoin.
   Separate functional completion from strict zero-error acceptance.

Deployment Ready, VPC Ready and successful packets answer different questions.
Configuration acceptance alone does not prove end-to-end reachability.

## Boundaries

- No production qualification, zero-loss guarantee or universal convergence SLA.
- No published maximum supported site count or gateway hardware throughput.
- Controller replicas do not prove API/etcd disaster recovery.
- Retained forwarding depends on relevant local OVN, host, gateway and underlay
  components remaining available.
- VM simulations do not establish independent physical NIC/host/site failures.
- Bounded offered load does not establish NIC line-rate capacity.
- Existing transport selection is immutable. Membership replacement, expansion,
  allocation/key recovery require explicit fenced procedures.
- Native extension installation, renewable identity and compute attachment
  remain explicit operator/platform responsibilities.

See the [roadmap](../ROADMAP.md), [security policy](../SECURITY.md) and
[quick start](managed-quickstart.md). Public fixtures and examples are invented;
deployment inventories and private run artifacts are not part of this release.
