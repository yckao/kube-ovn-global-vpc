#!/usr/bin/env python3
"""Build versioned controller binaries and an explicit managed-installation bundle."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
EXACT_INPUTS = (
    "VERSION", "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "CHANGELOG.md",
    "docs/managed-quickstart.md",
    "config/crd/platform.globalvpc.io_vpcs.yaml",
    "config/crd/platform.globalvpc.io_subnets.yaml",
    "config/crd/platform.globalvpc.io_networkbindings.yaml",
    "config/managed/authority.yaml", "config/managed/project-role.yaml",
    "config/managed/location-access.yaml", "config/managed/site.yaml",
    "config/examples/managed/smoke-pod.yaml", "config/examples/managed/network.yaml",
    "config/examples/managed/site-a.json", "config/examples/managed/site-b.json",
    "config/examples/managed/platform.json",
    "gateway/Dockerfile", "gateway/gateway.py", "gateway/managed.py",
    "gateway/overlay.py", "gateway/evpn.py",
    "integration/kube-ovn/source-lock.json",
    "integration/kube-ovn/native.patch",
    "integration/kube-ovn/scripts/apply.py",
    "integration/kube-ovn/README.md", "integration/kube-ovn/example-vpc.yaml",
    "integration/kube-ovn/scripts/test.sh", "integration/kube-ovn/scripts/test-portable.py",
    "integration/kube-ovn/destinationroute/plan.go",
    "integration/kube-ovn/destinationroute/plan_test.go",
    "integration/kube-ovn/overlay/go.mod",
    "integration/kube-ovn/overlay/pkg/apis/kubeovn/v1/destination_routes.go",
    "integration/kube-ovn/overlay/pkg/controller/vpc_destination_routes.go",
    "integration/kube-ovn/overlay/pkg/controller/vpc_destination_routes_test.go",
    "integration/kube-ovn/overlay/pkg/ovs/ovn-nb-destination-route.go",
    "integration/kube-ovn/overlay/pkg/ovs/ovn-nb-destination-route_test.go",
)
INSTALLATION_README = """# Managed installation input bundle

This archive contains templates and runtime/extension inputs for this release.
It is not an installer, a container image or the complete source distribution.
The adjacent source archive contains the full reviewed source and documentation.
Use docs/managed-quickstart.md and the full documentation in that source archive
at the commit recorded in provenance.json. Reserve pools, configure identities and replace image digest
placeholders before applying resources. Do not apply every YAML file blindly.

The Kube-OVN patch requires exactly the source in its source-lock.json. The
controller binary from the adjacent archive does not replace native Kube-OVN.
Build and validate the patched native controller separately. Gateway runtime
files are delivered by the controller; gateway/Dockerfile provides its Linux
runtime dependencies and is not independently a fully configured appliance.

This release is experimental. See CHANGELOG.md for its capability boundaries.
Review LICENSE and NOTICE before redistribution, including container images.
"""


def digest(data):
    return hashlib.sha256(data).hexdigest()


def run(args, **kwargs):
    return subprocess.check_output(args, cwd=ROOT, text=True, **kwargs).strip()


def validate_version(value):
    if not re.fullmatch(r"0|[1-9]\d*", value.split(".")[0]):
        raise ValueError("VERSION must be a numeric semantic version without a v prefix")
    if not re.fullmatch(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", value):
        raise ValueError("VERSION must be a numeric semantic version without a v prefix")
    return value


def read_inputs(root, tracked):
    result = {}
    for name in sorted(EXACT_INPUTS):
        path = root / name
        if name not in tracked:
            raise ValueError(f"Package input must be tracked: {name}")
        if path.is_symlink() or any((root / parent).is_symlink() for parent in Path(name).parents):
            raise ValueError(f"Symbolic links are not package inputs: {name}")
        if not path.is_file():
            raise ValueError(f"Missing package input: {name}")
        result[name] = path.read_bytes()
    result["RELEASE-CONTENTS.md"] = INSTALLATION_README.encode()
    return result


def write_archive(path, contents, executable=()):
    """Write deterministic archives without filesystem ownership or timestamps."""
    with path.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for name, data in sorted(contents.items()):
                    if name.startswith("/") or ".." in Path(name).parts:
                        raise ValueError(f"Unsafe archive member: {name}")
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    info.mode = 0o755 if name in executable else 0o644
                    info.mtime = 0
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    archive.addfile(info, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=ROOT / "dist",
                        help="Output parent directory; must be outside tracked source")
    args = parser.parse_args()
    version = validate_version((ROOT / "VERSION").read_text().strip())
    if run(["git", "status", "--porcelain", "--untracked-files=all"]):
        raise SystemExit("Refusing release packaging from a dirty or untracked working tree")
    commit = run(["git", "rev-parse", "HEAD"])
    source_date = run(["git", "show", "-s", "--format=%cI", "HEAD"])
    tracked = set(run(["git", "ls-files"]).splitlines())
    contents = read_inputs(ROOT, tracked)
    output = args.output.resolve() / f"v{version}"
    if output.exists():
        raise SystemExit(f"Refusing to overwrite existing release directory: {output}")
    if output.is_relative_to(ROOT):
        ignored = subprocess.run(["git", "check-ignore", "--quiet", str(output / "SHA256SUMS")], cwd=ROOT)
        if ignored.returncode != 0:
            raise SystemExit("Release output inside the checkout must be ignored by Git")
    # Do not let an ambient workspace, persistent Go setting or GOFLAGS overlay
    # silently change the source/build contract represented by this provenance.
    fixed_environment = {"CGO_ENABLED": "0", "GOOS": "linux", "GOFLAGS": "",
                         "GOWORK": "off", "GOENV": "off", "GOTOOLCHAIN": "local"}
    build_environment = dict(os.environ, **fixed_environment)
    metadata = {
        "schemaVersion": 1,
        "version": f"v{version}",
        "sourceCommit": commit,
        "sourceDate": source_date,
        "sourceTreeClean": True,
        "goVersion": run(["go", "version"], env=build_environment),
        "goModuleVersion": json.loads(run(["go", "list", "-m", "-json"], env=build_environment))["GoVersion"],
        "buildFlags": ["-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags",
                       f"-X main.version=v{version} -X main.commit={commit} -X main.sourceDate={source_date}"],
        "environment": fixed_environment,
        "architectures": ["amd64", "arm64"],
        "installationInputSHA256": {name: digest(data) for name, data in contents.items()},
        "dependencyManifestSHA256": {name: digest((ROOT / name).read_bytes())
                                     for name in ("go.mod", "go.sum")},
        "goModules": json.loads(run(["go", "list", "-m", "-json"], env=build_environment)),
        "sourceLocks": {
            "kubeOVN": json.loads(contents["integration/kube-ovn/source-lock.json"]),
        },
        "scope": "Unsigned build metadata. Compilation is not runtime or compatibility qualification.",
    }
    # Avoid embedding host paths from go list metadata.
    metadata["goModules"] = {key: metadata["goModules"][key]
                             for key in ("Path", "Version", "GoVersion")
                             if key in metadata["goModules"]}
    with tempfile.TemporaryDirectory(prefix="global-vpc-release-") as temporary:
        stage = Path(temporary)
        source_tar = subprocess.check_output([
            "git", "archive", "--format=tar", f"--prefix=kube-ovn-global-vpc-{version}/", commit,
        ], cwd=ROOT)
        source_archive = stage / f"kube-ovn-global-vpc_{version}_source.tar.gz"
        with source_archive.open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                compressed.write(source_tar)
        for arch in metadata["architectures"]:
            binary = stage / f"controller-{arch}"
            env = dict(build_environment, GOARCH=arch)
            subprocess.run(["go", "build", *metadata["buildFlags"], "-o", str(binary),
                            "./cmd/platform-vpc-controller"], cwd=ROOT, env=env, check=True)
            name = f"platform-vpc-controller_{version}_linux_{arch}.tar.gz"
            write_archive(stage / name, {
                "platform-vpc-controller": binary.read_bytes(),
                "VERSION": contents["VERSION"], "LICENSE": contents["LICENSE"],
                "NOTICE": contents["NOTICE"],
                "THIRD_PARTY_NOTICES.md": contents["THIRD_PARTY_NOTICES.md"],
            }, executable=("platform-vpc-controller",))
            binary.unlink()
        write_archive(stage / f"managed-installation_{version}.tar.gz", contents,
                      executable=("integration/kube-ovn/scripts/test.sh",))
        # Check again after build so recorded provenance cannot silently describe
        # files changed by another process during packaging.
        if run(["git", "status", "--porcelain", "--untracked-files=all"]) or commit != run(["git", "rev-parse", "HEAD"]):
            raise SystemExit("Source changed while packaging; no artifacts published")
        metadata["artifactSHA256"] = {path.name: digest(path.read_bytes())
                                      for path in sorted(stage.iterdir())}
        (stage / "provenance.json").write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n")
        checksums = "".join(f"{digest(path.read_bytes())}  {path.name}\n"
                            for path in sorted(stage.iterdir()))
        (stage / "SHA256SUMS").write_text(checksums)
        output.mkdir(parents=True)
        for path in stage.iterdir():
            (output / path.name).write_bytes(path.read_bytes())
    print(f"Prepared v{version} from {commit}: {output}")
    print("Artifacts are unsigned. No tag, GitHub Release or container image was published.")


if __name__ == "__main__":
    main()
