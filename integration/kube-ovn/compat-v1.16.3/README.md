# Native extension for the pinned v1.16.3 source

This compatibility patch targets the pinned build source,
`98af25ffae49193a8dc16bbc39bd8ca4110ec367`. It applies the same additive
`destinationRoutes` contract as the parent integration. It is a custom candidate,
not an official Kube-OVN release. Runtime qualification and a controlled rollout
are required before enabling managed bindings.

Follow the [native extension installation guide](../../../docs/kube-ovn-extension-install.md)
for the complete v1.16.3 fetch/build/test/image/schema/rollout procedure and
rollback commands. Select its compatibility branch; the parent v1.16.4 test
wrapper expects a different dependency version.

The source archive SHA256 is
`8d8195b60e39ea3291bc8f8ba3828c9b7f756a1ca863e8d10d8332adfdf0f676`.
`native.patch` applies without changes to this source. `source-lock.json` verifies
both affected baseline files and the dependency/client/IPAM files that preserve
the source target's recovery behavior.

## Preserve the pinned source behavior

The pinned source uses Go 1.26.6 and replaces libovsdb with
`github.com/kubeovn/libovsdb@v0.0.0-20251212071713-cb1c2bc5d43e`. Its normal NB
client creates one monitor; this patch creates no client or additional monitor.
It does not introduce the v1.16.4 ACL sampling cleanup path. It does not modify
Pod inspection, IPAM initialization, controller leadership, or health probes.
The existing omission of ACL-sampling command arguments must remain in place.

The pinned source already implements `Vpc.spec.bfdPort`, its native BFD-only
LRP and HA chassis group. No backport of these mechanisms is needed. This patch
adds only destination-scoped route/BFD ownership and acknowledgement to those
native mechanisms. The managed local operator continues to write Kube-OVN API
intent; the native Kube-OVN controller remains the OVN writer.

## Build and qualify

Use an unmodified archive for the exact commit and the matching Go toolchain:

```sh
python3 integration/kube-ovn/compat-v1.16.3/scripts/apply.py /path/to/unmodified-source --check
python3 integration/kube-ovn/compat-v1.16.3/scripts/build.py /path/to/unmodified-source \
  --go /path/to/go1.26.6/bin/go --output /tmp/new-compat-build
```

The builder makes independent production and test copies. The production
executable retains the original `go.mod`/`go.sum` and libovsdb implementation,
uses CGO=0, Linux/amd64 and the upstream PIE build mode, and identifies itself as
`v1.16.3-globalvpc.v1`. It does not contain test-server modifications.

The separate test dependency fixes the same three in-memory wait-operation
bugs documented by the parent integration. The upstream socket fixture is
changed only in the test copy to honor `TMPDIR`. All upstream test files remain
compiled; there is no Darwin portability change. Run the generated binaries in
an isolated temporary directory on Linux, with these exact patterns:

- `ovs.test`: `^TestNativeDestinationRoutes$`
- `controller.test`: `^TestDestinationRoute(Validation|OrphanGuardGC)$`
- `destinationroute.test`: all planner tests

The builder records hashes, paths, toolchain, original dependency graph and
separate test adjustments in its `build-evidence.json`. Compilation and these
unit tests do not prove northd forwarding, BFD failover, or running-controller
convergence. Real isolated OVN DB/northd tests must verify both child-prefix
ECMP groups, every combination of BFD availability, the parent discard when
all gateways are down, and preservation of unrelated defaults/NAT. The actual
managed path must then prove Kubernetes intent through native acknowledgement
to tenant packets. Run the existing reconnect/reset, blackhole, and leader
restart qualifications against the candidate while auditing existing Pod IPs
and allocation annotations.

## Additive CRD change

Verify that the installed `vpcs.kubeovn.io` schema supports `bfdPort`. Add the
`spec.destinationRoutes` and `status.destinationRoutes` property schemas from
this patch to the currently served VPC version. Preserve every existing field,
version, conversion setting, annotation and other CRD. Do not apply an older
complete chart/CRD bundle over the current installation. Use a resourceVersion
precondition and verify that unrelated schema fields are unchanged.

`status.destinationRoutes` includes the capability string, observed generation,
applied hash and readiness. This is configuration acknowledgement. Runtime BFD
health and tenant packet delivery remain separate validation results. The
existing destination guard assumes no administrator-installed source-policy
static route or logical-router reroute policy overrides it.

## Deployment and rollback

Package the replacement binary into a reproducible OCI image based on the
matching native-controller distribution. Preserve required executable file
capabilities and the existing Deployment security context, probes, environment,
arguments and leader-election identity. Do not start a second OVN writer.
Complete replacement of controller replicas before enabling managed intent.

Withdraw managed bindings through their owners while the patched native
controller remains active. Require current empty-intent acknowledgements and
absence of UID-owned destination routes and BFD rows before restoring stock
controller behavior. Stop materializers from replaying withdrawn intent.

Never remove schema fields while active destination-route intent remains.
Verify original image behavior, replica availability, native liveness,
allocation consistency and endpoint communication after rollback. Production
qualification must include the target environment's existing recovery tests.
