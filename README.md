# Global VPC Controller

**Version 0.1.1 · Experimental · Apache-2.0**

Declarative multi-site tenant networking built around Kube-OVN. Users create a
project-scoped `VPC` and a `Subnet` in each location; the platform creates native
Kube-OVN resources, discovers gateway members and configures cross-site routing.

[Documentation](https://yckao.github.io/kube-ovn-global-vpc/) ·
[Quick start](docs/managed-quickstart.md) ·
[Example YAML](config/examples/managed/network.yaml) ·
[Engineering diagrams](docs/managed-solution-diagrams/README.md) ·
[Release notes](CHANGELOG.md)

## What it provides

- A small public API: global VPC intent and location-scoped Subnets.
- Native Kube-OVN Vpc/Subnet integration with explicit writer ownership.
- Multiple gateways per VPC and location, with flow ECMP and BFD.
- WireGuard/BGP, Geneve/BGP and software VXLAN/EVPN transports.
- Automatic gateway discovery through Kubernetes resources, without manual
  peer IP lists or direct site-operator RPCs.
- Durable local snapshots: an unavailable management service does not
  automatically withdraw already accepted forwarding state.
- Tenant isolation with overlapping CIDRs across different VPCs. Subnets in
  the same VPC must not overlap.
- A routed underlay without tenant-specific ToR BGP/EVPN configuration.

```text
Management Kubernetes API
  VPC + Subnet -> Authority -> per-location NetworkBinding
                                  | desired state / discovery status
                 +----------------+----------------+
                 v                                 v
          Site Operator A                   Site Operator B
          local snapshot                    local snapshot
          native Kube-OVN                   native Kube-OVN
          multiple gateways <== transport ==> multiple gateways
```

Management is not in the packet path. Each location retains its own Kubernetes
API and OVN databases. New global changes still depend on management; this is
not disconnected multi-writer global control.

## Start with the managed API

Install the project's Helm charts: `global-vpc` for the management authority,
`global-vpc-site` for each Infra, and `kube-ovn-global-vpc-extension` for the
matched native integration. [vpcctl](docs/vpcctl.md) wraps the same Helm
install/upgrade/history/rollback/uninstall lifecycle and provides VPC/Subnet
operations without a UI. Release CI builds the CLI binaries and immutable images;
users do not need Go, Docker or BuildKit.

Follow the [managed quick start](docs/managed-quickstart.md) for preparation,
installation, tenant operations, verification, upgrades, rollback and removal.
[v0.1.1 is published](https://github.com/yckao/kube-ovn-global-vpc/releases/tag/v0.1.1)
with five OCI images, three Helm charts, four CLI builds and checksums. Use its
packaged charts; source chart defaults intentionally lack published image
digests. The [native reference](docs/kube-ovn-extension-install.md)
retains the underlying patch/build steps for maintainers.

After administrator installation:

```sh
vpcctl --context management -n project-demo vpc create production
vpcctl --context management -n project-demo subnet create app-a \
  --vpc production --location dc-a --cidr 10.60.1.0/24
vpcctl --context management -n project-demo vpc list
```

All supplied addresses, names and topology examples are synthetic. Replace them
with values reserved and authorized for your deployment.

## Boundaries of 0.1.1

Cross-location operation requires the source-matched Kube-OVN
[destination-routes.v1 extension](integration/kube-ovn/README.md), including both
CRD schema and controller binary. Stock Kube-OVN alone does not implement this
exact contract. Kube-OVN owns the managed native OVN route configuration.

This is experimental software, not production qualification or a promise of
zero-loss failover, physical-site survivability, supported site count or NIC
line-rate throughput. Geneve and VXLAN/EVPN do not encrypt and require a trusted
underlay. Transport selection is immutable for an existing VPC. Gateway
replacement and expansion require explicit fenced recovery procedures.

Tenant UI, compute/KubeVirt attachment adapters, renewable platform identity,
and cross-site workload/storage orchestration remain separate work. The
bootstrap identity helper is for development and does not renew credentials.
Read [validation and maturity](docs/managed-validation.md) and the
[roadmap](ROADMAP.md) before choosing an adoption scope.

## Develop and verify

Use the Go toolchain declared in `go.mod` and Python 3.11 or newer.

```sh
make build
make test
make check
make test-race
```

API integration checks use `make test-integration` with `KUBEBUILDER_ASSETS`.
Native adapter tests have separate prerequisites. These checks do not substitute
for deployment-specific dataplane or hardware qualification.

The public entry point is `cmd/platform-vpc-controller`. Earlier controllers
remain in source as experimental references; their APIs and deployment files
are not interchangeable with the managed `v1alpha2` API.

## Participate

- [Contributing](CONTRIBUTING.md), [governance](GOVERNANCE.md) and
  [code of conduct](CODE_OF_CONDUCT.md)
- [Support](SUPPORT.md) and [security reporting](SECURITY.md)
- [Open-source readiness](docs/open-source-readiness.md) and
  [release procedure](docs/releasing.md)
- [License](LICENSE), [attribution](NOTICE) and
  [third-party licensing](THIRD_PARTY_NOTICES.md)

This is an independent project. References to Kubernetes, Kube-OVN, Open vSwitch,
FRRouting or cloud networking products do not imply upstream endorsement.
