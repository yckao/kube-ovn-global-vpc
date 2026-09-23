# Release and publication

Helm charts are the primary distribution. A published release contains prebuilt
images and downloadable `vpcctl` binaries; `vpcctl` wraps the Helm lifecycle.
Administrators do not need Go, Docker or BuildKit on their machines. Those tools
belong to the project's release build environment.

[v0.1.1](https://github.com/yckao/kube-ovn-global-vpc/releases/tag/v0.1.1) was
published on September 23, 2026 by the successful
[release run](https://github.com/yckao/kube-ovn-global-vpc/actions/runs/35817763742)
from source commit `3015225cf5dad4176a4cd98672872cbe50b72168`. It contains five
OCI images, three Helm charts, four CLI builds and 18 downloadable assets in
total, including checksums, source and build evidence. Publication does not
establish deployment or packet-forwarding qualification.

The current version is **v0.1.1**, an experimental release. `VERSION` contains
`0.1.1`; Git tags and workflow version inputs use `v0.1.1`. Public APIs remain
`platform.globalvpc.io/v1alpha2`. Project versions, chart versions and Kubernetes
API versions are separate contracts.

## Distribution contract

A published release provides these three charts as GitHub release `.tgz` assets
and Helm OCI packages:

| Chart | Responsibility |
| --- | --- |
| `global-vpc` | Management authority, public APIs and project access |
| `global-vpc-site` | One site's operator, gateway configuration and authority access |
| `kube-ovn-global-vpc-extension` | Native extension installation and lifecycle hooks for an existing supported Kube-OVN baseline |

The OCI path is
`oci://ghcr.io/<owner>/<repository>/charts/<chart>`. Chart versions use the numeric
project version, such as `0.1.1`; image and Git release tags use `v0.1.1`.

Release chart defaults contain the actual immutable controller, gateway and
hook-helper image references. The native chart also embeds both qualified native
bundles as `files/native-v1.16.3.json` and `files/native-v1.16.4.json`; `native.target`
selects the appropriate baseline. Users provide site configuration and access
information, not local build outputs. Source charts intentionally have no
invented image digests and must receive release defaults or explicit test inputs.

`release.json` identifies the project source commit, controller and gateway
images, CLI hook image, both native bundles and chart asset URLs. Every image
reference is an actual `NAME@sha256:...` value returned by the build. The release
assembler rejects missing targets, inconsistent native qualification, mutable
image tags and missing source/chart/CLI artifacts. `SHA256SUMS` covers the release
descriptor and all adjacent downloadable assets.

## Supported build matrix

| Artifact | Build target | Toolchain |
| --- | --- | --- |
| `vpcctl` binaries | Linux and macOS, each on amd64 and arm64 | Go 1.26.5 |
| Platform controller binaries | Linux amd64 and arm64 | Go 1.26.5 |
| Controller, gateway and CLI hook images | Linux/amd64 | Prebuilt project binaries; digest-pinned runtime bases |
| Native Kube-OVN v1.16.3 image | Linux/amd64 | Go 1.26.6; source `98af25ffae49193a8dc16bbc39bd8ca4110ec367` |
| Native Kube-OVN v1.16.4 image | Linux/amd64 | Go 1.27.1; source `a9296ef2a37c6519bc0ecb798082ce139c72f8eb` |

CI uses Python 3.13 and Helm 3.17.3. Actions use immutable commit IDs. The Helm
archive is checked against the version's upstream SHA-256 file. Buildx runs only
on the Linux CI runner; its actual version and image build inputs are preserved
with release evidence. This is not a claim that every transitive OS package or
build-tool image is independently pinned or that image builds are reproducible.
Cross-compiling an arm64 binary does not qualify arm64 container deployment.

## Candidate workflow

Run **Build prebuilt release** from the intended reviewed ref with:

| Input | Candidate value |
| --- | --- |
| `version` | `v` followed by the exact contents of `VERSION` |
| `publish` | `false` (default) |
| Four base-image inputs | Unused; may be left empty |

The workflow runs formatting/vet, Go/Python tests, race tests, binary builds,
Helm chart tests, documentation validation and isolated Kubernetes API
integration tests. It then uploads the candidate artifacts to the Actions run.
Candidate artifacts are retained for 14 days by the workflow.

Candidates include the CLI binaries, controller binaries, source, vendored
project dependencies, templates, chart packages, provenance and checksums.
`CANDIDATE-NOTES.md` states that chart defaults still require immutable image
values. This mode does not build or push OCI images, produce an installable
`release.json`, create a Git tag or create a GitHub release. An uploaded candidate
is not an operationally qualified network release.

## Publish a prebuilt experimental release

The same workflow publishes when `publish=true`. Supply all of these inputs:

| Input | Required value |
| --- | --- |
| `version` | `vMAJOR.MINOR.PATCH`, exactly matching `VERSION`; the Git tag must not already exist |
| `publish` | `true` |
| `controller_base` | Reviewed distroless nonroot base as `NAME@sha256:<64 lowercase hex digits>`; also used by the CLI hook image |
| `gateway_base` | Reviewed Alpine 3.21 base in the same immutable format |
| `native_base_v1163` | Verified original Kube-OVN v1.16.3 Linux/amd64 distribution image in the same immutable format |
| `native_base_v1164` | Verified original Kube-OVN v1.16.4 Linux/amd64 distribution image in the same immutable format |

The reviewed v0.1.1 inputs are recorded in
[build/release-bases.json](../build/release-bases.json), including the source-revision
comparison for the upstream v1.16.4 release-tag commit. Use its platform-specific
digests when dispatching this release.

The maintainer must verify that native base images match their stated baselines.
The workflow validates digest syntax; a correctly formatted digest alone does
not establish source compatibility. No example or guessed digest is supplied.
The repository must permit its workflow token to publish GHCR packages and
create GitHub releases. Registry visibility and release access must match the
intended consumers.

New GHCR packages default to private. After the first push, check public pull
access for these packages and change any private package to **Public** in its
GitHub settings:
`platform-vpc-controller`, `gateway`, `vpcctl`, `kube-ovn` (both native tags), and
the three packages under `charts/`. The repository source label associates an
image with this repository; it does not make the package public. Confirm that
an unauthenticated client can pull all five image tags and all three charts
before advertising the release as ready for installation. A successful workflow
push using its registry credentials does not establish anonymous access.

After the candidate checks pass, publication proceeds as follows:

1. Build and push the controller, gateway and CLI hook images to GHCR with
   BuildKit SBOM and provenance attestations.
2. Build each native controller from its exact source lock and patch, run its
   native unit tests, push the replacement image and verify the binary checksum
   and file capabilities inside that image.
3. Preserve patched native production source, vendored Go dependencies, original
   licenses, source locks, patch files and qualification reports.
4. Retrieve OCI SBOM/provenance evidence from all five pushed image references.
   Run the controller and hook-helper binaries with networking disabled to
   check the exact version/commit; check required gateway executables. These
   commands do not start the platform or configure a cluster.
5. Package all three charts with the returned immutable image digests and both
   native bundles. Assemble `release.json` and final checksums, then push the
   charts to GHCR Helm OCI.
6. Upload reviewable Actions artifacts and create a GitHub **prerelease** with
   the complete asset set at the exact workflow commit.

This workflow performs external writes when `publish=true`. It is not a
transaction across registries and GitHub: a failure after an image push may
leave image artifacts without a completed GitHub release. Review the failed run
and its available evidence; do not advertise an incomplete upload as an
installable release. The workflow refuses to overwrite an existing Git tag and
does not delete partial registry uploads automatically.

The workflow does not deploy into an operator's Kubernetes cluster. It records
real OVN/northd and packet-forwarding qualification as **not run**; native unit
checks, API tests and executable smoke tests do not establish BFD failover,
cross-site traffic, hardware behavior or an operational support SLA.

## Release assets and verification

For version `v0.1.1`, the completed workflow produces:

| Asset | Contents |
| --- | --- |
| `vpcctl_0.1.1_{linux,darwin}_{amd64,arm64}.tar.gz` | Four CLI archives, each including project license/notices |
| `platform-vpc-controller_0.1.1_linux_{amd64,arm64}.tar.gz` | Controller binaries and project license/notices |
| `{global-vpc,global-vpc-site,kube-ovn-global-vpc-extension}-0.1.1.tgz` | Primary Helm packages with prebuilt-image defaults |
| `release.json` | Immutable images, inline native bundles, chart URLs and source identity |
| `kube-ovn-global-vpc_0.1.1_source.tar.gz` | Full tracked project source from the release commit |
| `project-vendored-source.tar.gz` | Project source plus vendored Go dependencies and included license files |
| `native-v1.16.3-source.tar.gz`, `native-v1.16.4-source.tar.gz` | Exact patched production source, vendored dependencies, patch, lock, modification notice and qualification |
| `managed-installation_0.1.1.tar.gz` | Reference CRDs, templates, examples and native integration inputs; Helm charts are the primary installer |
| `image-evidence.tar.gz` | Image manifests, retrieved SBOM/provenance, build inputs, tool versions, runtime checks and native qualification |
| `provenance.json` | Project packaging metadata: source commit, toolchain, build options and hashes of its initial packaged inputs/outputs |
| `SHA256SUMS` | Final checksums of all adjacent release assets |

Download assets from the intended GitHub release and verify before extracting:

```sh
# After downloading the full release asset set into one directory:
sha256sum -c SHA256SUMS

# On macOS, the corresponding command is:
shasum -a 256 -c SHA256SUMS
```

The binary `--version` commands require no Kubernetes credentials and include
the packaged source commit. Checksums establish integrity relative to the
checksum file; they do not replace publisher authentication. Packaging
provenance is unsigned metadata. OCI attestations are retrieved from the build,
but this workflow does not add cryptographic signatures, assert a SLSA level or
perform a vulnerability scan. Review the
[third-party notices](../THIRD_PARTY_NOTICES.md) for the exact source/license
material supplied and the responsibilities it does not discharge.

## Maintainer-only local packaging

Local development can use Go 1.26.5, Python, Git and Helm. End-user installation
uses the prebuilt release instead. `scripts/package-release.py` requires a clean
committed checkout and refuses modified/untracked source, invalid versions,
symlinked installation inputs or an existing output directory.

```sh
make check test test-race build
KUBEBUILDER_ASSETS=/path/to/envtest make test-integration

# Keep generated documentation outside the Go source tree.
python3 scripts/build-docs.py --output /tmp/global-vpc-docs --check
make release-artifacts
```

The local packager produces the binary/source/reference-input subset under
`dist/v0.1.1/`. Chart default injection, OCI publication, vendored source material
and final descriptor assembly are additional workflow steps. Running
`make release-artifacts` alone neither publishes images nor creates a complete
installable release.

Build flags disable ambient Go workspaces/overrides, use `CGO_ENABLED=0`,
`-trimpath`, `-buildvcs=false` and the local toolchain, and record the source
commit separately. Package archive timestamps and ownership are normalized.
Private installation configuration and credentials are not release inputs.

## Documentation publication

The separate **Documentation** workflow validates pull requests and deploys
`main` to GitHub Pages when Pages is configured to use GitHub Actions. A manual
run can also deploy `main`. Website publication does not create a software
release, push OCI packages or change a cluster.

Future releases update `VERSION`, `CHANGELOG.md`, chart compatibility guidance
and migration instructions together. Experimental `0.x` releases may introduce
explicitly documented breaking changes; publish the native-baseline and
rollback boundaries with each release.
