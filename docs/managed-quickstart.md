# Managed VPC quick start

This guide targets the experimental v0.1.0 managed implementation on `main`.
Its binary is `platform-vpc-controller` and its public API is
`platform.globalvpc.io/v1alpha2`. Users create project-scoped VPCs and Subnets.
The platform generates internal bindings, native Kube-OVN resources and multiple
gateways. Users do not maintain gateway IP, key, ASN or peer lists.

**Maturity:** this is an engineering preview, not production qualification.
Three transport profiles are implemented; hardware capacity, large-scale
forwarding, zero-loss lifecycle changes and unattended hardware recovery must be
qualified for the target deployment. See the [validation guide](managed-validation.md)
for test layers and known limitations. The API and upgrade path remain alpha.

## User input

After administrator setup, apply [network.yaml](../config/examples/managed/network.yaml)
to the management cluster. Its complete network input is:

```yaml
apiVersion: platform.globalvpc.io/v1alpha2
kind: VPC
metadata:
  name: production
  namespace: project-demo
spec: {}
---
apiVersion: platform.globalvpc.io/v1alpha2
kind: Subnet
metadata:
  name: app-a
  namespace: project-demo
spec:
  vpcRef: production
  locationRef: dc-a
  cidr: 10.60.1.0/24
---
apiVersion: platform.globalvpc.io/v1alpha2
kind: Subnet
metadata:
  name: app-b
  namespace: project-demo
spec:
  vpcRef: production
  locationRef: dc-b
  cidr: 10.61.1.0/24
```

The VPC is global; a Subnet belongs to one Infra/DC. This is not a stretched
regional subnet. Add a Subnet to join another authorized location. Different
VPCs can reuse CIDRs; Subnets within the same VPC cannot overlap.
The compute platform must resolve `Subnet.status.nativeSubnetName` when
attaching workloads. That adapter and a tenant UI are not implemented here.

## Administrator prerequisites

- One existing HA management Kubernetes API, plus one Kube-OVN Infra cluster
  per location. No new cross-site etcd or controller mesh is introduced.
- At least two eligible gateway nodes per Infra. Node InternalIPs must be
  routable between locations. Optional installation `endpointAnnotation`
  selects a platform-managed loopback annotation instead.
- An unchanged routed underlay, Linux transport support and sufficient MTU.
  WireGuard uses the reserved UDP range; native Geneve uses 6082, VXLAN control
  and data use 4788/4789. OVS UDP 6081 remains reserved.
  Native profiles require a trusted private underlay and do not encrypt.
- Workload, transit, BFD and control pool reservations from platform IPAM/NetBox.
  Sample ranges are examples, **not actual reservations**. Control/health pools
  must be disjoint across locations; exclude infrastructure ranges from tenant
  pools. This controller does not release NetBox parent reservations.
- Cross-location operation requires the reviewed native Kube-OVN
  [destination-routes.v1 extension](../integration/kube-ovn/README.md), including
  its CRD schema and matching controller binary. Applying the schema alone is
  insufficient. The local operator checks native generation/hash acknowledgements.
  Stock Kube-OVN does not provide this exact contract.

The extension keeps Kube-OVN as the sole writer of managed native OVN route
configuration. Source-pinned integration targets are provided for v1.16.4 and
[v1.16.3](../integration/kube-ovn/compat-v1.16.3/README.md). Use the target that
matches the actual native controller source and dependencies, and validate its
schema and controller binary together before deployment. A similar version
label does not establish compatibility. A single-location VPC can create native
Vpc/Subnet resources without the extension; this two-location example requires
it.

## Install the native Kube-OVN extension first

This is a required administrator step on **both Infra clusters** for the
two-location example. Building `platform-vpc-controller` and the gateway below
does **not** build or install the native Kube-OVN extension.

Follow [Build and install the native Kube-OVN extension](kube-ovn-extension-install.md)
through step 6, then return here. That guide provides the commands to:

1. Inventory the installed controller and CRD, identify their owner and select
   the exact v1.16.3 or v1.16.4 source target.
2. Fetch and verify the locked source, apply the patch, build the Linux native
   controller and run the matching tests.
3. Package the binary in the matching Kube-OVN distribution image, preserve its
   file capabilities and record the immutable image digest.
4. Add only `Vpc.spec.destinationRoutes` and `Vpc.status.destinationRoutes` to
   the existing CRD with guarded dry-run/apply commands.
5. Replace all existing native controller replicas and verify the running
   binaries before enabling managed intent.

The guide also specifies the separate real-OVN qualification gate, post-install
generation/hash checks and owner-driven rollback. Its direct update commands
target an administrator-owned evaluation installation; a GitOps-managed cluster
must carry the same changes through its existing resource owner. There is no
published prebuilt patched native image or one-command production installer.

Before continuing, both Infras must have the two schema fields, all native
replicas on the reviewed binary, and working baseline local networking. The
native intent acknowledgement and cross-site packet checks happen after the
managed example is created below.

## Build and prepare

These commands build the **platform and gateway images**. The separate native
controller build must already be complete. Run from the repository root with
Go, Docker/buildx, Python 3, jq and kubectl.
Replace registry/context placeholders and use immutable image digests.

```sh
docker build -f build/platform-controller.Dockerfile -t REGISTRY/platform-vpc:dev .
docker build -t REGISTRY/platform-gateway:dev gateway
docker push REGISTRY/platform-vpc:dev
docker push REGISTRY/platform-gateway:dev

AUTH_CTX=management
DC_A_CTX=infra-a
DC_B_CTX=infra-b
CONTROLLER_IMAGE=REGISTRY/platform-vpc@sha256:REPLACE
GATEWAY_IMAGE=REGISTRY/platform-gateway@sha256:REPLACE
MANAGED_DIR=$(mktemp -d)
chmod 700 "$MANAGED_DIR"
cp config/examples/managed/platform.json "$MANAGED_DIR/platform.json"
```

Edit the copied platform.json once: actual locations, project grants and reserved
workload pools. The example defines three network classes:

| Class | Transport |
|---|---|
| default | WireGuard/BGP |
| trusted-geneve | Geneve/BGP |
| trusted-evpn | VXLAN/EVPN |

A new VPC can set `spec.networkClassRef` to a class name. Transport changes
on an existing VPC require migration and are rejected. ToRs do not participate
in gateway BGP sessions.

## Install the management service

```sh
kubectl --context "$AUTH_CTX" apply -f config/crd/platform.globalvpc.io_vpcs.yaml
kubectl --context "$AUTH_CTX" apply -f config/crd/platform.globalvpc.io_subnets.yaml
kubectl --context "$AUTH_CTX" apply -f config/crd/platform.globalvpc.io_networkbindings.yaml
kubectl --context "$AUTH_CTX" wait --for=condition=Established --timeout=60s \
  crd/vpcs.platform.globalvpc.io crd/subnets.platform.globalvpc.io crd/networkbindings.platform.globalvpc.io
kubectl --context "$AUTH_CTX" apply -f config/managed/location-access.yaml
kubectl --context "$AUTH_CTX" apply -f config/managed/project-role.yaml
sed "s|REPLACE_WITH_PLATFORM_CONTROLLER_IMAGE_DIGEST|$CONTROLLER_IMAGE|g" \
  config/managed/authority.yaml | kubectl --context "$AUTH_CTX" apply -f -
kubectl --context "$AUTH_CTX" -n global-vpc-system create configmap platform-vpc-config \
  --from-file=platform.json="$MANAGED_DIR/platform.json" --dry-run=client -o yaml | \
  kubectl --context "$AUTH_CTX" apply -f -
kubectl --context "$AUTH_CTX" -n global-vpc-system rollout status deployment/platform-vpc-authority
```

The registry ConfigMaps use global-vpc-system, matching the sample Role.
If renaming it, update registryNamespace, CLI namespace, manifests and bindings.
Bind platform-vpc-project-editor to an actual project identity through a
RoleBinding in that project's namespace. Tenants must not have write access to
internal NetworkBindings, status, native Kube-OVN resources or operator namespaces.

## Install each local operator

Label two actual node names per Infra once. The controller discovers addresses
and generates keys; this label is the installation boundary.

```sh
kubectl --context "$DC_A_CTX" label nodes NODE_A1 NODE_A2 platform.globalvpc.io/gateway=true
kubectl --context "$DC_B_CTX" label nodes NODE_B1 NODE_B2 platform.globalvpc.io/gateway=true

for slot in a b; do
  case "$slot" in
    a) SITE_CTX=$DC_A_CTX ;;
    b) SITE_CTX=$DC_B_CTX ;;
  esac
  CLUSTER_UID=$(kubectl --context "$SITE_CTX" get namespace kube-system -o jsonpath='{.metadata.uid}')
  jq --arg uid "$CLUSTER_UID" --arg image "$GATEWAY_IMAGE" \
    '.clusterUID=$uid | .gatewayImage=$image' \
    "config/examples/managed/site-$slot.json" > "$MANAGED_DIR/site-$slot.json"
done
```

Review the generated files and replace infrastructure pools with reserved ranges.
gatewayReplicas is fixed for a binding's lifetime in this version. Use at least
2 and size transit blocks accordingly. Node UID/endpoint replacement is blocked
pending fenced recovery.

Production should use the platform's renewable location-scoped identity.
The helper below provides **one-hour development access** for the example
ServiceAccounts. It retains only the management server/CA and a newly issued
location token, and does not print credentials. Each identity can read only its
own location bindings and update their status.

```sh
for slot in a b; do
  case "$slot" in
    a) SITE_CTX=$DC_A_CTX ;;
    b) SITE_CTX=$DC_B_CTX ;;
  esac
  python3 scripts/managed-bootstrap-access.py --context "$AUTH_CTX" \
    --namespace "global-vpc-dc-$slot" --output "$MANAGED_DIR/authority-$slot.kubeconfig"
  kubectl --context "$SITE_CTX" apply -f config/crd/platform.globalvpc.io_networkbindings.yaml
  kubectl --context "$SITE_CTX" wait --for=condition=Established --timeout=60s crd/networkbindings.platform.globalvpc.io
  sed "s|REPLACE_WITH_PLATFORM_CONTROLLER_IMAGE_DIGEST|$CONTROLLER_IMAGE|g" \
    config/managed/site.yaml | kubectl --context "$SITE_CTX" apply -f -
  kubectl --context "$SITE_CTX" -n global-vpc-system create configmap platform-vpc-site-config \
    --from-file=site.json="$MANAGED_DIR/site-$slot.json" --dry-run=client -o yaml | \
    kubectl --context "$SITE_CTX" apply -f -
  kubectl --context "$SITE_CTX" -n global-vpc-system create secret generic platform-vpc-authority-access \
    --from-file=kubeconfig="$MANAGED_DIR/authority-$slot.kubeconfig" --dry-run=client -o yaml | \
    kubectl --context "$SITE_CTX" apply -f -
  kubectl --context "$SITE_CTX" -n global-vpc-system rollout status deployment/platform-vpc-site
done
```

The issuer controls the actual token lifetime. Before expiry, issue a new private
file, update the Secret and restart the local operator Deployment; inline
kubeconfig tokens are loaded at startup. Credential renewal/issuer integration
is not automated in this prototype. Never commit these files.
Expired management access stops snapshot updates and status reporting; it does
not withdraw accepted local dataplane state.

Controllers run non-root with two leader-elected replicas. Generated gateway
Pods use separate selected nodes, privileged hostPID access and no Kubernetes
API token. The operator namespace permits privileged Pods and is admin-only.

## Create and verify

```sh
kubectl --context "$AUTH_CTX" apply -f config/examples/managed/network.yaml
kubectl --context "$AUTH_CTX" -n project-demo wait \
  --for=condition=Ready --timeout=300s vpc.platform.globalvpc.io/production
kubectl --context "$AUTH_CTX" -n project-demo get vpcs.platform.globalvpc.io,subnets.platform.globalvpc.io

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

Repeat in the reverse direction. Deployment Ready means process health.
VPC Ready means configuration acknowledgements and local gateway health.
Only packet tests establish actual cross-site forwarding for the tested path.
Also run the native extension guide's
[generation and expected-hash check](kube-ovn-extension-install.md#7-return-to-the-managed-quick-start-and-verify-acknowledgement)
for each Infra's native VPC.

```sh
kubectl --context "$AUTH_CTX" -n project-demo describe vpc.platform.globalvpc.io production
kubectl --context "$AUTH_CTX" -n global-vpc-dc-a get networkbindings -o yaml
kubectl --context "$DC_A_CTX" -n global-vpc-system get networkbindings,pods
kubectl --context "$DC_A_CTX" -n global-vpc-system logs deployment/platform-vpc-site --tail=100
```

NativeCapabilityRequired means the extension is absent. NativeRoutesPending
means its exact generation/hash is not acknowledged. GatewayPending means local
gateway readiness is incomplete. Lost allocation records or changed resource
UIDs block reconfiguration; do not delete receipts to clear these conditions.

## Delete the example

Delete workload Pods first, public Subnets second, then the VPC. Do not delete
internal bindings or remove finalizers to accelerate cleanup.

```sh
kubectl --context "$DC_A_CTX" -n managed-vpc-smoke delete pod endpoint
kubectl --context "$DC_B_CTX" -n managed-vpc-smoke delete pod endpoint
kubectl --context "$AUTH_CTX" -n project-demo delete subnets.platform.globalvpc.io app-a app-b --wait=false
kubectl --context "$AUTH_CTX" -n project-demo wait --for=delete --timeout=300s \
  subnet.platform.globalvpc.io/app-a subnet.platform.globalvpc.io/app-b
kubectl --context "$AUTH_CTX" -n project-demo delete vpc.platform.globalvpc.io production --wait=false
kubectl --context "$AUTH_CTX" -n project-demo wait --for=delete --timeout=300s vpc.platform.globalvpc.io/production
```

Native IP allocations block in-use deletion. Source native subnet removal must
be acknowledged before peers withdraw its route. Unavailable locations may
delay deletion without authorizing address reuse. Allocation receipts remain
reserved after cleanup. Keep the operators and their records until all managed
objects have drained.

To remove the native extension itself, continue with its
[withdrawal and rollback procedure](kube-ovn-extension-install.md#8-withdraw-and-roll-back).
Restoring a stock image or removing schema fields before route/BFD drainage is
not a valid rollback.
