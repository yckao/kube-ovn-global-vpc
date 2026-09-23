#!/usr/bin/env python3
"""Assemble a release descriptor only from qualified build outputs, never tags."""

import argparse
import hashlib
import json
from pathlib import Path
import re


API_VERSION = "vpcctl.globalvpc.io/v1alpha1"
TARGETS = {
    "v1.16.3": ("98af25ffae49193a8dc16bbc39bd8ca4110ec367", "go1.26.6"),
    "v1.16.4": ("a9296ef2a37c6519bc0ecb798082ce139c72f8eb", "go1.27.1"),
}
PATCH_SHA = "d338f05a6c4b7fa538637ee78a155a4ab2361d3b2e67ef665f359feaad5d9912"
DIGEST_IMAGE = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$")


def require_image(image):
    if not DIGEST_IMAGE.fullmatch(image):
        raise ValueError("Every image must use an immutable NAME@sha256:<64 lowercase hex> reference")
    return image


def require_version(version):
    if not re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", version):
        raise ValueError("Version must be vMAJOR.MINOR.PATCH")
    return version


def native_bundle(bundle, qualification):
    target = bundle.get("target")
    if target not in TARGETS:
        raise ValueError("Native target is not a reviewed source lock")
    commit, toolchain = TARGETS[target]
    if (bundle.get("apiVersion") != API_VERSION or bundle.get("kind") != "NativeBundle"
            or bundle.get("sourceCommit") != commit or bundle.get("patchSHA256") != PATCH_SHA
            or bundle.get("platform") != "linux/amd64"
            or not re.fullmatch(r"[a-f0-9]{64}", bundle.get("binarySHA256", ""))):
        raise ValueError("Native bundle does not match the reviewed build contract")
    require_image(bundle.get("image", ""))
    require_image(bundle.get("baseImage", ""))
    if set(bundle.get("schema", {})) != {"spec", "status"}:
        raise ValueError("Native bundle must contain both destinationRoutes schema properties")
    if (bundle.get("qualification", {}).get("nativeUnitTests") != "passed"
            or qualification.get("status") != "built"
            or qualification.get("nativeUnitTests") != "passed"
            or qualification.get("sourceCommit") != commit
            or qualification.get("goVersion") != toolchain
            or qualification.get("image") != bundle["image"]
            or qualification.get("binarySHA256") != bundle["binarySHA256"]):
        raise ValueError("Native build qualification is missing or inconsistent")
    return bundle


def make_descriptor(version, commit, controller, gateway, bundles, cli_image=None, charts=None):
    require_version(version)
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise ValueError("Source commit must be the complete Git SHA")
    if set(bundles) != set(TARGETS):
        raise ValueError("Release must contain exactly both reviewed native targets")
    descriptor = {"apiVersion": API_VERSION, "kind": "Release", "version": version,
            "sourceCommit": commit, "controllerImage": require_image(controller),
            "gatewayImage": require_image(gateway), "nativeBundles": bundles}
    if cli_image:
        descriptor["cliImage"] = require_image(cli_image)
    if charts:
        descriptor["charts"] = charts
    return descriptor


def write_checksums(directory):
    files = sorted(path for path in directory.iterdir() if path.name != "SHA256SUMS")
    if any(not path.is_file() or path.is_symlink() or "\n" in path.name or "\r" in path.name for path in files):
        raise ValueError("Release assets must be plain files with safe names")
    (directory / "SHA256SUMS").write_text("".join(
        f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in files))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--controller-image", required=True)
    parser.add_argument("--gateway-image", required=True)
    parser.add_argument("--cli-image", required=True)
    parser.add_argument("--repository", required=True, help="GitHub OWNER/REPO for chart release asset URLs")
    parser.add_argument("--native-output", action="append", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args()
    bundles = {}
    for output in args.native_output:
        bundle = native_bundle(json.loads((output / "bundle.json").read_text()),
                               json.loads((output / "qualification.json").read_text()))
        if bundle["target"] in bundles:
            raise ValueError("Duplicate native bundle target")
        bundles[bundle["target"]] = bundle
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repository):
        raise ValueError("GitHub repository must be OWNER/REPO")
    charts = {}
    for name in ("global-vpc", "global-vpc-site", "kube-ovn-global-vpc-extension"):
        filename = f"{name}-{args.version.removeprefix('v')}.tgz"
        if not (args.artifacts / filename).is_file():
            raise ValueError(f"Release is missing required Helm chart: {name}")
        charts[name] = f"https://github.com/{args.repository}/releases/download/{args.version}/{filename}"
    descriptor = make_descriptor(args.version, args.source_commit,
                                 args.controller_image, args.gateway_image, bundles, args.cli_image, charts)
    if (args.artifacts / "release.json").exists():
        raise ValueError("Refusing to replace an existing release descriptor")
    version = args.version.removeprefix("v")
    for platform in ("linux", "darwin"):
        for arch in ("amd64", "arm64"):
            if not (args.artifacts / f"vpcctl_{version}_{platform}_{arch}.tar.gz").is_file():
                raise ValueError("Release is missing a prebuilt CLI platform archive")
    for required in (f"kube-ovn-global-vpc_{version}_source.tar.gz",
                     "project-vendored-source.tar.gz", "native-v1.16.3-source.tar.gz",
                     "native-v1.16.4-source.tar.gz", "image-evidence.tar.gz", "provenance.json"):
        if not (args.artifacts / required).is_file():
            raise ValueError(f"Release is missing source or provenance material: {required}")
    (args.artifacts / "release.json").write_text(json.dumps(descriptor, indent=2, sort_keys=True) + "\n")
    write_checksums(args.artifacts)
    print(f"Prepared {args.version} with five immutable images, Helm charts and checksums")


if __name__ == "__main__":
    main()
