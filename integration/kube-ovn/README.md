# Native destination routes integration

This source-pinned Kube-OVN extension makes the native VPC controller the sole OVN writer for destination-scoped gateway ECMP and BFD. The Global VPC local operator writes `Vpc.spec.destinationRoutes` and the existing `spec.bfdPort`; it must not patch NB static routes, BFD rows, or ECMP selection fields.

The upstream-oriented patch targets Kube-OVN source commit
`a9296ef2a37c6519bc0ecb798082ce139c72f8eb` (v1.16.4). The
[v1.16.3 compatibility target](compat-v1.16.3/README.md) applies the same contract
to source commit `98af25ffae49193a8dc16bbc39bd8ca4110ec367` with its pinned
dependencies. These are independent project extensions, not official Kube-OVN
releases. A schema and source artifact do not establish running compatibility;
validate the controller build, acknowledgement and actual tenant path separately.

## Native API contract

See [example-vpc.yaml](example-vpc.yaml). Each destination route specifies a canonical unicast IPv4 `cidr`, 2–64 directly connected local gateway `nextHops`, required BFD timers (`minRX`, `minTX`, `multiplier`), and explicit ECMP `selectionFields`. The initial public product accepts subnet prefixes /8–/30; this native validator supports /1–/31 and rejects default and host routes. It rejects overlapping destinations, repeated next hops, destination-overlapping gateways, invalid fields/timers, and inconsistent BFD timers for shared next hops. The 64-next-hop and 256-prefix limits bound configuration size; they are not measured hardware scale limits.

The existing native `bfdPort` creates a dedicated BFD-only LRP with HA chassis placement. Gateway next hops must be reachable through ordinary connected VPC router ports. The extension rejects default-VPC use, connected-network overlap, foreign BFD tuples, and foreign destination/static-route collisions at the requested prefix or more-specific prefixes. Existing default/covering static routes remain independent. Existing routing policies and source-policy static routes retain their normal precedence; the destination discard guarantee assumes neither overrides these destinations. Applications must not install such overriding routes or policy reroutes.

Native reconciliation writes this status after the NB transaction succeeds:

```yaml
status:
  destinationRoutes:
    capability: destination-routes.v1
    observedGeneration: 7
    appliedHash: <64 lowercase hex characters>
    ready: true
```

A consumer must require all four fields and the desired hash. A matching schema alone is insufficient: an old controller may accept/prune a field without implementing it. `ready` means the desired NB configuration was acknowledged; it does not mean BFD sessions are up, tunnel transport is healthy, or tenant packets have passed. Active route intent is rechecked every 30 seconds. OVN owns runtime BFD liveness and ECMP selection without a Go controller health-event loop.

The shared [Compile helper](destinationroute/plan.go) hashes SHA256 of JSON for a normalized non-null route array. Routes sort lexically by CIDR; next hops and selection fields sort lexically. Object field order is `cidr,nextHops,bfd,selectionFields`; BFD field order is `minRX,minTX,multiplier`. The empty route array hashes to `4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945`.

## Forwarding and ownership

OVN removes BFD-down next hops from route calculation. An ordinary ECMP route would therefore fall through to an unrelated default/NAT path when every gateway is down. This extension expands each desired prefix into two child-prefix ECMP groups and one original-prefix `discard` route. For `10.70.0.0/24`, the two healthy groups cover `10.70.0.0/25` and `10.70.0.128/25`; longest-prefix matching prefers them. When all sessions are down, the `/24` discard remains. The guard covers only the requested destination, and does not change NAT or default routes. A same-prefix discard cannot be mixed with the ECMP group: OVN drops the entire group even when healthy paths exist.

Each destination with N gateways consumes `2*N + 1` NB static-route rows. BFD sessions are shared per native BFD-port/next-hop tuple when timers agree. Stable reconciliation preserves row identities. BFD UUID replacement is repaired in the same transaction that changes route references.

All extension rows carry `kube-ovn.io/destination-vpc-uid`; routes also carry `kube-ovn.io/destination-route`. The ordinary native static-route diff excludes these rows, and the legacy native BFD chassis-priority handler excludes extension sessions. Route/BFD changes commit together with snapshot and ownership wait guards. Exact UID removal refuses BFD deletion if a foreign route or policy still references it. VPC deletion and native GC recover root BFD cleanup after crashes. Empty intent withdraws owned routes and sessions before the BFD LRP may be disabled; it does not remove an unrelated route, NAT entry, or shared session.

## Apply and test

Obtain an unmodified checkout at the exact commit from [source-lock.json](source-lock.json), then run:

```sh
python3 integration/kube-ovn/scripts/apply.py /path/to/kube-ovn --check
python3 integration/kube-ovn/scripts/apply.py /path/to/kube-ovn
integration/kube-ovn/scripts/test.sh /path/to/kube-ovn
```

`apply.py` checks the commit when Git metadata is available, verifies SHA256 for every affected original file, and runs `git apply --check` before mutation. `native.patch` is the complete applied artifact; `overlay/` keeps added native files easy to review. Go 1.27.1 is required by the pinned upstream source. `test.sh` runs the planner, native in-memory OVSDB tests, controller validation test, and controller package build on Linux in a disposable source copy. It does not install CRDs or deploy a controller.

On Darwin, upstream NDP/netfilter and broad test-suite files prevent direct native package tests. The optional `scripts/test-portable.py` runs the same new OVSDB transaction tests with original upstream NB fixtures in a disposable source copy. It substitutes the identical Ethernet IPv6 protocol constant for an unavailable Darwin constant; it excludes unrelated Linux-only tests. This is supplementary transaction validation, not Linux/controller/dataplane validation. Both test wrappers correct three bugs only in a disposable copy of the pinned in-memory libovsdb test server: unordered-set comparison, explicit empty-value comparison, and `_uuid` decoding in wait operations. These corrections retain the strong production CAS guard and do not alter the libovsdb client or the production OVN server. Direct `go test` against the uncorrected test-server dependency can spuriously fail those guards. Compile the pinned Linux target separately from any portable test run.

The route tests cover atomic creation, idempotency, BFD UUID replacement, default-route preservation, empty withdrawal, foreign tuple rejection, generated child-prefix conflict, replacement UID refusal, and foreign consumer cleanup protection. Planner/controller validation covers canonical hashes, overlap/default/host rejection, shared-timer conflicts, directly connected gateway checks, API deep copy, and static-route conflict preflight. The current patch also tests the BFD absence guard on its serialized wire representation and refuses stale absence snapshots. Native transaction tests and the real northd gate below precede any controlled rollout; tenant dataplane qualification remains separate.

## Real OVSDB and northd qualification

The in-memory tests do not replace a real OVSDB/OVN gate. Use isolated NB/SB
databases and the production adapter/dependency build to verify atomic route
creation and removal, both child-prefix ECMP groups, every combination of BFD
availability, and preservation of unrelated defaults/NAT.

The BFD absence guard uses the unique `(logical_port,dst_ip)` tuple with a
nonempty `until !=` comparison. The pinned encoder can omit empty `wait.rows`,
so the serialized wire representation is tested explicitly. Existing rows guard
both UUID and owner; stale absence snapshots must fail atomically.

Logical traces with injected BFD status validate route calculation, not physical
BFD exchange. A subsequent isolated Kubernetes test must exercise the running
controller from native API intent through generation/hash acknowledgement to
actual endpoint packets, including failure and guarded withdrawal.

## Deployment and rollback requirements

Build a reproducible native-controller image from the exact pinned source and
review schema additions alongside it. Preserve the deployment's existing
security context, probes, leadership, dependencies and required executable
capabilities. Complete the replica replacement before enabling managed intent:
an old replica must not regain leadership while extension routes are active.

Drain managed destination intent through its owning controller before restoring
a stock native controller. Require empty-intent acknowledgements and absence of
owned route/BFD state. Do not remove schema fields while active intent remains.
Verify local networking, native readiness, allocation consistency and actual
packets after rollback. Image/schema rollback without resource drainage is not
a safe substitute.

## Source evidence

- [Pinned Kube-OVN VPC API](https://github.com/kubeovn/kube-ovn/blob/a9296ef2a37c6519bc0ecb798082ce139c72f8eb/pkg/apis/kubeovn/v1/vpc.go) and [native VPC reconciliation](https://github.com/kubeovn/kube-ovn/blob/a9296ef2a37c6519bc0ecb798082ce139c72f8eb/pkg/controller/vpc.go).
- [Native VpcEgressGateway reconciliation](https://github.com/kubeovn/kube-ovn/blob/a9296ef2a37c6519bc0ecb798082ce139c72f8eb/pkg/controller/vpc_egress_gateway.go) uses source-address selection, creates its own gateway workload, and requires an external subnet. Those semantics do not implement this destination-scoped route API for existing local gateways.
- [OVN route parsing and BFD filtering](https://github.com/ovn-org/ovn/blob/caa2aa1832b7/northd/northd.c) (`parsed_routes_add_static`) excludes down sessions; [ECMP grouping](https://github.com/ovn-org/ovn/blob/caa2aa1832b7/northd/en-group-ecmp-route.c) handles a same-prefix discard by dropping the group. The fail-closed expansion is derived from this source behavior and must be qualified with real logical traces. Tenant packet delivery and BFD exchange remain separate gates.
