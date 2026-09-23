# Site-local transport profiles

> Legacy experimental API reference. New deployments should start with the
> [managed VPC quick start](managed-quickstart.md).

The D gateway adapter supports three complete profiles. An administrator chooses
one for each Global VPC grant. The local controller, immutable intent, delegated
IPAM ownership, OVN ECMP/BFD and guarded deletion are shared. Controllers use only
their local API and local OVN databases; route exchange runs in gateway daemons.

| Profile | Route exchange | Data encapsulation | Endpoint authentication/encryption |
| --- | --- | --- | --- |
| `wireguard-bgp` (default) | FRR IPv4 eBGP/BFD | WireGuard | WireGuard keys and AllowedIPs |
| `geneve-bgp` | FRR IPv4 eBGP/BFD over each native Geneve link | Native Linux Geneve | Trusted underlay; source-prefix filter only |
| `vxlan-evpn` | FRR BGP EVPN Type-5 over separate control VXLAN links | Native Linux VXLAN L3VNI | Trusted underlay; RT/prefix policy and source-prefix filter |

These are interchangeable deployment profiles, not protocol translation. All
attachments of one VPC interconnection domain must use the same profile and
compatible registration. Independent VPCs can choose different profiles. This
implementation provides routed IPv4 connectivity, not a stretched tenant L2,
EVPN all-active Ethernet segment, MAC mobility service, or cross-site transit.
The pre-existing v1 single-gateway format remains WireGuard-only; new overlay
profiles require the v2 active-active gateway registration.

## Preserve the existing ToR fabric

Every outer endpoint is an existing, reachable host address. The runtime creates
a down tunnel interface in the host network namespace, marks its exact owner,
then moves it into the tenant gateway Pod namespace. Linux retains the socket's
creation namespace for the outer packet route lookup. The overlay therefore
uses the host's existing routed uplinks while tenant routes stay inside its Pod.

The adapter does not configure ToRs, host BGP, host routes, host firewall rules,
or host sysctls. EVPN runs between gateway daemons. The ToRs carry ordinary IP/UDP
packets and do not become EVPN peers or VTEPs. No new host loopback or moving
workload prefix is advertised to the fabric.

This requires existing bidirectional IP reachability and UDP allowance for the
registered endpoints/ports. If the existing fabric blocks those packets, an
unchanged fabric cannot support that registration. Native fixed-mode Geneve has
no selectable local source-address field: preflight requires `ip route get` to
select the registered `underlayIP` already. It fails rather than rewriting host
routes. VXLAN explicitly selects `localVtepIP`. Both profiles require the
registered source to exist on the host. For each reachable route, they verify
that the configured inner MTU plus 50 bytes fits its local route/device MTU.
These are startup snapshots, not continuous route attestation or discovery of
a smaller downstream path MTU; end-to-end DF probes remain necessary.

A missing remote host route is recorded as `underlay-route-unavailable` and
does not block the gateway's healthy links or local sibling sessions. Native
tunnels can start without that route; BFD/BGP recover when the host FIB becomes
usable. Source-selection and path-MTU checks are skipped for that unreachable
peer and are not automatically revalidated when the route returns. The platform
must preserve the registered source selection and MTU across underlay changes.
Unknown inspection errors, missing local source addresses, and observed source
or MTU mismatches remain fatal.

## Native EVPN forwarding and health

Each gateway has a tenant VRF, L3 bridge, one L3VNI, a unique RD/router MAC, and a
shared tenant RT. FRR learns authorized Type-5 prefixes and installs the VRF
routes, remote router MACs and VTEP entries through zebra. The Python supervisor
does not parse EVPN NLRI into manually installed tenant routes.

Point-to-point control VXLAN interfaces carry eBGP/BFD, independently of the
multipoint data VNI. Control BFD alone cannot prove that data packets cross that
VNI. Each gateway therefore originates an administrator-reserved health `/32`
from a dummy interface in its tenant VRF. It imports only the exact health `/32`
registered for each remote VTEP, and runs multihop BFD between those addresses
through the real data VNI.

A remote workload import starts denied and becomes eligible only after its
exact data BFD session is Up. A native FRR route-map gate and inbound soft refresh
remove the failed peer's workload paths. The exact health route remains admitted
so recovery does not depend on an already-working workload route. The gate never
removes the permanent RT, Type-5 and delegated-prefix checks. Health routes are
excluded from sibling route exchange. This OAM detects reachability loss along
the sampled path; it does not establish every flow, packet size, or underlay
ECMP branch as healthy.

EVPN peers must negotiate BGP Route Refresh. Policy changes use native inbound
refresh without an Adj-RIB-In soft-reconfiguration cache. This avoids the
cache lifetime defect documented in FRR's [10.2 backport](https://github.com/FRRouting/frr/pull/20078).
A failed refresh command stops the supervised gateway rather than recording an
unapplied health gate. Local readiness does not wait for a remote peer.

Local OVN BFD separately gates the gateway's workload-prefix advertisement.
When local transit fails, the gateway withdraws that prefix but retains its OAM
prefix. Same-site iBGP can provide an authorized remote-prefix fallback when a
member's WAN or data VNI fails. Graceful restart is disabled for these peers to
avoid intentionally retaining stale forwarding paths.

## Registration and ownership

Start with the existing [local controller registration](../config/samples/site.json)
and choose a matching gateway example:

- [WireGuard](../config/samples/site-gateway.json)
- [Geneve/BGP](../config/samples/site-gateway-geneve-bgp.json)
- [VXLAN/EVPN](../config/samples/site-gateway-vxlan-evpn.json)

Use the same `revision` in the gateway adapter and matching Go gateway grant.
Distribute `gateway.py` and its sibling `overlay.py`/`evpn.py` together at the
registered `runtimePath`. The adapter packages the required modules into the
owned ConfigMap and includes their content in the immutable configuration hash.
WireGuard grants retain their existing key volume and default profile behavior;
the unencrypted profiles have no key Secret or key mount.

Common overlay fields bind each local gateway ID to one distinct node, local ASN,
router ID, MTU, and authorized direct links. Each link has a remote attachment,
ASN, delegated pools, separate point-to-point control addresses, VNI, UDP port,
and remote host endpoint. Both ends use the same link VNI and destination port,
with reversed control addresses and endpoint identities. Remote gateways of one
attachment may advertise the same pool; unrelated owners in a VPC may not.

EVPN additionally requires `localVtepIP`, unique `healthIP`, common `vni` and
`routeTarget`, unique `routeDistinguisher`, and common `vxlanPort` (default 4789).
Each link supplies its remote gateway's `remoteHealthIP`. The supported RD/RT
registration syntax is `ASN16:number32`. The sample uses unique RDs per gateway;
keep them unique across the whole EVPN domain. Reserve one health address per
VTEP and tenant. Health, control, underlay, local transit and delegated workload
addresses must not overlap within one tenant's applicable scope.

Unencrypted grants require `trustedUnderlay: true`. Pod-local nftables source
filters reject decapsulated sources outside the authorized remote pools/control
addresses. Those filters and EVPN route targets do not authenticate a malicious
underlay sender. VXLAN/Geneve provide no confidentiality. Choose WireGuard when
the WAN/underlay is untrusted or confidentiality is required. Restrict all
registration, privileged gateway Pods, transit access and runtime modules to
platform administrators.

A complete local registration is checked for `(node, UDP port, VNI)` collisions
before resource writes. Multiple native overlay VNIs can share a matching
protocol's UDP port. WireGuard listeners remain unique. Independent registration
files still need a common platform port/VNI allocation policy; there is no
cross-controller lock or dynamic allocation service. A kernel collision fails
closed and never permits deleting another owner's interface. Cross-site
compatibility is an administrator contract rather than an API handshake.
Reserve these ports independently of the existing CNI and other host services.
The examples use 46081/46089 rather than sharing an existing CNI listener;
verify their availability on the target hosts before assigning them. The
registration collision check covers controller grants, not every host process.
Linux's fixed-mode tunnels cannot safely share those ports with an OVS/CNI
`collect_md` tunnel. A conflict can reject tunnel creation or link activation;
starting the gateway first can instead prevent the CNI from acquiring its port.

## Failure and lifecycle boundaries

All profiles share the [multi-gateway HA contract](multi-gateway-ha.md). Each
accepted attachment has 2–8 statically registered members. Eight is a
current API/validation bound, not a bandwidth or
protocol limit. Capacity and more sites require measured CPU/FIB/tunnel/session
budgets. Direct gateway peering grows with gateway pairs across attachments;
this implementation has no EVPN route reflector, dynamic peer discovery, or
site-count performance certification.

Losing a controller does not remove healthy gateway forwarding or stop FRR/OVN
failure convergence. Losing all WAN paths or the destination site cannot preserve
cross-site delivery. Recovery can cause loss/reordering and TCP retransmission;
it is not a zero-packet-loss guarantee. A single flow uses one selected gateway
and path. Two 25 Gbit/s NICs do not by themselves prove 50 Gbit/s usable gateway
throughput, and aggregate NIC line rates are not measured tenant capacity.

Readiness indicates local configuration/routing-process readiness. EVPN also
requires the expected native L3VNI, VRF, VTEP and interface mapping to be Up.
Its supervised FRR `mgmtd` applies VRF/VNI configuration through the local
management backend; a successful CLI parse alone is not a readiness receipt.
Remote connectivity requires BGP, data BFD, kernel FIB and traffic observations. Overlay
cleanup checks exact interface ownership and verifies deletion, including after
an uncertain write outcome. Unmarked remnants are preserved for operator review.
No resource is adopted from a generated name alone. Runtime SIGKILL recovery can
rebuild marked resources in the same Pod namespace; the CNI retains responsibility
for Pod deletion and namespace teardown.

A transport change, membership change, module update or accepted configuration
change is not a rolling migration. The immutable resource hash rejects drift.
Drain and finalize the old attachment, then recreate with the new grant; arrange
an outage or separately designed migration. Do not replace ConfigMap content
under running accepted Pods. IPAM reservations remain retained on deletion.

## Evidence and references

Local verification uses `make test`, `make test-race`, and `make check`;
real API integration remains a separate suite. Native dataplane acceptance
requires independent routing, failure, isolation and cleanup tests in the
target deployment.

- [Linux v6.8 Geneve socket and route namespace](https://github.com/torvalds/linux/blob/v6.8/drivers/net/geneve.c)
- [Linux v6.8 VXLAN socket and route namespace](https://github.com/torvalds/linux/blob/v6.8/drivers/net/vxlan/vxlan_core.c)
- [FRR 10.2 EVPN](https://docs.frrouting.org/en/stable-10.2/evpn.html)
- [FRR 10.2 BFD](https://docs.frrouting.org/en/stable-10.2/bfd.html)
- [FRR 10.2.1 local management command ownership](https://github.com/FRRouting/frr/blob/frr-10.2.1/mgmtd/mgmt_vty.c#L639)
- [Geneve protocol and security considerations](https://www.rfc-editor.org/rfc/rfc8926.html)
- [WireGuard network namespaces](https://www.wireguard.com/netns/)
