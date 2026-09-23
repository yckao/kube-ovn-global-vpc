# Roadmap

This is a direction, not a delivery schedule or a statement of existing features.
Version 0.1.0 is an experimental managed API and native integration implementation.

## Reduce integration work

- Collaborate with upstream Kube-OVN on the destination-route contract and a
  compatibility matrix for matching native binaries and schemas.
- Provide a reviewed installer with image digests and explicit extension
  installation, upgrade and rollback steps.
- Integrate renewable location identities and compute/KubeVirt attachment
  adapters resolving `nativeSubnetName`.

## Improve lifecycle and operations

- Define supported fenced gateway-node replacement and membership expansion.
- Complete independently reproducible join/leave/rejoin and outage/recovery
  acceptance across transports.
- Reduce update disturbance and document measured convergence budgets.
- Improve actionable status, metrics and diagnostics.

## Establish production and capacity boundaries

- Publish a synthetic reproducible multi-site environment independent of any
  maintainer's infrastructure.
- Measure CPU, encryption, MTU, flow distribution and hardware aggregation;
  measure single-flow capacity separately.
- Measure larger real dataplanes, mesh growth, object sizes and convergence.
- Verify independent physical node/NIC/site failures and availability prerequisites.
- Complete security review, dependency/image scans, SBOMs and signed provenance
  before distributing production-oriented binary/container releases.

Tenant UI, firewall products, cross-site compute placement, workload control-plane
HA and storage mobility are separate platform concerns.
