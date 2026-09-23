# Open-source readiness

Version 0.1.0 is experimental. Repository publication and a documentation website
are distinct from production readiness.

## Initial publication requirements

- Apache-2.0, attribution and third-party notices.
- A privacy-reviewed snapshot and history using synthetic examples only.
- Quick start, native prerequisites, API/architecture, transport/HA, operations,
  engineering diagrams and explicit maturity limits.
- Go/Python CI, documentation validation and Pages deployment.
- Contribution, security, support, governance and conduct policies.
- Version/release notes and reproducible source packaging.

## Repository settings

Use `main` as default. Enable private vulnerability reporting and available
secret scanning/push protection. Restrict Actions permissions and Pages deployment
to `main`. Enable dependency updates and protect against force-push/deletion.
Add required status checks after successful first runs, taking the current
maintainer/review model into account.

Verify the tree and package before tagging. Never push unrelated local branches,
mirror private development history, or include runtime artifacts, credentials
or deployment-specific evidence in release uploads.

## Before production-oriented binary or container distribution

- Inventory actual bundled dependencies and licenses, including FRRouting and
  OS packages; preserve notices and satisfy source distribution obligations.
- Generate SPDX/CycloneDX SBOMs and scan pinned dependencies/images. Review
  findings rather than treating a scan as certification.
- Provide signatures/build provenance, immutable digests, checksums, supported
  OS/CPU combinations and native-controller compatibility.
- Exercise public install/upgrade/rollback instructions, replacement, identity
  renewal, recovery and tenant isolation.
- Publish reproducible sanitized dataplane/scale/hardware methodology and results;
  do not infer throughput from NIC sums or site capacity from API-only tests.

## Community follow-ups

Appoint a backup maintainer, establish a dedicated private conduct-reporting
contact, and define a support/version policy. Track engineering work in the
[roadmap](../ROADMAP.md).
