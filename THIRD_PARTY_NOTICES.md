# Third-party notices

Project-authored code is licensed under Apache-2.0. Dependencies retain their own
licenses. The root license does not relicense container packages, vendored
modules, Kube-OVN or separately executed programs, and no upstream endorsement is
implied.

The project implements a prebuilt release pipeline for CLI binaries, Helm charts
and container images. That implementation does not establish that a release has
already been published or that every redistribution obligation has been
reviewed. This document explains the materials the workflow supplies and their
limits; it is not a complete per-file license inventory or legal-compliance
certification.

## Project source and binaries

The source archive contains the tracked project source at the release commit,
including `LICENSE`, `NOTICE`, this file and the build/patch scripts. Binary
archives include the project license and notices. The controller and CLI hook
images also include these files under `/usr/share/licenses/global-vpc/`.

`project-vendored-source.tar.gz` adds the Go dependencies selected by the project
module graph, preserving license files included by Go vendoring. `go.mod` and
`go.sum` record module versions and integrity hashes. Vendored source makes the
actual dependency source and included notices available; it is not a substitute
for reviewing the applicable licenses, attribution requirements or completeness
of each dependency's upstream notices.

## Patched Kube-OVN

The native integration contains companion code and changes to pinned Kube-OVN
source for the `destination-routes.v1` contract, controller reconciliation and
OVSDB behavior. These changes are by Global VPC Controller contributors. The
source locks identify exact upstream commits; `native.patch` and `overlay/`
identify project changes. Preserve original source headers and identify the
modifications when redistributing patched source.

Kube-OVN is Copyright The Kube-OVN Authors and uses Apache-2.0. Its source license
is retained in each native source archive; the Apache-2.0 text is also provided
in the project's [LICENSE](LICENSE).

Each published native source archive contains the production source used for the
patched binary, vendored Go dependencies, upstream license files, the patch,
source lock, modification notice, native bundle and qualification report.
Packaging verifies that vendoring did not change the production `go.mod` or
`go.sum`. Build qualification records the exact source commit, patch checksum,
toolchain, binary checksum and resulting immutable image reference.

These native archives cover the patched controller source and its vendored Go
dependencies. They do **not** contain the complete corresponding source of every
OS package, Open vSwitch/OVN component or other program inherited from the
original Kube-OVN image.

## Container bases and gateway packages

Controller and CLI hook images inherit a maintainer-selected distroless base.
Native images inherit the corresponding Kube-OVN distribution image. The gateway
image uses a maintainer-selected Alpine base and installs FRRouting, WireGuard
tools, iproute2, util-linux, Python, tcpdump, iputils, nftables and iperf3 with their
runtime dependencies. These components are independently licensed.

The release workflow requires immutable base-image references and records them.
BuildKit emits SBOM and provenance attestations for the resulting images; the
workflow retrieves those documents and preserves them in `image-evidence.tar.gz`
alongside image manifests and build/qualification evidence. The documents report
what the build tools detected. They may contain missing or unasserted license
information and do not establish a complete source or license audit.

Maintainers distributing an image remain responsible for reviewing the actual
package versions and licenses, preserving required notices and providing
corresponding source, build instructions or other materials required by those
licenses. Relevant source must match the distributed packages and modifications.
In particular, the project/native Go source archives are not a complete source
distribution for the gateway or inherited container OS packages. A generic
upstream URL, an SBOM or this notice alone does not discharge those obligations.

No automated license review, vulnerability scan, artifact signing or legal
signoff is claimed by this pipeline. See the
[release guide](docs/releasing.md) for the artifact contract, build matrix and
checks that are actually implemented.
