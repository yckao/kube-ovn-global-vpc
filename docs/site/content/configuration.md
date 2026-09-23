# Configuration reference

Tenant intent stays small because administrators configure locations, network classes, pools and identities. Example values are placeholders; they are not reservations in your environment.

## Management configuration: platform.json

| Field | Purpose |
|---|---|
| `registryNamespace` | Namespace for retained authority allocation state; keep RBAC and CLI namespace consistent |
| `networkClasses` | Class name to `transportProfile` mapping |
| `locations[].name` | Value accepted as a public Subnet's `locationRef` |
| `locations[].region`, `site`, `dc` | Administrative topology metadata; a location still represents one Infra/DC |
| `locations[].bindingNamespace` | Management namespace for the location-scoped internal bindings and identity |
| `locations[].allowedProjects` | Project namespaces authorized to use the location |
| `locations[].cidrPools` | Reserved workload parent pools admitted for that location |

The supplied classes are `default` (`wireguard-bgp`), `trusted-geneve` (`geneve-bgp`) and `trusted-evpn` (`vxlan-evpn`). See [platform.json](../../../config/examples/managed/platform.json).

## Local configuration: site.json

| Field | Purpose |
|---|---|
| `locationRef` | Exact registered management location |
| `clusterUID` | Infra cluster's `kube-system` namespace UID; do not reuse an old cluster identity |
| `namespace` | Admin-only local operator/runtime namespace |
| `authorityNamespace`, `authorityKubeconfig` | Location-scoped management access |
| `gatewayNodeSelector` | Eligible Node labels; examples select `platform.globalvpc.io/gateway=true` |
| `endpointAnnotation` | Optional platform-managed routable endpoint; otherwise use Node InternalIP |
| `transitPool`, `transitPrefixLength` | Local gateway/native transit allocations |
| `bfdSourcePool` | Local native health-address allocations |
| `controlPool` | Inter-gateway control address allocations |
| `portStart`, `portEnd` | Reserved WireGuard UDP range |
| `gatewayReplicas` | Selected gateway members per location/VPC; example 2; fixed for a binding lifetime |
| `gatewayImage` | Immutable gateway image reference |
| `runtimeDir` | Runtime script directory inside the image |
| `mtu` | Explicit encapsulated path budget; example 1380, must be verified |

See [site-a.json](../../../config/examples/managed/site-a.json) and [site-b.json](../../../config/examples/managed/site-b.json). Keep control/health pools disjoint across locations and exclude infrastructure ranges from tenant pools. Size transit blocks for the selected member count.

## Deployment and RBAC

- [Authority manifest](../../../config/managed/authority.yaml): controller deployment and management permissions.
- [Site manifest](../../../config/managed/site.yaml): local operator and privileged gateway namespace boundary.
- [Location access](../../../config/managed/location-access.yaml): scoped access to each location's binding namespace.
- [Project role](../../../config/managed/project-role.yaml): public resource editing; bind it to actual project identities.
- [Network example](../../../config/examples/managed/network.yaml): Namespace, VPC and two Subnets.
- [Smoke Pods](../../../config/examples/managed/smoke-pod.yaml): resolve the native subnet name before application.

The controller Pods run non-root. Generated gateway Pods need privileged hostPID access for their networking role and do not receive a Kubernetes API token. Operator namespaces are an administrator trust boundary.

## Configure once, retain runtime state

Administrator configuration is desired policy. Runtime allocation registries, immutable anchors, generated keys and accepted snapshots are controller-owned state. Do not place generated identity state into a GitOps loop that repeatedly resets it to a template.
