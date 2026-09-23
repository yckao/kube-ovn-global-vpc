# Release and publication

The prepared version is **v0.1.0**, an experimental release. The `VERSION` file
contains `0.1.0`; use the `v` prefix for Git tags. Public APIs are currently
`platform.globalvpc.io/v1alpha2`. The project version and Kubernetes API version
are separate contracts.

The initial public distribution is **source-only**. Binary and image distribution
remain gated on their complete transitive notices, SBOM and provenance review.
The manual workflow can prepare binary candidates for maintainer review; an
Actions artifact is not a published supported release.

## What is automated

- Pull requests and pushes to `main`: Go formatting, vet, unit tests, race tests,
  Python tests, builds and Kubernetes API-server integration tests.
- Pull requests: build and validate the documentation website without deploying.
- Pushes to `main`: publish the generated website when GitHub Pages has been
  configured to use GitHub Actions.
- Manual **Prepare release artifacts** workflow: validate and build a clean
  checkout, then upload downloadable build artifacts to the workflow run. It
  does not create a tag, create a GitHub Release, upload images or deploy software.

GitHub Actions dependencies use reviewed immutable commit IDs. The pinned action
tags were checked against the official [GitHub Actions organization](https://github.com/actions).
Dependabot proposes updates; review each update before merging. CI downloads Go
modules and checksum-verified envtest assets from their upstream providers.

The API integration tests run a local API server and etcd without cluster
credentials. They are not packet-forwarding, native Kube-OVN or hardware tests.
The separately source-locked Kube-OVN extension requires its own source-matched
build and integration review before deployment; see
[native integration](../integration/kube-ovn/README.md).

## Build the release locally

Use Go 1.26.5, Python 3.13 or newer, Git and a clean committed checkout. The
packager refuses modified or untracked source files, invalid versions and
symbolic links in its installation input. Build output directories are ignored
by Git. No Kubernetes credentials are needed.

```sh
make check test test-race build
# With downloaded envtest assets:
KUBEBUILDER_ASSETS=/path/to/envtest make test-integration
make docs
make release-artifacts
```

Output appears in `dist/v0.1.0/`:

| File | Purpose |
| --- | --- |
| `kube-ovn-global-vpc_0.1.0_source.tar.gz` | Complete reviewed source commit, including documentation and native integration inputs |
| `platform-vpc-controller_0.1.0_linux_amd64.tar.gz` | Linux amd64 controller binary |
| `platform-vpc-controller_0.1.0_linux_arm64.tar.gz` | Linux arm64 controller binary; cross-compilation alone is not runtime qualification |
| `managed-installation_0.1.0.tar.gz` | Managed CRDs, RBAC/deployment templates, example configuration, gateway Python runtime and native extension inputs |
| `provenance.json` | Source commit, version, build toolchain, input hashes and output hashes |
| `SHA256SUMS` | SHA-256 checksums for the packages and provenance file |

The installation package is a reviewed input bundle, not a one-command
installation or a complete source distribution. The clean source checkout is
the source of truth; the source archive is generated with `git archive` from
that exact commit, without local branches or Git history. Image digest placeholders, site identities, address-pool
reservations and platform access identities still require administrator setup.
Private configuration, runtime credentials, validation captures and historical
environment material are excluded by an explicit packaging allowlist.

The packager uses `CGO_ENABLED=0`, `-trimpath` and `-buildvcs=false` for the
controller binaries, disables ambient workspaces/Go build overrides, requires
the local Go toolchain and records the actual source commit separately. Tar/gzip
timestamps and ownership are normalized. This supports repeatable packaging
with the same toolchain; it is not a claim of audited reproducible container
images. `provenance.json` is unsigned build metadata, not a signed supply-chain
attestation. Verify artifact checksums after downloading:

```sh
sha256sum -c SHA256SUMS
./platform-vpc-controller --version
```

The controller's `--version` command runs without Kubernetes access. Packaged
binaries print the source commit and its commit timestamp, not an invented build
time. Direct developer builds report unknown source metadata unless supplied by
the build process.

## Container and native-extension artifacts

Build the controller and gateway images using the commands in the
[quick start](managed-quickstart.md). Select a registry owned by the project or
your organization, publish there deliberately, and record immutable image
digests. Do not replace example digests with a guessed registry path.

Gateway images contain independently licensed system components, including FRR.
Review the distribution obligations described in the repository's third-party
notices before publishing images. The controller container and the native
Kube-OVN extension have different build inputs and compatibility boundaries.
Do not label a controller-only build as a complete supported network stack.

Before distributing container images, pin and record their base image digests,
produce an SBOM, scan the final images, review applicable redistribution/source
requirements and sign artifacts using the project's chosen identity. These
steps remain release-owner responsibilities; the initial workflows do not
pretend to perform them.

## Enable GitHub Pages

1. Create or select the public GitHub repository and push the reviewed `main`
   branch. Review the complete public tree and its ancestry for confidential
   material before publication.
2. In **Settings → Pages → Build and deployment**, choose **GitHub Actions**.
3. Enable Actions and allow the `github-pages` environment to deploy from `main`.
   Add environment approval rules if the maintainers require them.
4. Run **Documentation** manually, or push a documentation change to `main`.
5. Read the resulting URL from the workflow deployment. The site uses relative
   links and works for a repository Pages site without a hard-coded owner or
   repository name.

No DNS or custom domain is assumed. The workflow needs `pages: write` and
`id-token: write` only in its deployment job; pull requests have no deployment
permissions. Fork pull requests build documentation without deployment.

## Publish v0.1.0 deliberately

Before creating the release, confirm:

- The Apache-2.0 license and third-party notices match the submitted work and
  any distributed dependencies.
- Contributor guidance, a security-reporting route and a maintainer/review
  policy are public and actionable.
- The public repository and its reachable history contain no credentials,
  environment identities, private endpoints or confidential validation data.
- CI and documentation checks pass on the exact release commit. Review the
  limitations in the [changelog](../CHANGELOG.md) and compatibility guidance.
- The registry/image policy, vulnerability handling and artifact-signing/SBOM
  approach are agreed. Do not advertise images that have not been published.
- Repository ownership, branch protection, required checks and Pages settings
  are configured by a repository administrator.
- Upgrade, downgrade, rollback, credential renewal and allocator recovery
  procedures are understood. An experimental release does not establish a
  stable upgrade contract or an operational support SLA.

Then run **Prepare release artifacts** at the intended commit, review its
checksums and provenance, create a signed `v0.1.0` tag if a signing identity is
available, and create a **GitHub prerelease** with the changelog limitations and
reviewed source archive. Publish binary candidates only after their separate
distribution gates have been completed. Mark it as experimental. Publishing the tag or release is
a separate maintainer action and is never performed merely by building docs.

Future releases update `VERSION`, `CHANGELOG.md`, compatibility notes and any
migration instructions together. Patch releases should preserve the documented
API/ownership contract. Minor `0.x` releases may introduce explicitly documented
breaking changes.
