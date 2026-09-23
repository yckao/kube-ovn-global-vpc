# Management authority chart

Install exactly one authority release per Kubernetes cluster. A fixed Helm-owned
ClusterRole and ClusterRoleBinding prevent a second release, including one in
another namespace, from becoming another writer. Do not bypass Helm ownership
checks. Authority, site, and native integration use distinct guards and may share
a cluster when the rest of the deployment configuration supports it.

Install this chart in the management cluster. Install `global-vpc-site` once in
each separate Infra cluster, after the native Kube-OVN extension is ready. These
are independent Helm releases; a Helm release never spans Kubernetes clusters.

Published chart packages contain prebuilt controller and CLI images pinned by
digest. A chart from a source checkout requires `image` and `cliImage` repository
and digest values. Operators do not compile binaries or build images.

```sh
helm upgrade --install global-vpc ./global-vpc-0.1.0.tgz \
  --kube-context management --namespace global-vpc-system --create-namespace \
  --values authority-values.yaml --wait --timeout 5m

helm status global-vpc --kube-context management -n global-vpc-system
helm test global-vpc --kube-context management -n global-vpc-system --logs
helm history global-vpc --kube-context management -n global-vpc-system
```

`platform` is the existing administrator-owned platform configuration. The
registry namespace follows the Helm release namespace. `projects` optionally
binds the public editor role to existing Kubernetes identities and project
namespaces. Location reporter service accounts can only read NetworkBindings and
update their status in their own binding namespace.

`locationAccess.createNamespaces` creates location binding namespaces and retains
them on uninstall. Set it to `false` when these namespaces are managed separately.
The chart does not create or take ownership of the Helm release namespace, project
namespaces, workloads, user identities, or Pod Security policy.

Upgrade or roll back with normal Helm commands. Keep allocation identities,
location identities, reserved pools and gateway topology stable while public
resources exist. A pre-upgrade/pre-rollback check blocks configuration changes
while managed resources remain; image changes are permitted. Draining does not
release retained allocation receipts or authorize reusing their address pools.
Image rollback does not undo VPC/Subnet changes or allocations.
Helm does not upgrade CRDs in `crds/`; schema changes require the release's migration
procedure before controller upgrades. This release uses the unchanged v1alpha2
API, and tests require bundled CRDs to match the source definitions.

Before uninstall, remove workloads and drain public Subnets/VPCs while all
controllers still run, and stop new intent until the removal finishes. A pre-delete hook refuses remaining public resources or
NetworkBindings. It has read-only access and never strips finalizers or deletes
tenant networks. `lifecycle.preDeleteCheck=false` disables this guard and should
only be used in a separately verified recovery. CRDs, binding namespaces and
controller-created allocation receipts remain after uninstall. Helm test checks
controller/API observations; it does not claim packet delivery.

The release namespace and location binding namespaces are administrator-only.
Never grant project identities write access to controller configuration, internal
NetworkBindings, or these service accounts.
