# Start with DC-A, then extend the VPC to DC-B

First install one working location and test its local VPC network. Then register
DC-B and add a second Subnet to the **same VPC**. DC-A remains installed throughout.
The installation interface is the project's Helm charts, used directly or through
`vpcctl`; release packages supply the images and native bundles. No local Go,
Docker, BuildKit, Python or jq is required.

Use [CLI installation](vpcctl.md#install-the-cli) to obtain `vpcctl`. The commands
below use the published
[v0.1.1 images and charts](https://github.com/yckao/kube-ovn-global-vpc/releases/tag/v0.1.1).
Publishing those artifacts does not by itself establish a live installation or
a packet-forwarding result.

## What is needed for the first location

- A management Kubernetes API and **DC-A's Infra API** with working baseline
  Kube-OVN. DC-B is not needed until the expansion stage.
- A matched v1.16.3 or v1.16.4 Kube-OVN distribution on Linux/amd64. Its installed
  distribution digest must match the native bundle's base image; a similar
  version tag is insufficient. Baseline Pod networking must already work.
- Helm 3.14+ or Helm 4, the prebuilt CLI, and `kubectl` for workload smoke tests
  and one-time namespace/node preparation.
- At least two eligible Linux/amd64 gateway nodes in DC-A, suitable MTU/firewall
  rules, and reserved workload, transit, BFD and control ranges.
- A project namespace and identity. Each site gets its own scoped authority
  credential. The one-hour evaluation credential below needs renewal.

The examples use synthetic allocations; replace them with authorized values.
Helm is the installation interface. A native chart release manages only the
extension image/schema fields and keeps the upstream Kube-OVN installation.

## Prepare the DC-A values

Download the release's installation inputs into a clean working directory.
This example verifies the archive against
that release's checksums before extraction:

```sh
VERSION=0.1.1
BASE="https://github.com/yckao/kube-ovn-global-vpc/releases/download/v${VERSION}"
ASSET="managed-installation_${VERSION}.tar.gz"
mkdir -p global-vpc-installation
cd global-vpc-installation || exit 1
curl -fLO "$BASE/$ASSET" || exit 1
curl -fLO "$BASE/SHA256SUMS" || exit 1
awk -v name="$ASSET" '$2 == name { print }' SHA256SUMS > INSTALL-SHA256SUMS
test "$(wc -l < INSTALL-SHA256SUMS | tr -d ' ')" = 1 || exit 1
shasum -a 256 -c INSTALL-SHA256SUMS || exit 1
tar -xzf "$ASSET" || exit 1
test -f config/examples/helm/authority-dc-a.yaml || exit 1
test -f config/examples/managed/smoke-pod.yaml || exit 1
```

Linux can use `sha256sum -c INSTALL-SHA256SUMS` for the checksum step. The archive
extracts `config/` directly, with no enclosing version directory. Run the remaining
commands from this working directory. The files used by this guide are:

| File | When to use it |
|---|---|
| [authority-dc-a.yaml](../config/examples/helm/authority-dc-a.yaml) | Initial authority with only DC-A registered |
| [site-a.yaml](../config/examples/helm/site-a.yaml) | DC-A's local pools, gateway selector and binding namespace |
| [native.yaml](../config/examples/helm/native.yaml) | Native source target, default v1.16.3; use a site-specific copy when needed |
| [authority.yaml](../config/examples/helm/authority.yaml) | Later expansion, retaining DC-A and appending DC-B |
| [site-b.yaml](../config/examples/helm/site-b.yaml) | DC-B's local settings, needed only in the expansion stage |

Review the DC-A files and replace the example project RBAC group before
installation. Published charts supply image digests. Connected Helm obtains the
local Infra's `kube-system` UID; never copy another cluster's UID into the values.

```sh
VERSION=0.1.1
AUTH_CTX=management
DC_A_CTX=infra-a
VALUES_DIR=./config/examples/helm

kubectl --context "$AUTH_CTX" create namespace project-demo
kubectl --context "$DC_A_CTX" create namespace global-vpc-system
# Create namespaces only when absent. The site namespace is admin-only.
kubectl --context "$DC_A_CTX" label namespace global-vpc-system \
  pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl --context "$DC_A_CTX" label nodes NODE_A1 NODE_A2 \
  platform.globalvpc.io/gateway=true
```

Gateway Pods need privileged host access and receive no Kubernetes API token.
Controllers run non-root with leader election. Node eligibility and namespace
security policy remain administrator choices.

## Install the authority and DC-A

Install the authority using the **DC-A-only** file:

```sh
vpcctl --context "$AUTH_CTX" plan global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority-dc-a.yaml"
vpcctl --context "$AUTH_CTX" install global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority-dc-a.yaml"
vpcctl --context "$AUTH_CTX" verify global-vpc
```

`plan` is Helm's server dry-run; hooks run on the actual operation. Next install
the native extension in DC-A. No source patching or binary replacement commands
are needed on the administrator's computer:

```sh
vpcctl --context "$DC_A_CTX" install native-extension --component native \
  --version "$VERSION" -f "$VALUES_DIR/native.yaml"
vpcctl --context "$DC_A_CTX" verify native-extension
```

The native hook records the original state in a private Kubernetes Secret before
mutation. Keep that Secret and namespace for retries and removal. Source/base
mismatches, stale identities and incompatible existing schemas stop installation.

Issue evaluation access for DC-A, then install its site operator:

```sh
umask 077
ACCESS_DIR=$(mktemp -d)
vpcctl --context "$AUTH_CTX" access issue --binding-namespace global-vpc-dc-a \
  --output "$ACCESS_DIR/authority-a.kubeconfig" --duration 1h
vpcctl --context "$DC_A_CTX" install global-vpc --component site \
  --version "$VERSION" -f "$VALUES_DIR/site-a.yaml" \
  --set-file "authorityAccess.kubeconfig=$ACCESS_DIR/authority-a.kubeconfig"
vpcctl --context "$DC_A_CTX" verify global-vpc
```

For production identity integration, provision renewable, location-scoped access
through `authorityAccess.existingSecret`. The evaluation token can read only its
location's bindings and update status; its actual expiry is reported by the
issuer. Inline access is stored in Helm history. Protect that history and never
commit credential files. See [scoped access and renewal](vpcctl.md#scoped-location-access).

## Create and test the VPC in DC-A

A project user now creates one VPC and its first Subnet:

```sh
vpcctl --context "$AUTH_CTX" -n project-demo vpc create production
vpcctl --context "$AUTH_CTX" -n project-demo subnet create app-a \
  --vpc production --location dc-a --cidr 10.60.1.0/24
vpcctl --context "$AUTH_CTX" -n project-demo --timeout 5m subnet wait app-a
vpcctl --context "$AUTH_CTX" -n project-demo --timeout 5m vpc wait production
vpcctl --context "$AUTH_CTX" -n project-demo subnet list --vpc production
vpcctl --context "$AUTH_CTX" -n project-demo doctor
```

DC-B is neither registered nor required at this point. The example network class
`default` uses WireGuard/BGP; `trusted-geneve` and `trusted-evpn` are also configured.
Select a class with `vpc create --network-class NAME`. A VPC's transport is
immutable. Geneve and VXLAN/EVPN need a trusted underlay; ToRs do not participate
in tenant gateway BGP sessions.

For a local packet check, attach **two different Pods** to DC-A's public Subnet
mapping using [smoke-pod.yaml](../config/examples/managed/smoke-pod.yaml):

```sh
NATIVE_SUBNET_A=$(kubectl --context "$AUTH_CTX" -n project-demo \
  get subnet.platform.globalvpc.io app-a -o jsonpath='{.status.nativeSubnetName}')
test -n "$NATIVE_SUBNET_A" || exit 1
for POD_NAME in endpoint-a1 endpoint-a2; do
  sed -e "s|REPLACE_WITH_NATIVE_SUBNET_NAME|$NATIVE_SUBNET_A|g" \
      -e "s|name: endpoint$|name: $POD_NAME|g" \
    config/examples/managed/smoke-pod.yaml | kubectl --context "$DC_A_CTX" apply -f -
  kubectl --context "$DC_A_CTX" -n managed-vpc-smoke \
    wait --for=condition=Ready "pod/$POD_NAME" --timeout=120s
done
DC_A_PEER_IP=$(kubectl --context "$DC_A_CTX" -n managed-vpc-smoke \
  get pod endpoint-a2 -o jsonpath='{.status.podIP}')
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint-a1 -- ping -c 5 "$DC_A_PEER_IP"
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint-a1 -- wget -qO- "http://$DC_A_PEER_IP:8080/"
```

The HTTP response should be `managed-vpc`. This checks the local path between the
selected Pods; it does not prove cross-location forwarding or gateway failover.
You can stop here with a single-location VPC. Keep these Pods for the expansion
check; do not recreate `production` or `app-a` to add DC-B.

## Register DC-B without changing DC-A

Once DC-A is working, prepare DC-B with the same baseline networking and gateway
prerequisites. Review `site-b.yaml` and a native values copy if its upstream target
differs from DC-A. In the expanded `authority.yaml`, preserve **every existing
DC-A location field, all network classes, registry namespace and project grants**;
append only DC-B's location. Carry your real edits from `authority-dc-a.yaml` into
this file before using it. The supplied examples differ only by the new location.

```sh
DC_B_CTX=infra-b
kubectl --context "$DC_B_CTX" create namespace global-vpc-system
# Create the namespace only when absent.
kubectl --context "$DC_B_CTX" label namespace global-vpc-system \
  pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl --context "$DC_B_CTX" label nodes NODE_B1 NODE_B2 \
  platform.globalvpc.io/gateway=true

vpcctl --context "$AUTH_CTX" plan global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority.yaml"
vpcctl --context "$AUTH_CTX" upgrade global-vpc --component authority \
  --version "$VERSION" -f "$VALUES_DIR/authority.yaml"
vpcctl --context "$AUTH_CTX" verify global-vpc
vpcctl --context "$AUTH_CTX" -n project-demo doctor
```

This is a **configuration expansion at the same release version**. The upgrade
hook allows new locations while existing resources remain, provided the existing
allocation configuration is unchanged. Changing or removing an existing location,
its pools/grants, or the network classes is not an additive expansion. Registering
DC-B alone does not add it to `production`; VPC membership follows its Subnets.

## Install DC-B, then extend the existing VPC

Install its native extension and issue a separate credential for the newly
registered binding namespace:

```sh
vpcctl --context "$DC_B_CTX" install native-extension --component native \
  --version "$VERSION" -f "$VALUES_DIR/native.yaml"
vpcctl --context "$DC_B_CTX" verify native-extension
vpcctl --context "$AUTH_CTX" access issue --binding-namespace global-vpc-dc-b \
  --output "$ACCESS_DIR/authority-b.kubeconfig" --duration 1h
vpcctl --context "$DC_B_CTX" install global-vpc --component site \
  --version "$VERSION" -f "$VALUES_DIR/site-b.yaml" \
  --set-file "authorityAccess.kubeconfig=$ACCESS_DIR/authority-b.kubeconfig"
vpcctl --context "$DC_B_CTX" verify global-vpc
```

Now add DC-B's Subnet to the **existing** `production` VPC:

```sh
vpcctl --context "$AUTH_CTX" -n project-demo subnet create app-b \
  --vpc production --location dc-b --cidr 10.61.1.0/24
vpcctl --context "$AUTH_CTX" -n project-demo --timeout 5m subnet wait app-b
vpcctl --context "$AUTH_CTX" -n project-demo --timeout 5m vpc wait production
vpcctl --context "$AUTH_CTX" -n project-demo vpc get production
vpcctl --context "$AUTH_CTX" -n project-demo subnet list --vpc production
vpcctl --context "$AUTH_CTX" -n project-demo doctor
vpcctl --context "$DC_A_CTX" verify global-vpc
vpcctl --context "$DC_B_CTX" verify global-vpc
vpcctl --context "$DC_A_CTX" verify native-extension
vpcctl --context "$DC_B_CTX" verify native-extension
```

Attach a DC-B Pod and test both directions against the existing DC-A Pod:

```sh
NATIVE_SUBNET_B=$(kubectl --context "$AUTH_CTX" -n project-demo \
  get subnet.platform.globalvpc.io app-b -o jsonpath='{.status.nativeSubnetName}')
test -n "$NATIVE_SUBNET_B" || exit 1
sed -e "s|REPLACE_WITH_NATIVE_SUBNET_NAME|$NATIVE_SUBNET_B|g" \
    -e 's|name: endpoint$|name: endpoint-b|g' \
  config/examples/managed/smoke-pod.yaml | kubectl --context "$DC_B_CTX" apply -f -
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke \
  wait --for=condition=Ready pod/endpoint-b --timeout=120s
DC_A_IP=$(kubectl --context "$DC_A_CTX" -n managed-vpc-smoke \
  get pod endpoint-a1 -o jsonpath='{.status.podIP}')
DC_B_IP=$(kubectl --context "$DC_B_CTX" -n managed-vpc-smoke \
  get pod endpoint-b -o jsonpath='{.status.podIP}')
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint-a1 -- ping -c 5 "$DC_B_IP"
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke exec endpoint-a1 -- wget -qO- "http://$DC_B_IP:8080/"
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke exec endpoint-b -- ping -c 5 "$DC_A_IP"
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke exec endpoint-b -- wget -qO- "http://$DC_A_IP:8080/"
```

Deployment Ready establishes process health. VPC Ready and Helm tests establish
observed configuration/ACK state. Packet checks establish only the tested paths.
Gateway/BFD and physical failure tests remain separate qualification.
`NativeCapabilityRequired`, `NativeRoutesPending` and `GatewayPending` distinguish
missing capability, unacknowledged native state and incomplete gateway readiness.

## Upgrade and roll back software

Read the target release's compatibility and CRD migration notes. For a compatible
release on the same upstream native baseline:

```sh
NEXT_VERSION=0.1.2  # Example only: choose a published compatible release.
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

Each release has its own history. Select actual successful, compatible revisions:

```sh
vpcctl --context "$AUTH_CTX" history global-vpc
vpcctl --context "$DC_A_CTX" history global-vpc
vpcctl --context "$DC_B_CTX" history global-vpc
vpcctl --context "$DC_A_CTX" history native-extension
vpcctl --context "$DC_B_CTX" history native-extension

# Examples only: revision 2 includes the authority's DC-B registration;
# each Infra component's revision 1 is its initial installation.
vpcctl --context "$AUTH_CTX" rollback global-vpc 2
vpcctl --context "$DC_A_CTX" rollback global-vpc 1
vpcctl --context "$DC_B_CTX" rollback global-vpc 1
vpcctl --context "$DC_A_CTX" rollback native-extension 1
vpcctl --context "$DC_B_CTX" rollback native-extension 1
```

Do not roll the authority back to the DC-A-only configuration while managed
resources remain. That removes a registered allocation domain, so the guard
blocks it. Rollback restores Helm values/deployments, not tenant objects, retained
allocations or CRD schemas. A site rollback can restore expired access; issue a
fresh file and upgrade the selected version if necessary. Repeat configuration
verification and packet checks after rollback.

Compatible native image rollback can retain acknowledged active routes. Restoring
stock Kube-OVN requires the drainage sequence below. The project's chart does not
perform a full upstream Kube-OVN version upgrade.

## Drain and remove

Stop new intent. Remove workloads while all cleanup controllers remain available,
then drain public resources and uninstall components. If you stopped after the
DC-A stage, omit the DC-B commands:

```sh
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke delete pod endpoint-a1 endpoint-a2 --wait=true
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke delete pod endpoint-b --wait=true
vpcctl --context "$AUTH_CTX" --timeout 10m drain --project project-demo
vpcctl --context "$DC_A_CTX" uninstall global-vpc
vpcctl --context "$DC_B_CTX" uninstall global-vpc
vpcctl --context "$AUTH_CTX" uninstall global-vpc
vpcctl --context "$DC_A_CTX" uninstall native-extension
vpcctl --context "$DC_B_CTX" uninstall native-extension
```

Drain deletes Subnets before VPCs and waits for normal controller cleanup. In-use
native IPs or unavailable locations can block it. Finalizers and attached
workloads are never forcibly removed. Site hooks retain acknowledged cleanup
snapshots while refusing active bindings and gateways. Native hooks independently
check empty ACKs and absence of owned OVN route/BFD rows before restoring the
original stock digest and removing only the extension fields.

If a hook fails, inspect Helm `status`, hook Job logs and controller observations,
correct the cause, and retry the operation. Keep receipt Secrets and hooks.
Uninstall retains CRDs, namespaces, allocation receipts and Helm history; it does
not authorize address reuse. Baseline Kube-OVN remains installed.
