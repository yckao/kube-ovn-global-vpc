# Native Kube-OVN extension chart

This integration chart is installed once per Infra cluster after the supported
upstream Kube-OVN installation. It deliberately does not adopt, replace, or delete
the upstream chart. Packaged releases contain qualified native bundles and a
digest-pinned prebuilt hook image; administrator machines need only Helm.
The chart's fixed ClusterRole name guards the shared cluster-wide native CRD,
so Helm refuses a second independent extension release in the same cluster.
Do not bypass Helm ownership checks or retarget an existing extension release.

Helm post-install, post-upgrade, and post-rollback Jobs reconcile the revision's
native bundle through the Kubernetes API. They guard cluster and resource UIDs,
schema and Pod-template snapshots, immutable images, and current native ACKs.
Only the existing controller container image and the two `destinationRoutes`
schema properties are managed. Installing this chart authorizes those narrow
changes; the upstream installation must not independently revert them.

The private original-state receipt is stored in a Secret in the release namespace
before the first mutation. Failed hooks keep it and can be retried with Helm.
Keep that namespace and Secret until the extension has been uninstalled. Do not
delete the receipt to repair an error. No receipts or build tools are needed on
the administrator's machine.

Compatible native upgrades retain the same upstream baseline and extension
contract. A full upstream Kube-OVN version upgrade belongs to the upstream chart;
this chart refuses to approximate it by replacing only one controller image.

Before `helm uninstall`, delete tenant resources through the public API and keep
their cleanup controllers running. Stop materializers only after tenant cleanup.
The pre-delete Job then requires empty native intent, current empty ACKs, and
independent absence of UID-owned OVN static-route and BFD rows. It restores the
recorded immutable stock image, waits for the complete rollout, and removes only
the two extension schema properties. A failed check leaves the receipt and native
state available for retry. Finalizers are never removed. Do not bypass hooks.

`helm test` verifies schema, rollout, and current native ACKs. It does not prove
tenant packet delivery or baseline network health. Source checkouts have no
published image digests or embedded bundles and fail closed when rendered without
maintainer test fixtures; use a packaged release chart for installation.
