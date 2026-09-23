# Infra site chart

Install exactly one site operator release per Kubernetes cluster. A fixed Helm-owned
ClusterRole and ClusterRoleBinding prevent a second release, including one in
another namespace, from becoming another writer. Do not bypass Helm ownership
checks. Authority, site, and native integration use distinct guards and may share
a cluster when the rest of the deployment configuration supports it.

Install one release in each Infra cluster. Use the published chart package so
controller, gateway and CLI images are already pinned to prebuilt release digests.
No Go, Python, Docker, or BuildKit installation is required on the operator's
computer. Python inside the prebuilt gateway image is a runtime implementation
detail, not an operator prerequisite.

Prepare the administrator-only release namespace with Pod Security policy that
permits privileged gateway Pods. At least two nodes must match
`local.gatewayNodeSelector`, and their gateway endpoints must be reachable between
sites. The chart deliberately does not label nodes or modify namespace security
policy. Install the pinned native Kube-OVN extension first.

```sh
helm upgrade --install global-vpc ./global-vpc-site-0.1.1.tgz \
  --kube-context infra-a --namespace global-vpc-system \
  --values site-a-values.yaml --wait --timeout 5m

helm status global-vpc --kube-context infra-a -n global-vpc-system
helm test global-vpc --kube-context infra-a -n global-vpc-system --logs
helm history global-vpc --kube-context infra-a -n global-vpc-system
```

The `local` values are the existing local operator configuration. The chart sets
its namespace from the Helm release, the authority kubeconfig path from the
mounted Secret, and gateway image from the release digest. Connected Helm
operations resolve the `kube-system` namespace UID when `local.clusterUID` is
empty. Offline rendering needs the explicit real UID; never reuse a former
cluster's allocation identity. Reserve transit, BFD and gateway control pools outside all
workload pools and underlay networks.

Prefer `authorityAccess.existingSecret`, naming a Secret in the release namespace
with key `kubeconfig`. It must use a location-scoped binding reporter identity,
HTTPS and a trusted CA. The credential must only address this location's authority
binding namespace. Provision and renew credentials through your identity system, then restart the
site release's Deployment after credential rotation. Token lifetime
is finite, and Helm does not automatically renew credentials.

Alternatively set `authorityAccess.kubeconfig` in a private values file. This
creates a chart-owned Secret, but its contents are retained in Helm release values
and revision history. Never pass credentials with `--set`, commit them to source,
or share unredacted Helm values/manifests. Inline credential changes trigger a
controller rollout through a template checksum.

Use standard Helm upgrade/rollback. Keep location identity, cluster UID, reserved
pools, gateway replicas and node identity stable while networks exist; release
rollback changes deployment/configuration, not data or retained allocations.
A pre-upgrade/pre-rollback check rejects local configuration changes while
active bindings remain, except for the gateway image used by the existing controlled
rollout. Draining does not release retained receipts or authorize pool reuse.

Before uninstall, remove workloads and drain all public resources while both
authority and site controllers still run, and stop new intent until removal
finishes. The read-only pre-delete hook refuses incomplete local NetworkBindings
and generated gateway Pods. Fully acknowledged `Deleted` local snapshots remain
as cleanup receipts and do not block removal. The hook does not remove finalizers
or force cleanup. CRDs, namespace and allocation receipts survive
uninstall. Helm does not upgrade CRDs in `crds/`; follow future release migration
instructions when schemas change. Helm test is configuration evidence only;
packet/BFD behavior still requires a workload test.
