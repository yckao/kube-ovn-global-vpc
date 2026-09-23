# Global VPC legacy experimental architecture

> Legacy experimental API reference. New deployments should start with the
> [managed VPC quick start](managed-quickstart.md).

## Scope

The network controller owns tenant network intent, participating Infra clusters,
address claims, interconnection policy, reconciliation and cleanup. VM creation,
CAPI/CAPK placement and workload Kubernetes bootstrap are consumers of this
network and are outside this project. A workload cluster's control-plane HA is
also outside this controller.

A deployment can use **region -> site -> DC** labels to describe failure
domains. Each Infra Kubernetes/OVN deployment retains its own identity and
local control plane. Stable cluster UIDs identify attachments; location names
must not replace identity. Management-cluster placement and the number of
sites are deployment choices.

## Fully attached reference topology

Two Global VPCs (red and blue) each have one local VPC per DC. Both tenants reuse
the same site prefixes. Their transit networks use separate OVN-IC Transit
Switches. Each site subnet is routed, and the transit switch joins routers.
This does not implement tenant endpoint L2 stretching or IP mobility.

The legacy `GlobalVpc` controller reconciles intent from one authoritative
Kubernetes API into registered Infra clusters. Native ovn-ic runs in each
Infra cluster and exchanges interconnection state through shared IC NB/SB
databases. This reference topology does not establish production HA.

## Object and field ownership

| Object or field | Writer |
| --- | --- |
| Global intent and prefix claims | Experiment coordinator / future Global Controller |
| Local Vpc/Subnet specs and attachment ConfigMaps | Coordinator |
| Local logical router, subnet switch, local router/switch ports | Kube-OVN |
| Static routes represented by Vpc.spec.staticRoutes | Kube-OVN |
| IC NB Transit_Switch definitions | Coordinator |
| Local transit switch other_config:interconn-ts | Adapter phase |
| Local transit router port gateway_chassis | Adapter phase |
| Transit switch requested tunnel key, remote ports, IC SB data | Native ovn-ic |
| Node ovn.kubernetes.io/ic-gw label | Adapter phase, with recorded previous value |
| OVS ovn-is-interconn flag and local SB chassis registration | Kube-OVN node daemon / ovn-controller |
| Local Pod ports and per-endpoint address assignment | Kube-OVN |

The installed Kube-OVN has enable-external-vpc=false. Its logical-switch GC can
delete a custom transit switch without a matching Subnet. This experiment uses
an explicit Kube-OVN Subnet for each transit switch, reserves every usable
transit address against endpoint allocation, disables DHCP/LB, and assigns a
different gateway IP in each DC. Kube-OVN owns the local router connection.
The adapter adds only the IC marker and gateway chassis. Existing switches must
be marked before native ovn-ic starts to avoid duplicate switch creation.

The local VPC names include the DC identity, making the generated router-port
and switch-port names unique across the IC domain. The transit-switch name is
shared across attachments of one Global VPC. A shared transit Subnet name in
different Kubernetes APIs is intentional.

This is a compatibility hypothesis that needs runtime evidence, including
reconcile, garbage collection, daemon restart and detach. It also has a scaling
limit: native ovn-ic can replicate all Transit Switches in an IC domain to each
participating OVN deployment, even when a local router is not attached. Sparse
VPC membership, future DC onboarding and GC of unattached switches need a
separate design; the legacy fully attached topology does not resolve them.

Kube-OVN's node daemon owns the OVS `ovn-is-interconn` flag. Set the native
Node label and let that daemon manage the flag to avoid competing writers.

Full Kube-OVN gc() runs at leader initialization. The 360-second loop runs
markAndCleanLSP only. A waiting period cannot establish that full
logical-switch GC ran. Validate the relevant switch-GC keep predicates and
controller initialization behavior separately.

## Site-autonomous implementation (D)

The `feature/site-autonomous-d` branch implements an independent local authority
per attachment. The former OVN-IC implementation and the shared-quorum proposal
below are preserved in baseline commit `59c36d1` on `baseline/ovn-ic-alpha`.

The D Controller owns only its local SiteVpc, Kube-OVN resources and local
gateway configuration. It does not discover or wait for another Controller,
read remote Kubernetes APIs, acquire a global lock, or call a shared IC database.
An administrator provides stable global identity and locally durable prefix
delegations before autonomous operation. Different tenants may overlap; prefix
ownership within one routed tenant must remain disjoint.

Cross-site routes are exchanged by FRR BGP over per-tenant WireGuard tunnels.
The gateway's encrypted socket uses the host's existing L3 underlay; cleartext
tenant routes remain in separate Pod network namespaces. ToRs retain their
underlay routing role. Local provisioning does not wait for routing peers, and
`LocalReady` explicitly does not mean remote reachability.

The original implementation used one gateway per attachment. Its v2 registration
adds active-active gateways on distinct nodes, local OVN BFD, per-link WireGuard,
BGP multipath, and same-site iBGP fallback. Explicit five-tuple ECMP fields let
one workload distribute multiple flows. Only one local Controller process is
active; general multi-writer fencing remains unsupported. See
[multi-gateway HA](multi-gateway-ha.md) for exact ownership and failure boundaries.
Global revocation, prefix transfer, and all-site deletion remain separately
coordinated workflows. See [the D guide](site-controller.md) for the actual
supported contract and evidence boundaries.

## Shared-quorum HA direction (B, retained proposal)

Design availability independently for the Global API/store, reconcilers, IC
databases, local adapters, gateways, and external IPAM.

1. Separate a control domain from the region/site/DC location hierarchy. A
   proposed ControlDomain identifies a set of existing site Management clusters
   hosting shared control services and the VPCs assigned to that authority. It
   does not require a regional Management cluster, every site's participation
   in a database quorum, or three sites in every region. A VPC's participating
   DCs need not be the sites hosting its authority. A directory can locate the
   authoritative domain without joining the established packet-forwarding path.
   Cross-region membership of a control domain is an option only if acceptable
   latency and operational/data-boundary requirements permit it; no such layout
   has been selected. This placement decision does not by itself implement
   cross-region tenant routing.
2. If three suitable independent sites exist, place one member of each shared
   authoritative database in each site's Management cluster. Each database then
   has a three-member quorum spanning sites, subject to measured RTT, durable
   storage and reliable management-network endpoints. Three DCs within one site
   cannot provide the equivalent site-failure tolerance. Keep the Management
   clusters' own Kubernetes APIs and etcd independent; this proposal clusters
   application databases across them, not their Kubernetes control planes.
3. Run Global Controller replicas in multiple site Management clusters, with
   durable per-shard leadership and fencing against shared authority. A Lease in
   each independent Kubernetes API does not elect a global leader. A GlobalVpc
   CR copied between site APIs is not automatically a shared source of truth:
   local APIs may hold requests or projections, while accepted global intent
   needs an explicit authoritative store/protocol. Its implementation remains
   undecided. Committing intent and projecting it to IC NB/local APIs requires
   retryable reconciliation, not an assumed transaction across databases.
4. Scope IC NB and IC SB database clusters to the selected interconnection
   domain. Each needs its own quorum; sharing site placement with an intent store
   does not merge these databases. Native ovn-ic in each DC connects to multiple
   reachable members. Keep their bootstrap, addressing and storage independent
   of the tenant VPC being controlled. Do not assume one worldwide synchronous
   quorum is desirable. Benchmark convergence and inter-site latency before
   choosing the domain size.
5. Keep adapters and redundant interconnect gateways local to each DC. Preserve
   the last accepted configuration during control-plane outages. Existing flows
   and new attachment/allocation operations have different availability claims.
6. When external IPAM is unavailable, retain existing claims and dataplane.
   New allocation waits unless the provider explicitly supports delegated,
   durable pools. NetBox HA belongs to the external service's architecture.

With only two independent sites, a majority-based store cannot promise writable
availability after arbitrary loss of either site. A third voting failure domain
or a more limited failure/recovery promise is necessary. For clustered OVSDB,
that third participant must be an actual database member; a generic witness
service is not a substitute. Site count varies by region; actual candidate
placements and inter-site RTT are still unknown. A one- or two-site region could
use a control domain also hosted in another region if that is acceptable, or
expose a more limited availability promise. No production layout is selected by
this basic experiment.

Region/site registration must not automatically add/remove voting database
members. Treat quorum membership as an explicit, controlled lifecycle. Expose a
domain's configured failure tolerance separately from its replica count; the
site Management clusters may themselves have correlated physical dependencies.

In a one-site region, surviving loss of that site requires authoritative replicas
outside the region; more replicas within the same site cannot satisfy that
promise. A region with more than five sites does not need a voter in every site.
A selected three- or five-member OVSDB cluster can serve clients in all those
sites, provided connectivity and capacity requirements are met. Hosting control
state for another region is separate from stretching tenant subnets across
regions. Neither a one-domain-for-all-sites topology nor one-domain-per-region
is mandated; choose domain boundaries from availability and latency requirements.

If independent site writes during a partition are required, that is a different
authority design. It needs explicit ownership/delegated address pools and merge
semantics. Two autonomous IC database pairs are separate IC domains; native
ovn-ic does not turn them into one federated shared database. Cross-domain
connectivity would require additional design and validation.

The intended outage policy is to stop conflicting global mutations when
authority is unavailable, while local DCs retain their last accepted network
configuration. Continuity of forwarding between surviving reachable endpoints
must be tested separately; it does not imply reachability to a failed site or
unlimited autonomous gateway/topology changes. A site Management API outage and
loss of the entire site are different fault cases.

References:

- [OVN interconnection database placement and clustered client endpoints](https://docs.ovn.org/en/latest/tutorials/ovn-interconnection.html)
- [OVSDB clustered service model and majority semantics](https://docs.openvswitch.org/en/stable/ref/ovsdb.7/)

## IPAM plugin contract

The coordinator invokes a configured executable argument array without a shell.
One versioned JSON request goes to stdin and one response comes from stdout.
This allows an external IPAM adapter in any language without modifying network
reconciliation. Credentials remain in provider-local configuration.

Current operations: Capabilities, EnsurePrefix, GetPrefix, ReleasePrefix.
Requests identify a stable VPC UID, claim UID, provider scope reference and an
explicit prefix. Receipts contain the provider allocation ID, CIDR and retention
policy. Repeating a claim must rediscover its existing allocation, including
after a committed write whose response was lost. Timeouts mean unknown state,
not absence. Prefixes may overlap across VPC scopes, not within a routed VPC.

The NetBox provider uses one preprovisioned VRF per VPC and REST calls.
Its credentials need permission to view the selected VRFs and view/add
prefixes inside them. Release is deliberately rejected (Retain).

This provider currently accepts explicit prefixes; choosing a prefix from a
pool, individual address claims and credential rotation are future extensions.
NetBox REST does not make the claim-key read/create sequence atomic. Production
requires a fenced single writer per scope or a backend providing atomic
idempotency/uniqueness. The prototype advertises atomicClaimUpsert=false.

## Compatibility routing boundaries

A platform-owned compatibility routing domain can connect a shared network
to newer tenant networks through separately allocated prefixes and explicit L3
policy. Tenant VPCs retain independent routing contexts and may overlap each
other; their routes must not be exported indiscriminately into a shared domain.

Application migration requires control of new connections at each relevant
entrypoint while established connections drain. Network connectivity alone
does not provide zero-downtime application migration or transfer TCP state
between servers.

## Validation boundaries

An Infra-level dataplane check does not establish guest VM NIC behavior, nested
CNI compatibility, hardware offload, migration continuity, storage HA, or
physical disaster recovery. Tenant isolation and NetworkPolicy enforcement
require separate acceptance tests.
