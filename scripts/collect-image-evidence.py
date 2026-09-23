#!/usr/bin/env python3
"""Read pushed OCI digests, attestations and native qualification for release assets."""

import argparse
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess


SPEC = importlib.util.spec_from_file_location("assemble_release", Path(__file__).with_name("assemble-release.py"))
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


def image_from_metadata(repository, metadata):
    digest = metadata.get("containerimage.digest", "")
    return release.require_image(repository + "@" + digest)


def require_attestations(index, sbom, provenance):
    if not any(manifest.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest"
               for manifest in index.get("manifests", [])):
        raise ValueError("Pushed image has no OCI attestation manifest")
    # Buildx exposes one SPDX document and SLSA provenance per runtime platform.
    # Preserve these verbatim: they are evidence from the build, not our claims.
    if not isinstance(sbom, dict) or not (sbom.get("SPDX") or any(isinstance(item, dict) and item.get("SPDX") for item in sbom.values())):
        raise ValueError("Pushed image has no retrievable SPDX SBOM")
    if not isinstance(provenance, dict) or not (provenance.get("SLSA") or any(isinstance(item, dict) and item.get("SLSA") for item in provenance.values())):
        raise ValueError("Pushed image has no retrievable SLSA provenance")


def inspect_image(image, directory, name):
    base = ["docker", "buildx", "imagetools", "inspect", image]
    index = json.loads(subprocess.check_output([*base, "--raw"], text=True))
    sbom = json.loads(subprocess.check_output([*base, "--format", "{{json .SBOM}}"], text=True))
    provenance = json.loads(subprocess.check_output([*base, "--format", "{{json .Provenance}}"], text=True))
    require_attestations(index, sbom, provenance)
    for suffix, data in (("index", index), ("sbom", sbom), ("provenance", provenance)):
        (directory / f"{name}-{suffix}.json").write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def verify_runtime(image, name, version, commit):
    command = ["docker", "run", "--rm", "--platform", "linux/amd64", "--network", "none"]
    if name == "gateway":
        # Inspect installed executables without starting routing daemons or
        # configuring any host network. Cluster dataplane qualification is separate.
        script = "set -eu; python3 --version; wg --version; ip -Version; test -x /usr/lib/frr/bgpd; test -x /usr/lib/frr/bfdd; command -v nft; command -v nsenter"
        return subprocess.check_output([*command, "--entrypoint", "/bin/sh", image, "-c", script], text=True)
    binary = "/vpcctl" if name == "cli" else "/usr/local/bin/platform-vpc-controller"
    output = subprocess.check_output([*command, "--entrypoint", binary, image, "--version"], text=True)
    program = "vpcctl" if name == "cli" else "platform-vpc-controller"
    if not output.startswith(f"{program} {version} (commit={commit},"):
        raise ValueError(f"Published {name} image does not contain the versioned release binary")
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--image-prefix", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--native-output", action="append", type=Path, required=True)
    args = parser.parse_args()
    for name, image_name in (("controller", "platform-vpc-controller"), ("gateway", "gateway"), ("cli", "vpcctl")):
        metadata = json.loads((args.evidence / f"{name}-metadata.json").read_text())
        image = image_from_metadata(args.image_prefix + "/" + image_name, metadata)
        inspect_image(image, args.evidence, name)
        runtime = verify_runtime(image, name, args.version, args.source_commit)
        (args.evidence / f"{name}-runtime-check.txt").write_text(runtime)
        (args.evidence / f"{name}-image.txt").write_text(image + "\n")
    for output in args.native_output:
        bundle = release.native_bundle(json.loads((output / "bundle.json").read_text()),
                                       json.loads((output / "qualification.json").read_text()))
        name = "native-" + bundle["target"]
        inspect_image(bundle["image"], args.evidence, name)
        for filename in ("bundle.json", "qualification.json", "source-lock.json", "native.patch"):
            shutil.copyfile(output / filename, args.evidence / f"{name}-{filename}")


if __name__ == "__main__":
    main()
