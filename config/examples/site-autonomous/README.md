# D deployment examples

Follow the [quick start](../../../docs/quickstart.md) from the repository root.
Do not apply this directory recursively: Site A and Site B files target
different clusters and intentionally share local resource names.

| File | Purpose |
| --- | --- |
| `namespace.yaml` | Gateway namespace and token-free local kubeconfig |
| `ovn-exec-rbac.yaml` | Local OVN exec access; review its kube-system scope |
| `deployment.yaml` | One active D controller per local attachment |
| `site-a-config.yaml`, `site-b-config.yaml` | Paired static two-gateway WireGuard registrations |
| `site-a-vpc.yaml`, `site-b-vpc.yaml` | Same tenant identity, different local workload subnets |
| `smoke-pod.yaml` | HTTP test workload; render its local Subnet annotation first |

Also install the existing `config/rbac/site.yaml` and SiteVpc CRD as described in
the guide. Replace node names, underlay addresses, UIDs, public keys and image
references before deploying. Private keys are created separately as Secrets.

The three-profile implementation is already present, but the complete paired
quick start uses WireGuard. Native Geneve and VXLAN/EVPN registrations are linked
from the guide. There is no Gateway Directory in these manifests.
