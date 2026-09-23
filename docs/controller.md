# Global VPC Controller v1alpha1

> Legacy experimental API reference. New deployments should start with the
> [managed VPC quick start](managed-quickstart.md).

This legacy Go controller implements a routed, fully attached
Global VPC mechanism. It owns `GlobalVpc` intent in one
explicit authoritative Kubernetes API and projects it into registered independent
Infra APIs. It does not provision VMs, CAPI clusters, workload Cilium, external
compatibility routing, or a distributed authoritative store.

## Supported contract

- One authoritative Kubernetes API, one controller replica, one registered IC
  domain, and one serialized reconcile worker. The manager uses a Kubernetes
  Lease; a separate durable ConfigMap serializes topology operations.
- IPv4 routed site subnets; one site subnet per attachment. Every registered
  Infra cluster must participate in every VPC in the domain. Sparse membership
  and domain membership changes are deliberately rejected.
- The entire v1alpha1 spec is immutable. Changing topology requires a separate
  VPC and a consumer migration. No in-place detach, prefix resize, or IP mobility
  is implemented.
- Stable Kubernetes VPC UID and actual Infra `kube-system` Namespace UIDs drive
  resource names, labels and prefix claims. Region/site/DC are location metadata.
- CIDRs can overlap between tenant scopes. All site and transit prefixes inside
  a routed VPC must be disjoint; transit gateway addresses must be distinct.
- The executable IPAM protocol and existing NetBox plugin retain reservations.
  A receipt is persisted before creating networks. Retries use the same claim
  identity; timeouts never authorize release or replacement allocation. Existing
  receipts permit reconciliation during a provider outage.
- Consumers select the generated local Subnet explicitly, for example with
  `ovn.kubernetes.io/logical_switch`. The controller neither creates namespaces
  nor binds existing namespace defaults. Status exposes generated names.

A NetBox VRF must have `description=gvpc-scope:<GlobalVpc metadata.uid>` and the
spec's `scopeRef` must be its numeric ID. Pre-create the VRF, create the GlobalVpc,
then set that ownership description using its server-assigned UID. Reconciliation
waits safely on the provider until the scope is bound. Use a separate VRF per VPC.
The sample values are documentation addresses and placeholders, not IPAM grants.

## Ownership

| State | Writer |
| --- | --- |
| GlobalVpc status, finalizer, domain operation lock | Global VPC Controller |
| Owned local Vpc staticRoutes and Subnet network fields | Global VPC Controller through native APIs |
| Routers, local switches, ports and static route NB records | Kube-OVN |
| Owned transit switch `other_config:interconn-ts`, router port `gateway_chassis` | OVN adapter |
| IC NB Transit_Switch and its owner external ID | Global VPC Controller |
| Remote ports, tunnel keys, IC SB state | Native ovn-ic |
| Dedicated native IC Deployment `spec.replicas` | Domain lifecycle gate |
| Domain AZ names, Node gateway labels, gateway registration, IC DB/TLS provisioning | Platform administrator |
| OVS interconnection flag derived from Node labels | Kube-OVN node daemon |
| Workloads and endpoint lifecycle | Consumers |

Existing objects require matching owner/cluster labels and, once recorded, exact
UIDs. Foreign objects are never adopted. API changes use optimistic concurrency;
deletes carry UID and resourceVersion preconditions. OVN deletion verifies row
UUID, name and owner within the mutation transaction. `get` adds OVSDB column
verification before `wait-until`; a bare wait alone is insufficient.

Controller spec fields are repaired while vendor-defaulted fields and status
are preserved. Static routes are rendered as a complete stable set on every
ordinary pass. All usable transit addresses are excluded from endpoint IPAM.

## Native IC bootstrap barrier

The experiment initially started native IC after preparing switches. Continuous
operation requires an explicit barrier: native IC can create a duplicate named
switch when an IC transit definition precedes the local marker, and deletes a
marked switch when no IC transit definition exists. Neither ordering alone is
safe across separate NB databases.

For first creation, recorded transit loss recovery, and tenant deletion:

1. Persist finalizer and accepted plan hash, verify every Infra identity and
   resource ownership, and obtain the durable domain lock.
2. Stop only the explicitly registered dedicated native IC Deployments. Verify
   Deployment and owned ReplicaSet observed generations, zero desired replicas
   in every owned ReplicaSet, and absence of all nonterminal owned Pods. A
   terminating Pod still blocks the barrier.
3. With all native IC writers stopped, prepare the Kube-OVN-owned local transit
   switches, mark them, and create the owner-tagged IC definition. During initial
   provisioning the local resources and readiness checks precede the stop barrier.
4. Persist the IC row identity and `operation=Resume`, restore dedicated IC
   Deployments to one replica, wait for their observed readiness, and release
   the lock. A restart resumes this durable operation instead of pausing again.
5. Verify local Subnet readiness, native remote ports/tunnel keys and the exact
   destination-route set. Poll again to detect remote drift.

The domain registration must enumerate **all native IC processes**. External
standbys or another controller managing these Deployment replicas invalidate
this barrier. Do not label an existing shared Kube-OVN/CNI Deployment as owned.
The gate changes only dedicated native IC process replicas; it never restarts
Kube-OVN, ovn-central, OVS or Infra nodes.

This alpha implementation temporarily stops IC topology propagation domain-wide
for these operations. Existing forwarding continuity during this barrier has
not been measured by this controller. A failed operation intentionally retains
the domain lock and may leave native IC paused until the holder can recover;
other VPC topology operations wait. Repair the reported dependency and restart
with the same registration rather than deleting the lock or removing finalizers.

Lease leadership and the ConfigMap lock do **not** provide external stale-writer
fencing for OVSDB/NetBox. Run one controller replica and stop the old process
before replacement. No writable multi-site authority or safe partitioned leader
failover is claimed. The NetBox REST read/create sequence also needs exclusive
scope writers; unrelated automation must not allocate in those scopes.

## Deletion and recovery

Consumers must stop new endpoint admission and drain every endpoint in both the
site and transit subnets before deleting a GlobalVpc. The controller checks OVN
ports before and after the native IC stop barrier, but cannot fence a workload
creator or detect every in-flight CNI allocation. It never deletes consumer Pods,
VMs, namespaces or endpoint finalizers.

Deletion withdraws only owned Vpc routes, deletes the owned IC transit definition,
then deletes owned Subnets and Vpcs. It confirms local OVN object absence before
resuming IC and removing the GlobalVpc finalizer. An unavailable Infra/API/OVN,
foreign ownership, a replaced UID, or a remaining endpoint blocks cleanup. IPAM
allocations remain retained; the controller has no Release implementation.

Before recreating a previously recorded missing Kubernetes resource or IC row,
the controller persists that absence by clearing its recorded UID. Thus a lost
creation response can be rediscovered safely. An unexpected replacement found
without this durable absence step is rejected.

The accepted plan hash includes domain identity, connection command registration,
and generated resources. Changing registration for an accepted VPC is blocked,
including during deletion. Credential material may rotate in place at the same
configured paths; changing cluster/Deployment identities needs a separate,
explicit migration workflow that is not part of v1alpha1.

## Build and run

Requires Go 1.26, Kubernetes 1.35-compatible CRD CEL support, administrator-provided
`ovn-nbctl`, `ovn-sbctl`, `ovn-ic-nbctl`, and the selected IPAM executable. OVN CLI
versions must match the registered domain.

```sh
make build
make test
make test-race
make check
```

Install `config/crd/networking.globalvpc.io_globalvpcs.yaml` and
`config/rbac/authority.yaml` into the explicitly selected authority API. Apply
scoped Infra roles/bindings for dedicated credentials, including the named
Deployment role in `config/samples/native-ic-access.yaml`. The controller does
not need access to Infra Secrets or permission to change Nodes.

Copy `config/samples/domain.json` to an administrator-managed location and replace
all placeholders with live registered identities and reachable TLS endpoints.
Use mounted or existing kubeconfigs without copying their contents into this
repository. Commands are argument arrays, never shell strings, and are available
only in administrator configuration, never in a tenant CR. TLS arguments should
reference files; do not embed credentials in command arguments.

Native IC prerequisites are provisioned separately: durable IC NB/SB databases,
AZ names, node gateway labels, registered SB gateway chassis, and one dedicated
Deployment per Infra with `networking.globalvpc.io/domain: <domain>` on both its
metadata and Pod template. Record each Deployment UID. The controller owns that
Deployment's replica count (zero during barriers, one otherwise). Automatic IC
route advertisement/learning must remain disabled; routes have one writer.

```sh
KUBECONFIG=/existing/authority-kubeconfig \
GVPC_NETBOX_URL=https://netbox.example \
GVPC_NETBOX_TOKEN_FILE=/existing/private/netbox-token \
bin/global-vpc-controller --config=/existing/private/domain.json
```

`config/manager/deployment.yaml` is a deployment template for an image containing
the built binary plus the configured provider/OVN executables. Its ConfigMap and
Secrets are supplied out of band. No image is published and no live domain is
installed by the development test target. Process `/readyz` checks manager
availability; inspect GlobalVpc status for dependency/topology readiness.

## Validation and evidence boundaries

`Ready=True / ConfigurationConverged` means the accepted Kube-OVN Subnets report
Ready, the intended IC transit ports/tunnel keys exist, and native destination
routes match intent. It is not a packet-delivery, continuous-availability, MTU,
isolation, full tenant-switch drift, physical-HA or workload-cluster readiness
claim. Functional probes remain a separate acceptance step.

Unit and subprocess tests cover deterministic plans, overlapping-tenant isolation
in rendering, invalid topology, receipt identity/redaction, timeout and lost
responses, ownership/UID conflicts, remote API failures, lifecycle barriers,
restart replay, endpoint-protected deletion, retained allocation state and OVN
command parsing/transaction guards.

The integration target starts three isolated API servers/etcd stores with the
real GlobalVpc CRD and simulated Kube-OVN CRDs. It exercises immutable-spec CEL,
status persistence, replay and finalizers over actual APIs. OVN/IPAM and native
IC process behavior are test doubles there; these tests do not establish
end-to-end dataplane behavior.

```sh
KUBEBUILDER_ASSETS=/path/to/envtest-binaries make test-integration
```

Live acceptance still requires a fresh isolated domain and new IPAM scopes:
create two overlapping tenant VPCs; verify bidirectional protocols/source IP/MTU
and negative cross-tenant controls; replay and repair drift; restart the controller
at durable phase boundaries; test provider/API outages; measure traffic during
native IC barriers; drain/delete one VPC while the other remains functional; and
prove local/IC/backend absence separately from retained IPAM reservations.

Source references behind the bootstrap and ownership design:

- [OVN interconnection tutorial](https://docs.ovn.org/en/latest/tutorials/ovn-interconnection.html)
- [OVN native IC switch reconciliation](https://github.com/ovn-org/ovn/blob/branch-25.03/ic/ovn-ic.c)
- [OVS database command transaction verification](https://github.com/openvswitch/ovs/blob/branch-3.5/lib/db-ctl-base.c)
- [Architecture and unresolved HA/sparse-membership boundaries](architecture.md)
