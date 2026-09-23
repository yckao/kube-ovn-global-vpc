# API reference

The managed API group is `platform.globalvpc.io/v1alpha2`. All three resource kinds are namespaced. VPC and Subnet are public; NetworkBinding is internal platform state. The `v1alpha2` API is an alpha contract and does not promise schema or storage compatibility across later releases.

## VPC

A project-scoped logical network. It does not create a stretched L2 domain, shared OVN database or automatic adoption of existing native Vpcs.

| Spec field | Type | Meaning |
|---|---|---|
| `networkClassRef` | string, optional | Approved class; defaults to `default`; transport is immutable for an existing VPC |

| Status field | Meaning |
|---|---|
| `observedGeneration` | Generation represented by status |
| `networkID` | Allocated global network identity |
| `phase`, `conditions` | Reconciliation phase and Kubernetes conditions |
| `bindings` | Location, name, UID and projected native VPC references |
| `subnetClaims` | Serialized prefix admission receipts, including terminating claims until withdrawal completes |

## Subnet

A routed prefix in one authorized Infra/DC location. Subnets within one VPC cannot overlap; different VPCs may reuse prefixes.

| Spec field | Type | Meaning |
|---|---|---|
| `vpcRef` | string, required | VPC in the same project namespace |
| `locationRef` | string, required | Registered and project-authorized location |
| `cidr` | string, required | Canonical unicast IPv4 prefix; managed public admission supports /8 through /30 |

| Status field | Meaning |
|---|---|
| `observedGeneration`, `phase`, `conditions` | Current reconciliation observation |
| `vpcUID` | Bound VPC incarnation |
| `bindingName` | Internal location binding |
| `nativeVpcName`, `nativeSubnetName` | Native names for platform attachment and diagnostics |

Do not guess the native object names. Use `status.nativeSubnetName` when integrating endpoint attachment.

## NetworkBinding: internal snapshot

Only platform identities may write desired bindings or status. A Ready gateway report is an observation; it does not grant membership or permission to export arbitrary prefixes.

| Spec group | Fields |
|---|---|
| Identity | `vpcRef` (namespace/name/UID), `networkID`, `locationRef`, `nativeVpcName` |
| Policy | `networkClassRef`, `transportProfile`, `localASN`, `authorizedLocations` |
| Routes | Local `subnets`, `remoteSubnets` with location and CIDR |
| Discovery | Public `peers`, native per-pair `linkAllocations` |
| Lifecycle | `revision`, explicit `deleting` tombstone |

Status contains `observedGeneration`, `appliedRevision`, `phase`, conditions, exact native/runtime resource records and public gateway receipts. Gateway endpoint receipts can contain Node name/UID, routable endpoint, transit/health address, public key, ASN and per-peer allocation data. Private keys never belong in this object.

## Lifecycle and schema sources

Managed objects use finalizer `platform.globalvpc.io/network-cleanup` and UID-based ownership. Do not bypass finalizers or edit status to resolve failures. An object absent from a management list is not permission to delete its local accepted state.

Canonical definitions:

- [Go API types](../../../api/v1alpha2/types.go)
- [VPC CRD](../../../config/crd/platform.globalvpc.io_vpcs.yaml)
- [Subnet CRD](../../../config/crd/platform.globalvpc.io_subnets.yaml)
- [NetworkBinding CRD](../../../config/crd/platform.globalvpc.io_networkbindings.yaml)

The separate native `Vpc.spec.destinationRoutes` contract is documented in [Kube-OVN integration](integration.md) and the [native extension reference](../../../integration/kube-ovn/README.md).
