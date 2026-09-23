# Site-autonomous Global VPC controller (D)

> Legacy experimental API reference. New deployments should start with the
> [managed VPC quick start](managed-quickstart.md).

For a complete two-site installation sequence, use the
[quick start](quickstart.md) and its paired deployment examples.

This branch adds `SiteVpc` and `site-vpc-controller`. The original `GlobalVpc`
controller remains available on `baseline/ovn-ic-alpha` and as a separate binary.
Do not run both implementations against the same owned networks.

Each instance manages one local Infra attachment using a local authoritative
API. A site with several Infra clusters can run separate attachment instances.
It has no peer-controller discovery, remote Kubernetes credentials, global
leadership, shared IC database, or synchronous IPAM call in reconciliation.

## Ownership and route exchange

An administrator assigns a durable `globalVpcID`, delegates non-overlapping
prefix pools inside that VPC, and distributes local grants. Different VPCs may
reuse prefixes. The local SiteVpc Kubernetes UID fences resource lifecycle; it
does not identify the global tenant. Resource names derive from global identity
and attachment identity, so recreating a SiteVpc cannot silently adopt an old
owner's resources.

The local controller renders a Kube-OVN Vpc, workload Subnet, protected transit
Subnet, and default routes to the local gateways. Kube-OVN remains the writer of
its OVN logical topology. The v2 HA adapter separately owns dedicated BFD rows
and only the native default routes' ECMP hash fields; see the
[explicit ownership contract](multi-gateway-ha.md). A gateway has a route back to the local
workload CIDR and advertises exactly that CIDR through FRR BGP. Remote workload
prefixes are learned through BGP, never rendered from another Controller's API.

Each tenant gateway has a separate Pod network namespace. The default WireGuard
profile uses per-gateway keys; native Geneve/BGP and VXLAN/EVPN profiles require
an explicitly trusted underlay. Tunnel interfaces are created in the host
network namespace and moved into the Pod: their outer UDP sockets use the
existing host underlay while tenant interfaces and routes remain in the Pod.
This needs no tenant routes on ToRs or changes to existing host BGP. The
[transport guide](transport-profiles.md) explains profile-specific registration,
native EVPN Type-5 forwarding, data-VNI BFD, and the encryption boundary.

Transport registration contains routing peers, their public keys, stable host
endpoints, ASNs, and authorized remote prefix pools. Routing peers are distinct
from Controller peers. WireGuard AllowedIPs and BGP inbound prefix lists restrict
remote source/prefix ownership. Outbound BGP exports only the local workload
prefix. An unreachable default in the gateway prevents unknown destinations
from looping through the OVN default route.

## Local operation and outage behavior

`LocalReady` means the local Kube-OVN resources, gateway process configuration,
and OVN route have converged. `GatewayConfigured` describes local readiness.
Neither condition asserts remote connectivity. BGP session and learned-route
observations belong to the routing layer and must be checked independently.
Gateway readiness checks configuration identity and routing-process liveness;
it does not continuously attest every kernel interface or forwarding entry.

Healthy gateway forwarding continues independently of the Controller process.
A failed or disconnected remote Controller does not gate local creation, repair,
or deletion. Local configuration is limited to previously delegated resources;
new grants, ownership transfers, global revocation, and global deletion still
require an administrator workflow. A missing site never authorizes reclaiming
its prefixes. IPAM reservations and gateway key Secrets are retained by the
controller on local deletion.

The v2 registration supports 2–8 active gateways per attachment, placed on
distinct nodes. The original single-gateway format remains supported. See the
[multi-gateway guide](multi-gateway-ha.md) for BFD, flow ECMP, partial-path
failures, and the local OVN services needed for convergence. One active local
Controller process is still required; a Kubernetes Lease is not a fence for
already-running external commands. Stop the previous process before replacing
it. Gateway redundancy does not establish physical site survival or replication
of arbitrary stateful middlebox sessions.

## Configuration and execution

1. Install `config/crd/networking.globalvpc.io_sitevpcs.yaml` on the local
   authority API. Provision a dedicated gateway namespace and local grants.
2. Build both binaries with `make build`. The D binary is
   `bin/site-vpc-controller`; it invokes `scripts/site_gateway.py` through a
   bounded JSON stdin/stdout protocol.
3. Adapt `config/samples/site.json` and `config/samples/sitevpc.yaml`. Only one
   local Infra kubeconfig and local NB/SB observer commands are accepted.
4. Build `gateway/Dockerfile` and distribute its image. For WireGuard, create a separate local
   Secret per gateway containing `privateKey`. Private key material never enters
   a SiteVpc, status, gateway protocol response, or tracked configuration.
5. Adapt `config/samples/site-gateway.json` or the Geneve/EVPN examples linked in
   the transport guide. Configure the adapter with `revision`, `siteID`, `attachmentID`,
   `kubeconfig`, `clusterUID`, `namespace`, `namespaceUID`, `image`, and `grants`.
   HA grants contain `globalVpcID` and `gateways`; each gateway binds a stable
   ID to a node, key Secret, ASN, router ID and per-peer `links`. The matching Go
   grant supplies gateway transit IPs and local BFD configuration. See the
   sample and multi-gateway guide for all fields. The earlier `nodeName`,
   `listenPort`, `privateKeySecretName`, `localTunnelIP`, `localASN`, `peers`
   single-gateway grant remains supported for v1.
6. Run with the local authority's `KUBECONFIG`:

   ```sh
   bin/site-vpc-controller --config /etc/global-vpc/site.json \
     --namespace global-vpc-system --resync-period 10s
   ```

The `GatewayRevision` must match the adapter registration. Accepted local intent
is immutable in this alpha, and the controller pins its accepted plan hash.
Each SiteVpc owns one workload Subnet. Disconnected creation of another granted
tenant is supported; adding or resizing subnets inside an existing attachment
requires a future API lifecycle extension.
Changing an accepted tenant's backend configuration requires an explicit
migration; silently retargeting existing resources is rejected. Adding an
unrelated tenant grant does not change an accepted tenant's plan. Preserve the
revision for unchanged existing grants when appending another grant.

HA requires `bfdTransactionCommand`, an administrator-owned `ovsdb-client
transact` command targeting the same local NB database. It atomically checks
that no static-route or policy reference remains before deleting a BFD row
whose UUID and ownership are already verified. This runtime access command is
excluded from the accepted plan hash, so changing its local wrapper or access
arguments does not alter tenant intent. It does not transfer resource ownership
or authorize retargeting an attachment to another database.

`config/rbac/site.yaml` describes local API permissions. The gateway adapter
creates privileged, host-PID Pods in its dedicated namespace, so keep that
namespace and the adapter registration under platform-administrator control.
The Controller does not read private-key Secret values itself. Local OVN
access needs separate credentials; HA additionally writes dedicated BFD rows
and guarded ECMP selection fields. An adapter configured to use `kubectl exec`
into an OVN central container requires broader permissions than a read-only
TLS DB client.
Do not grant tenants arbitrary access to the protected transit Subnet: labels
and excluded IP ranges are not an admission/security policy.

## Recovery and deletion

Every external resource is checked for owner and recorded UID. A gateway call
failure has unknown outcome: retry the same identity rather than deleting or
allocating a replacement. A subsequent read may recover a committed response
or confirm a missing resource. The Controller persists that observed inventory
before recreation; a same-name object with a changed UID blocks reconciliation.

Consumers must stop endpoint admission and drain local workloads before
deleting a SiteVpc. Local OVN ports, Pods, and Kube-OVN IP objects guard deletion;
only the exact owned gateway is excluded from the initial transit check. The
controller then removes its gateways, waits for CNI cleanup, withdraws routes
and removes unreferenced owned BFD rows in HA mode, then removes local
Vpc/Subnets, and confirms OVN topology absence before removing its finalizer.
It does not wait for any remote Controller or remove remote tenant resources.
These checks cannot fence a concurrent workload creator; draining is required.

## Validation

Run `make test`, `make test-race`, and `make check`. `make test-integration`
requires `KUBEBUILDER_ASSETS` and starts real isolated API servers; gateway and
OVN operations there are test doubles, not dataplane proof. Deployment
acceptance should separately verify bidirectional traffic, overlapping tenant
isolation, BGP withdrawal and prefix rejection, controller outages, independent
local provisioning, and endpoint-protected deletion.

`scripts/build_site_gateway.py` and `scripts/load_site_gateway.py` build/import
a gateway image using explicitly supplied access and image arguments.

References:

- [WireGuard network namespace semantics](https://www.wireguard.com/netns/)
- [FRR BGP filtering and route exchange](https://docs.frrouting.org/en/latest/bgp.html)
