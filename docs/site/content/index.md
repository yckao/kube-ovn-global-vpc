# Global VPC, native to your platform

Create a project-scoped VPC, add a Subnet in each location, and let the platform manage membership, gateways and native Kube-OVN configuration. Keep your routed ToRs unchanged.

> **v0.1.0 · experimental software.** The public API is `platform.globalvpc.io/v1alpha2`. This release is a reviewable engineering preview with explicit validation boundaries, not production qualification or a stable API promise.

## Start with the right guide

| Your task | Read next |
|---|---|
| Operate without a UI | [vpcctl: Helm lifecycle and VPC/Subnet operations](../../vpcctl.md) |
| Understand the design in ten minutes | [Architecture](architecture.md), then [packet path and transports](transports.md) |
| Install a two-location evaluation | [Installation quick start](../../managed-quickstart.md) and [configuration reference](configuration.md) |
| Audit or build the native extension as a maintainer | [Kube-OVN patch reference](../../kube-ovn-extension-install.md) |
| Integrate with an existing platform | [Kube-OVN integration](integration.md) and [API reference](api.md) |
| Operate or troubleshoot | [Operations](operations.md) and [HA and recovery](ha.md) |
| Brief engineers and technical leadership | [12-diagram engineering briefing](diagrams.md), with Traditional Chinese speaker notes |
| Decide whether to adopt | [Evidence and limitations](validation.md) and [release readiness](release-readiness.md) |

## A small tenant API

```yaml
apiVersion: platform.globalvpc.io/v1alpha2
kind: VPC
metadata:
  name: production
  namespace: project-demo
spec: {}
---
apiVersion: platform.globalvpc.io/v1alpha2
kind: Subnet
metadata:
  name: app-a
  namespace: project-demo
spec:
  vpcRef: production
  locationRef: dc-a
  cidr: 10.60.1.0/24
```

Add a second Subnet with `locationRef: dc-b` and a distinct CIDR to connect another location. Administrators supply approved network classes, address pools, eligible gateway nodes and scoped identities once. Tenants do not maintain gateway IP lists, keys, ASNs or peer sessions.

## Four deliberate boundaries

- **Global intent, local execution.** Each Infra cluster owns its native VPC, accepted snapshot and gateway runtime. Site operators do not call each other.
- **One owner for native routes.** The local operator writes native APIs. A source-matched Kube-OVN extension configures destination ECMP/BFD in OVN.
- **Controllers stay off the packet path.** Existing accepted forwarding can continue through bounded management outages. New intent still needs the management service.
- **Evidence is explicit.** Unit, API, native integration and packet tests cover different claims. Hardware independence, production throughput and live large-site forwarding require separate qualification.

## What this project is

This independent project provides routed IPv4 tenant networking across Kube-OVN Infra clusters. A location is one Infra/DC allocation domain. A VPC can span locations; an individual Subnet does not stretch across them. Different VPCs can reuse CIDRs, while Subnets within one VPC must not overlap.

The interface is inspired by familiar cloud networking: project, VPC, location and Subnet. It does not claim GCP feature parity. Compute provisioning, a tenant UI, managed firewall policy, workload-cluster control-plane topology, storage mobility and VM live migration are separate workstreams.

The repository also retains earlier `global-vpc-controller` OVN-IC and `site-vpc-controller` experiments. This website's installation and API guidance target **`platform-vpc-controller`**. Historical experiment results are not silently transferred to this implementation.
