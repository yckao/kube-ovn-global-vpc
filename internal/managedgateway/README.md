# Managed local gateways

`Reconciler.Ensure` takes an accepted local `NetworkBinding` and the local native
transit/BFD allocation. `Result.Gateways` publishes public discovery receipts even
before peer links can be built. `Result.Ready` requires the accepted runtime
configuration on every member plus local OVN BFD health. It is not an end-to-end
connectivity or hardware throughput claim.

The namespace-local allocation ConfigMap uses Kubernetes resource-version CAS.
Address and per-node WireGuard port receipts remain reserved after teardown.
An immutable identity ConfigMap pins that registry's UID. Missing, replaced or
corrupt registry state requires fenced recovery; a new binding cannot bootstrap
around retained member state. Before changing runtime resources, reconciliation
validates all retained address/port claims against member ledgers and published
status, including existing links that need no new allocation. Committed writes
whose responses were lost are recovered without reassigning their claims.
The binding-local ledger stores chosen node names and UIDs, member identities,
transit addresses, health addresses, public key identities, and peer link claims.
WireGuard private keys are generated locally and stay in immutable Secrets.
Local ASN, native tenant VNI and native per-pair control VNIs come from retained
platform authority allocations. Native ports are Geneve 6082, VXLAN control 4788
and VXLAN data 4789; WireGuard ports come from the administrator-reserved range.

Runtime ConfigMaps are immutable and named by configuration, image, and source
hash. Existing Pods continue reading their own accepted generation while a new
snapshot is prepared. At most one member is deleted for a generation update.
All surviving siblings must be locally Ready and pass a native rollout probe
against their installed configuration and their own newly rendered desired
configuration. The controller supplies desired JSON on bounded stdin to an
embedded read-only adapter that works with the existing v1 runtime. It filters a
memory copy to retain old peers and prefixes that are still desired, then calls
the installed `managed.rollout_ready`. Explicitly withdrawn peers or prefixes do
not pin an old generation after their source stops advertising. Retained peers
still require direct BGP/BFD, and retained prefixes require their direct FIB next
hops; local OVN BFD remains mandatory even when every peer is withdrawn. New
delegations become mandatory when an updated replacement acts as a survivor.
Gateway, transport and delegation-owner identity mismatches fail closed. This
probe does not accept sibling backup routes as evidence of a direct peer path.
A remote outage may therefore block configuration updates while the accepted
local dataplane continues running. Pod Ready itself depends only on local OVN BFD.

`Delete` follows local native-route withdrawal. Pod deletion uses UID and resource
version preconditions, waits for actual API absence, and then removes runtime
ConfigMaps and Secrets. It never force-deletes a Pod on an unreachable host and
never reuses an old member for a different node UID. Node replacement, replica
count changes, lost ledger recovery and lost published keys require an explicit
fenced administrative recovery workflow; automatic hardware replacement is not
implemented. Retained allocation records are not garbage-collected automatically.

`gateway/managed.py` validates the multi-subnet runtime contract and adds native
socket/VNI preflight. Native interfaces and decrypted routes remain inside the
Pod network namespace; the host supplies the existing UDP routing namespace.
Preflight inspects current namespaces read-only, rejects collect-metadata or
unknown/user-space socket ownership, and permits shared fixed native sockets only
with disjoint VNIs. Native kernel create remains the final arbiter of concurrent
socket/VNI creation. No host route, ToR, CNI, or OVN database writer is introduced.

The unit and fake Kubernetes tests cover allocation races, public discovery,
immutable keys, incarnation fencing, all three rendered profiles, multi-subnet
advertisement, gradual updates, native health gates, and guarded cleanup. These
checks do not substitute for fresh native integration and live packet validation.
