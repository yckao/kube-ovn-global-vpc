# Third-party notices

The project code is licensed under Apache-2.0. External dependencies keep their
own licenses; the root license does not relicense container packages or separately
executed programs. This is a source release, not a complete binary/image notice inventory.

## Kube-OVN integration

The `integration/kube-ovn/` directory contains original companion code and changes
to pinned Kube-OVN source. The exact upstream commits are in the source locks;
`native.patch` and `overlay/` identify changes for the destination-routes.v1
contract, native controller reconciliation and OVSDB behavior. These changes are
by Global VPC Controller contributors. Preserve original source headers when
applying them and identify modified files in redistributed patched source.

Kube-OVN is Copyright The Kube-OVN Authors, Apache-2.0:
[upstream license](https://github.com/kubeovn/kube-ovn/blob/a9296ef2a37c6519bc0ecb798082ce139c72f8eb/LICENSE).
The complete Apache-2.0 text is included in [LICENSE](LICENSE).
No upstream endorsement is implied.

## Go modules

`go.mod` and `go.sum` record requested modules and integrity hashes. Kubernetes,
controller-runtime and their dependencies retain upstream attribution/licenses.
Generate the complete transitive dependency/license inventory and SBOM from the
actual build before distributing binaries. A module list is not that inventory.

## Gateway runtime and OS packages

The gateway Dockerfile installs separately distributed programs including
FRRouting, WireGuard tools, iproute2, util-linux, Python and other OS packages.
They are not covered by the project's Apache-2.0 grant.

FRRouting has per-file licenses; its combined binary is generally distributed
under GPLv2-or-later. See [FRR COPYING](https://github.com/FRRouting/frr/blob/master/COPYING)
and the relevant [Alpine package recipe](https://github.com/alpinelinux/aports/blob/3.21-stable/community/frr/APKBUILD).
Before distributing an image, record the actual package versions, include
applicable license texts/notices, and provide corresponding source/build patches
as required by their licenses. See [GPLv2 section 3](https://www.gnu.org/licenses/old-licenses/gpl-2.0.en.html#section3).

This repository does not claim that a generic source URL or this notice alone
satisfies all obligations for a future binary/container distribution. Pinning,
SBOMs, source availability and license review are release gates for those artifacts.
