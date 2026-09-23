# Operate VPCs with Helm and vpcctl

Helm charts are the primary installation interface. `vpcctl` wraps Helm for
installation, upgrades, release history, rollback, verification and removal.
Both interfaces use the same chart, values, hooks and Helm release Secrets.
The CLI also operates project-scoped VPCs and Subnets through the Kubernetes API.

Project release CI builds the controller, gateway, native extension and hook
images, plus standalone CLI binaries. An administrator needs Helm 3.14+ or Helm 4,
a downloaded `vpcctl` binary, and Kubernetes access. Tenants need only `vpcctl`
and their project access. Go, Docker, BuildKit, Python and jq are not client
requirements. See the [quick start](managed-quickstart.md) for a complete example.

**Availability:** the repository contains this release workflow; this change does
not itself publish a release or qualify a live deployment. Use commands below
with a version whose GitHub Release includes the prebuilt images and packaged
charts. A source checkout's chart defaults intentionally lack release digests.

## Install the CLI

Choose a published version and the archive matching your computer: `linux` or
`darwin`, and `amd64` or `arm64`. For example, on an Apple Silicon Mac:

```sh
VERSION=0.1.0  # Select an actually published prebuilt release.
ASSET="vpcctl_${VERSION}_darwin_arm64.tar.gz"
BASE="https://github.com/yckao/kube-ovn-global-vpc/releases/download/v${VERSION}"
curl -fLO "$BASE/$ASSET"
curl -fLO "$BASE/SHA256SUMS"
awk -v name="$ASSET" '$2 == name { print }' SHA256SUMS > CLI-SHA256SUMS
# The filtered file must contain exactly one entry.
test "$(wc -l < CLI-SHA256SUMS | tr -d ' ')" = 1
shasum -a 256 -c CLI-SHA256SUMS
mkdir -p cli "$HOME/.local/bin"
tar -xzf "$ASSET" -C cli
install -m 0755 cli/vpcctl "$HOME/.local/bin/vpcctl"
export PATH="$HOME/.local/bin:$PATH"
vpcctl version
```

Linux can use `sha256sum -c CLI-SHA256SUMS`. Checksums establish consistency with
the downloaded release metadata, not an independent signature. Native extension
and gateway release images currently target **Linux/amd64**; CLI arm64 support
does not imply an arm64 dataplane.

## Helm lifecycle

| Component | Published chart | Installation scope |
|---|---|---|
| `authority` | `global-vpc` | One management cluster |
| `site` | `global-vpc-site` | Each Infra cluster |
| `native` | `kube-ovn-global-vpc-extension` | One supported Kube-OVN controller per Infra |

Charts are published beneath
`oci://ghcr.io/yckao/kube-ovn-global-vpc/charts`. GitHub Releases also contain the
three chart archives for direct Helm use. Packaged charts include immutable image
references; the native chart includes the matched schema and build bundle.

Global flags precede the command. `--context` selects a kubeconfig context without
changing it. `--kubeconfig` and `KUBECONFIG` follow Kubernetes loading rules.
Lifecycle commands default to namespace `global-vpc-system`; tenant commands
use the selected context's namespace unless `-n` is supplied.

```sh
vpcctl values --component authority --version "$VERSION" > authority-values.yaml
# Edit locations, approved pools and project subjects before installation.
vpcctl --context management plan global-vpc --component authority \
  --version "$VERSION" -f authority-values.yaml
vpcctl --context management install global-vpc --component authority \
  --version "$VERSION" -f authority-values.yaml
vpcctl --context management status global-vpc
vpcctl --context management verify global-vpc
vpcctl --context management history global-vpc
```

`install` executes `helm upgrade --install --wait`. `plan` executes its server-side
dry-run with Secret output hidden; it does not run hooks and is not a guarantee
that installation will succeed. Values may contain sensitive data outside Secret
objects, so keep plan output private when necessary. `verify` runs `helm test`.
The CLI passes values directly as process arguments without invoking a shell.

The equivalent install through Helm is:

```sh
helm upgrade --install global-vpc \
  oci://ghcr.io/yckao/kube-ovn-global-vpc/charts/global-vpc \
  --kube-context management --namespace global-vpc-system --create-namespace \
  --version "$VERSION" -f authority-values.yaml --wait --timeout 5m
```

To upgrade and return to an earlier release revision:

```sh
NEXT_VERSION=0.1.1  # Example only: select a published compatible release.
vpcctl --context management upgrade global-vpc --component authority \
  --version "$NEXT_VERSION" -f authority-values.yaml
vpcctl --context management history global-vpc
# Select the actual successful revision shown by history.
vpcctl --context management rollback global-vpc 1
vpcctl --context management verify global-vpc
```

`upgrade` uses Helm's `--reset-then-reuse-values`: prior user overrides remain,
while new chart defaults supply updated image digests. Explicit image overrides
remain explicit. Rollback restores the selected revision's values, manifests and
hooks; it does not undo tenant changes, migrate allocation receipts or downgrade
CRDs. Helm retains CRDs from `crds/`; schema migrations require release-specific
instructions. Hooks reject incompatible configuration changes while managed
resources exist. Never use uninstall as a substitute for an upgrade.

Use the same commands with the site or native component and the corresponding
Infra context. Native upgrades and rollbacks support the **same upstream source
baseline and extension contract**. Changing upstream Kube-OVN from v1.16.3 to
v1.16.4 is a separate upstream installation upgrade, not a one-container update.

Native chart hooks add the two extension schema properties, update only the
native controller image, and verify rollout/ACK state. A private Kubernetes
Secret records the original state before mutation. Failed hooks retain it for
retry; keep that Secret and namespace. No local plan file is needed. One
extension release may target a given native controller. The extension does not
install or remove upstream Kube-OVN itself.

## Scoped location access

Use your identity system to provision and renew a kubeconfig restricted to the
location's authority binding namespace. Set `authorityAccess.existingSecret` to
a Secret containing key `kubeconfig`. For evaluation, the CLI can issue a
short-lived file using the chart-created `binding-reporter` ServiceAccount:

```sh
umask 077
vpcctl --context management access issue --binding-namespace global-vpc-dc-a \
  --output ./authority-a.kubeconfig --duration 1h
```

The output must be a new file. The command copies only the authority endpoint
and trusted CA, and requests the scoped token; it does not copy administrator
credentials. The issuer decides the actual expiration. It is not a credential
renewal service. Supply this file with `--set-file authorityAccess.kubeconfig=...`
on site install/upgrade. Helm stores inline credentials in release history.
Credential changes trigger a site controller rollout. A rollback can restore an
expired credential: supply fresh access with an upgrade afterward. Never commit
or share these private files or unredacted Helm values.

## Tenant resource lifecycle

The administrator grants a project namespace access to public VPCs and Subnets.
Tenants do not need internal NetworkBinding, native Kube-OVN or operator Secret
access. Workloads are attached separately through the platform's compute/CNI
interface.

```sh
vpcctl --context management -n project-demo vpc create production
vpcctl --context management -n project-demo subnet create app-a \
  --vpc production --location dc-a --cidr 10.60.1.0/24
vpcctl --context management -n project-demo subnet create app-b \
  --vpc production --location dc-b --cidr 10.61.1.0/24
vpcctl --context management -n project-demo vpc list
vpcctl --context management -n project-demo subnet list --vpc production
vpcctl --context management -n project-demo -o yaml subnet get app-a
vpcctl --context management -n project-demo --timeout 5m vpc wait production
vpcctl --context management -n project-demo doctor
```

Select approved locations and pools. `vpc create --network-class NAME` chooses
an administrator-defined transport class. Class, location and CIDR identities
are immutable in the current API; a change requires migration. Creation does
not overwrite existing objects. Add `--wait` to wait after API acceptance, or
`--dry-run` to render validated YAML without connecting to the API.

`wait` requires Ready for the current generation and pins the object's UID.
`doctor` reports public API access and stale/pending observations. Neither proves
packet delivery. Follow the quick start's workload packet checks for that claim.

Remove attached workloads before deleting their networks:

```sh
vpcctl --context management -n project-demo subnet delete app-a --wait
vpcctl --context management -n project-demo subnet delete app-b --wait
vpcctl --context management -n project-demo vpc delete production --wait
```

Deletes are guarded by UID/resourceVersion. VPC deletion refuses remaining
Subnets. A timeout leaves the accepted request in place; it never strips
finalizers. Retained allocation receipts are not released for reuse.

## Drain and uninstall

Stop new tenant changes and remove attached workloads first. Keep all cleanup
controllers running until public intent has drained. Drain operates only on
explicitly selected projects and deletes Subnets before VPCs:

```sh
vpcctl --context management --timeout 10m drain --project project-demo
# Repeat --project for every managed project before removing the service.
vpcctl --context infra-a uninstall global-vpc
vpcctl --context infra-b uninstall global-vpc
vpcctl --context management uninstall global-vpc
vpcctl --context infra-a uninstall native-extension
vpcctl --context infra-b uninstall native-extension
```

Chart pre-delete checks refuse active resources or incomplete cleanup. Completed
local cleanup snapshots may remain as receipts. Checks are observations, not a
transaction against concurrent new intent; keep writes stopped throughout
removal. Native removal additionally requires empty acknowledged intent and
independent absence of owned OVN static-route/BFD rows, restores the original
immutable stock image, then removes only the two extension schema properties.
A failed check leaves the release/state available for retry.

Uninstall keeps Helm history by default. CRDs, allocation records and namespaces
remain; removal does not authorize address reuse. Do not delete records or use
Helm hook bypasses to force removal. Installations with workloads or broken
cleanup require diagnosis, not automatic destructive reset.

## Maintainer tools

Only release builders need Go, Git, Python and Docker/buildx. Low-level native
inspection, guarded plans and recovery live under `vpcctl maintainer native`;
they are not the administrator installation interface. The
[native reference](kube-ovn-extension-install.md) documents the underlying patch
and build operations. [Release maintenance](releasing.md) explains how CI turns
them into the prebuilt distribution.
