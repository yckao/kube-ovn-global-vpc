# Install and operate a two-location VPC

Install the project's **Helm charts**, either directly with Helm or through
`vpcctl`. The commands below use the CLI wrapper; images and native bundles come
from the packaged release. No local Go, Docker, BuildKit, Python or jq is required.
See [CLI installation](vpcctl.md#install-the-cli) and the
[release procedure](releasing.md) for distribution details.

**Availability:** the prebuilt release workflow is implemented, but these changes
do not themselves publish images/charts or establish a live installation result.
Choose an actually published release with packaged chart/image assets. The
version values below are examples, not a claim that those assets already exist.

## Prerequisites

- Management Kubernetes API and two Infra APIs with working baseline Kube-OVN.
  Native extension images currently support matched v1.16.3 or v1.16.4
  distributions on Linux/amd64. The installed distribution digest must match the
  bundle's recorded base image; a similar version tag is insufficient.
- Helm 3.14+ or Helm 4, a prebuilt `vpcctl`, and `kubectl` for the workload smoke
  checks and one-time namespace/node preparation shown below.
- At least two eligible Linux/amd64 gateway nodes per Infra, reachable underlay
  endpoints, suitable MTU and firewall rules for the selected transport.
- Reserved workload, transit, BFD and control ranges. All values below are
  synthetic examples; replace them with actual authorized allocations.
- A project namespace and identity, and a scoped authority credential per site.
  The short-lived evaluation access example below does not provide renewal.

Helm is the installation interface. Connecting it to an external deployment
system is optional and outside this guide. The native chart manages only its
extension image/schema fields; another owner must not independently revert them.

## Prepare values once

Download the release's managed-installation archive, which contains the example
values, or copy the four files from
[config/examples/helm](../config/examples/helm/authority.yaml). Review them before
running any install:

- [authority.yaml](../config/examples/helm/authority.yaml): locations, allowed
  projects, workload pools and the real project RBAC subjects.
- [site-a.yaml](../config/examples/helm/site-a.yaml) and
  [site-b.yaml](../config/examples/helm/site-b.yaml): reserved local pools,
  gateway selectors and the authority binding namespace.
- [native.yaml](../config/examples/helm/native.yaml): the installed upstream
  source target; defaults to v1.16.3. Use separate files if the sites differ.

Published chart defaults supply image digests. Connected Helm obtains each
Infra's `kube-system` UID automatically. Do not copy one cluster's UID into
another. Keep gateway replica count and allocation identity stable while
networks exist.

```sh
VERSION=0.1.0  # Replace with a published prebuilt release.
AUTH_CTX=management
DC_A_CTX=infra-a
DC_B_CTX=infra-b
VALUES_DIR=./config/examples/helm

kubectl --context "$AUTH_CTX" create namespace project-demo
kubectl --context "$DC_A_CTX" create namespace global-vpc-system
kubectl --context "$DC_B_CTX" create namespace global-vpc-system
# Run namespace creation only when absent. The site namespace is admin-only.
kubectl --context "$DC_A_CTX" label namespace global-vpc-system pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl --context "$DC_B_CTX" label namespace global-vpc-system pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl --context "$DC_A_CTX" label nodes NODE_A1 NODE_A2 platform.globalvpc.io/gateway=true
kubectl --context "$DC_B_CTX" label nodes NODE_B1 NODE_B2 platform.globalvpc.io/gateway=true
```

The gateway runtime needs privileged host access; controllers run non-root and
use leader election. Gateway Pods receive no Kubernetes API token. The charts
leave node eligibility and namespace security policy under administrator control.

## Install native integration and the authority

Install one native extension release per Infra before creating managed intent.
The chart applies the matched schema and controller image with guarded hook Jobs;
you do not patch the CRD or rebuild a binary by hand.

```sh
vpcctl --context "$DC_A_CTX" install native-extension --component native \
  --version "$VERSION" -f "$VALUES_DIR/native.yaml"
vpcctl --context "$DC_B_CTX" install native-extension --component native \
  --version "$VERSION" -f "$VALUES_DIR/native.yaml"
vpcctl --context "$DC_A_CTX" verify native-extension
vpcctl --context "$DC_B_CTX" verify native-extension

vpcctl --context "$AUTH_CTX" plan global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority.yaml"
vpcctl --context "$AUTH_CTX" install global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority.yaml"
vpcctl --context "$AUTH_CTX" verify global-vpc
```

`plan` is Helm's server dry-run; hooks run only on the actual operation.
The native chart stores its original-state receipt in a private Kubernetes
Secret before modifying the target. Keep it for retries and eventual removal.
It does not replace the upstream Kube-OVN installation. A source/base mismatch,
stale resource identity or incompatible existing schema stops installation.

## Connect and install each site

Production identity integration supplies a renewable, location-scoped kubeconfig
through `authorityAccess.existingSecret`. For this evaluation, issue one-hour
access files from the authority chart's ServiceAccounts:

```sh
umask 077
ACCESS_DIR=$(mktemp -d)
vpcctl --context "$AUTH_CTX" access issue --binding-namespace global-vpc-dc-a \
  --output "$ACCESS_DIR/authority-a.kubeconfig" --duration 1h
vpcctl --context "$AUTH_CTX" access issue --binding-namespace global-vpc-dc-b \
  --output "$ACCESS_DIR/authority-b.kubeconfig" --duration 1h

vpcctl --context "$DC_A_CTX" install global-vpc --component site \
  --version "$VERSION" -f "$VALUES_DIR/site-a.yaml" \
  --set-file "authorityAccess.kubeconfig=$ACCESS_DIR/authority-a.kubeconfig"
vpcctl --context "$DC_B_CTX" install global-vpc --component site \
  --version "$VERSION" -f "$VALUES_DIR/site-b.yaml" \
  --set-file "authorityAccess.kubeconfig=$ACCESS_DIR/authority-b.kubeconfig"
vpcctl --context "$DC_A_CTX" verify global-vpc
vpcctl --context "$DC_B_CTX" verify global-vpc
```

The actual expiry is reported by the issuer. These identities can read only their
location's bindings and update status. Inline access is stored in Helm release
history, so protect that history and never commit credential files. Before expiry,
issue a new file and run `upgrade` at the same version with a new `--set-file`;
the credential checksum rolls the site Deployment. Existing Secret users must
arrange credential renewal and the corresponding controller restart.

The direct Helm equivalent for a site is:

```sh
helm upgrade --install global-vpc \
  oci://ghcr.io/yckao/kube-ovn-global-vpc/charts/global-vpc-site \
  --kube-context "$DC_A_CTX" --namespace global-vpc-system \
  --version "$VERSION" -f "$VALUES_DIR/site-a.yaml" \
  --set-file "authorityAccess.kubeconfig=$ACCESS_DIR/authority-a.kubeconfig" \
  --wait --timeout 5m
```

## Create and verify

The project identity can now operate public resources without a UI:

```sh
vpcctl --context "$AUTH_CTX" -n project-demo vpc create production
vpcctl --context "$AUTH_CTX" -n project-demo subnet create app-a \
  --vpc production --location dc-a --cidr 10.60.1.0/24
vpcctl --context "$AUTH_CTX" -n project-demo subnet create app-b \
  --vpc production --location dc-b --cidr 10.61.1.0/24
vpcctl --context "$AUTH_CTX" -n project-demo --timeout 5m vpc wait production
vpcctl --context "$AUTH_CTX" -n project-demo subnet list
vpcctl --context "$AUTH_CTX" -n project-demo doctor
```

The example authority values offer `default` (WireGuard/BGP), `trusted-geneve`
(Geneve/BGP), and `trusted-evpn` (VXLAN/EVPN). Pass `--network-class NAME` to VPC
creation to select a configured class. Transport is immutable for an existing
VPC. Geneve and VXLAN/EVPN require a trusted underlay; ToRs do not participate in
the generated tenant gateway BGP sessions.

Attach one smoke workload per location using its public native Subnet mapping:

```sh
for slot in a b; do
  case "$slot" in
    a) SITE_CTX=$DC_A_CTX ;;
    b) SITE_CTX=$DC_B_CTX ;;
  esac
  NATIVE_SUBNET=$(kubectl --context "$AUTH_CTX" -n project-demo get subnet.platform.globalvpc.io "app-$slot" \
    -o jsonpath='{.status.nativeSubnetName}')
  test -n "$NATIVE_SUBNET" || exit 1
  sed "s|REPLACE_WITH_NATIVE_SUBNET_NAME|$NATIVE_SUBNET|g" \
    config/examples/managed/smoke-pod.yaml | kubectl --context "$SITE_CTX" apply -f -
  kubectl --context "$SITE_CTX" -n managed-vpc-smoke wait --for=condition=Ready pod/endpoint --timeout=120s
done
REMOTE_IP=$(kubectl --context "$DC_B_CTX" -n managed-vpc-smoke get pod endpoint -o jsonpath='{.status.podIP}')
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint -- ping -c 5 "$REMOTE_IP"
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint -- wget -qO- "http://$REMOTE_IP:8080/"
```

Repeat in reverse. Deployment Ready establishes process health; VPC Ready and
Helm tests establish observed configuration/ACK state. Only packet tests establish
cross-site forwarding for the tested path. Gateway/BFD and physical failure tests
remain separate qualification. `NativeCapabilityRequired`, `NativeRoutesPending`
and `GatewayPending` distinguish missing capability, unacknowledged native state
and incomplete gateway readiness.

## Upgrade and roll back

Read the target release's compatibility and CRD migration notes. For a compatible
release on the same upstream native baseline:

```sh
NEXT_VERSION=0.1.1  # Select an actually published compatible release.
vpcctl --context "$DC_A_CTX" upgrade native-extension --component native --version "$NEXT_VERSION"
vpcctl --context "$DC_B_CTX" upgrade native-extension --component native --version "$NEXT_VERSION"
vpcctl --context "$DC_A_CTX" upgrade global-vpc --component site --version "$NEXT_VERSION"
vpcctl --context "$DC_B_CTX" upgrade global-vpc --component site --version "$NEXT_VERSION"
vpcctl --context "$AUTH_CTX" upgrade global-vpc --component authority --version "$NEXT_VERSION"
vpcctl --context "$AUTH_CTX" verify global-vpc
vpcctl --context "$DC_A_CTX" verify global-vpc
vpcctl --context "$DC_B_CTX" verify global-vpc
vpcctl --context "$DC_A_CTX" verify native-extension
vpcctl --context "$DC_B_CTX" verify native-extension
```

Each release has its own history. Roll back only to actual compatible successful
revisions; `1` below assumes the initial install is the desired revision:

```sh
vpcctl --context "$AUTH_CTX" history global-vpc
vpcctl --context "$DC_A_CTX" history global-vpc
vpcctl --context "$DC_A_CTX" history native-extension
vpcctl --context "$AUTH_CTX" rollback global-vpc 1
vpcctl --context "$DC_A_CTX" rollback global-vpc 1
vpcctl --context "$DC_B_CTX" rollback global-vpc 1
vpcctl --context "$DC_A_CTX" rollback native-extension 1
vpcctl --context "$DC_B_CTX" rollback native-extension 1
```

Repeat verification and packet checks after rollback. Values and deployment
history are restored; tenant resources, retained allocations and CRD schemas are
not rewound. A site rollback may restore an expired credential; issue fresh
access and upgrade that selected chart version if necessary. Native rollback
between compatible extension images can retain active acknowledged routes;
removing the extension and restoring stock Kube-OVN requires drainage below.

## Drain and remove

Stop new public intent for the removal window. Delete workloads while controllers
remain available, then drain each managed project:

```sh
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke delete pod endpoint
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke delete pod endpoint
vpcctl --context "$AUTH_CTX" --timeout 10m drain --project project-demo
vpcctl --context "$DC_A_CTX" uninstall global-vpc
vpcctl --context "$DC_B_CTX" uninstall global-vpc
vpcctl --context "$AUTH_CTX" uninstall global-vpc
vpcctl --context "$DC_A_CTX" uninstall native-extension
vpcctl --context "$DC_B_CTX" uninstall native-extension
```

Drain deletes Subnets before VPCs, uses UID/version guards and waits for owner
cleanup. Native in-use IPs or unavailable locations can block removal. It never
removes finalizers or attached workloads. Site hooks allow acknowledged terminal
cleanup snapshots, while refusing active bindings and gateway Pods. Native hooks
independently check empty ACKs and absence of owned OVN route/BFD rows before
restoring the stock digest and removing only the extension fields.

If a hook fails, inspect `status`, Helm hook Job logs and controller observations,
correct the cause, then retry the same operation. Do not delete receipt Secrets
or bypass hooks. Uninstall retains CRDs, namespace, allocation receipts and Helm
history; it does not authorize address reuse. Baseline Kube-OVN remains installed.

For maintainers auditing the underlying patch/build/recovery operations, see the
[native implementation reference](kube-ovn-extension-install.md). Those commands
are not required for the normal Helm installation path.
