# Changelog

This project follows [Semantic Versioning](https://semver.org/). During the
experimental `0.x` series, APIs and installation contracts can change between
minor releases. Read migration notes before upgrading; a version number is not
a production support commitment.

## 0.1.1 — Prebuilt Helm distribution

Published September 23, 2026:
[v0.1.1](https://github.com/yckao/kube-ovn-global-vpc/releases/tag/v0.1.1), with five
OCI images, three Helm charts, four CLI builds, source and checksums.

### Added

- Project-scoped `VPC` and per-location `Subnet` resources, with generated
  internal `NetworkBinding` snapshots.
- A management authority and independent local operators with leader election,
  explicit ownership and persisted local configuration.
- Automatic gateway membership, address/key discovery and per-VPC multi-gateway
  runtime generation.
- WireGuard/BGP, Geneve/BGP and VXLAN/EVPN transport classes over a routed
  underlay, without tenant routing configuration on ToRs.
- A source-locked Kube-OVN destination-route extension with generation/hash
  acknowledgements, ECMP/BFD and fail-closed remote-prefix routing.
- Allocation retention, guarded deletion and controller-incarnation fencing.
- Installation examples, architecture diagrams, operations documentation and a
  static documentation website suitable for GitHub Pages.
- Progressive installation: bring up DC-A and verify a local VPC before adding
  DC-B. Authority upgrade guards permit strictly additive location registration.
- Helm charts as the primary distribution for authority, site and native extension,
  with guarded cleanup, configuration checks and release rollback hooks.
- Standalone `vpcctl` binaries wrapping Helm lifecycle and public VPC/Subnet
  operations, including scoped access issuance and project drainage.
- CI publication of prebuilt controller, gateway, native and hook images,
  four CLI platforms, chart packages, checksums, source and image evidence.
- Credential-free unit/API test automation and repeatable release packaging.

### Known limitations

- Cross-location operation requires the matching native Kube-OVN extension;
  stock Kube-OVN alone does not implement this exact route contract.
- This release is experimental. It provides no production scale, bandwidth,
  zero-packet-loss or physical failure-domain guarantee.
- Existing configuration can remain available during bounded control-plane
  outages; new global changes require management authority availability.
- Geneve and VXLAN/EVPN require a trusted underlay and do not encrypt traffic.
- Gateway membership resizing, endpoint replacement and lost allocator/key
  recovery require an explicit administrative procedure.
- Compute-platform attachment automation, a tenant UI, renewable platform
  identity integration and seamless transport migration are not included.
- Dataplane and hook images currently target Linux/amd64; runtime packet/BFD
  qualification remains separate from CI build and API checks.

## 0.1.0 — Source preview

The original GitHub prerelease published source and reference materials. It did
not publish the OCI images or Helm packages required by the new installation
path. Use the prebuilt distribution introduced with 0.1.1 for that path.
