# Local gateway runtime

The v1 contract keeps one WireGuard interface and the original single-gateway
behavior. The v2 contract runs two to eight independently scheduled gateway
Pods, with one local ConfigMap per member. Members must use distinct nodes and
the same local ASN. No runtime calls a Kubernetes API or another controller.

The default `wireguard-bgp` profile additionally requires an externally supplied
private-key Secret reference per member. The `geneve-bgp` and `vxlan-evpn`
profiles use native Linux overlays without key Secrets. Their registration,
trusted-underlay requirement, FRR configuration, source filtering, ownership
and cleanup are documented in [transport profiles](../docs/transport-profiles.md).
Deploy `gateway.py`, `overlay.py` and `evpn.py` together; the adapter packages
the modules needed by the selected profile into its immutable ConfigMap.

The following WireGuard-specific details describe the default profile. Local
OVN health gating, sibling fallback, controller independence and the stateless
routing limitation also apply to the native profiles.

WireGuard interfaces receive an ownership alias after successful creation,
again in the namespace-move request, and during rename; each stage reads the
alias back. These are separate netlink operations. A crash or partial failure
before marking can leave an unmarked interface. The runtime preserves that
unknown remnant and fails closed; an operator must verify its origin and perform
controlled cleanup before retrying. Existing unmarked interfaces are never
adopted or deleted by name alone.

Each v2 WireGuard transport link owns a separate interface, UDP listen port,
and pair of control addresses. Repeated remote delegated prefixes therefore do
not compete in one WireGuard AllowedIPs table. Repeated pools are accepted only
when their remote site, attachment, ASN, and complete delegation match. FRR uses
eBGP ECMP for direct remote links; the Linux hash includes transport ports, so
multiple flows can use multiple paths. One flow remains on one selected path.
Every local member must cover the same remote delegated owners and pools, though
the number of links may differ. This keeps sibling fallback filters consistent.

Sibling sessions are derived from the local member registration. They use iBGP
with next-hop-self and exchange only remote delegated prefixes. A member whose
WAN links fail can forward through a local sibling. WAN exports remain limited
to the exact local workload CIDR; remote or sibling routes are never exported to
another remote site. Normal iBGP split horizon remains enabled, and graceful
restart helper behavior is disabled for these sessions.

The local workload prefix is initially absent from BGP. The runtime reads the
exact local OVN BFD session from its own FRR daemon and advertises the prefix
only while that session is Up. Down, Init, or Shutdown withdraws the prefix.
The local workload static route and the BFD source host route remain installed
so local reachability can recover. The polling interval is minRX, bounded to
100–1000 ms; each daemon read or configuration call has a two-second deadline.
Malformed or ambiguous health evidence and failed configuration changes stop
the routing daemons and remove the owned interfaces, preventing stale exports.

Kube-OVN's patched `bfd-only` implementation can deliver single-hop BFD packets
with TTL 254, while FRR requires TTL 255. A named and ownership-checked nftables
table in the **gateway Pod network namespace** changes only packets arriving on
eth0 from the registered BFD source to that member's transit IP, UDP port 3784,
and TTL 254. The rule counts matching packets and changes the TTL to 255. It
does not change host nftables configuration. Runtime cleanup removes only a
table with the expected ownership comment. This is a compatibility mechanism
for the pinned Kube-OVN implementation, not a general relaxation of BFD checks.

Pod readiness reports that local processes and configuration are installed.
It does not prove remote routes or end-to-end reachability. The adapter returns
sorted `readyGatewayIDs` with consistent `readyGateways` and `desiredGateways`
counts; the controller intersects those IDs with its local OVN BFD Up set to
report available redundancy. Existing dataplane forwarding and local BFD/BGP
convergence do not require the controller process to remain available.

Run `python3 /app/gateway.py --stats` inside a gateway Pod for read-only BFD,
BGP route, interface byte, WireGuard transfer, and compatibility-rule counters.
Native profiles expose their tunnel, kernel route and source-policy counters;
EVPN also exposes its Type-5 RIB, tenant VRF and data BFD state.
The collector does not use WireGuard commands that reveal private keys. Tests
must measure actual flows and failure convergence in addition to these counters.
The current dataplane is stateless routing; it does not replicate NAT, load
balancer, or firewall connection state between gateways.

The configuration shape is illustrated in
[`site-gateway.json`](../config/samples/site-gateway.json). Its namespace/cluster
UIDs, node names, image, and public-key placeholders require local registration.
