#!/usr/bin/env python3
"""Preserve the exact patched native production source and vendored Go licenses."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native-output", type=Path, required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    build = args.native_output.resolve()
    bundle = json.loads((build / "bundle.json").read_text())
    qualification = json.loads((build / "qualification.json").read_text())
    if qualification.get("status") != "built":
        raise SystemExit("Only a completed build can supply corresponding source")
    source = build / ("build/production-source" if bundle["target"] == "v1.16.3" else "upstream")
    environment = dict(os.environ, GOWORK="off", GOENV="off", GOFLAGS="", GOTOOLCHAIN="local")
    version = subprocess.check_output([args.go, "env", "GOVERSION"], env=environment, text=True).strip()
    if version != qualification["goVersion"]:
        raise SystemExit("Source vendoring requires the same toolchain used for the binary")
    dependencies = {name: hashlib.sha256((source / name).read_bytes()).hexdigest()
                    for name in ("go.mod", "go.sum")}
    subprocess.run([args.go, "mod", "vendor"], cwd=source, env=environment, check=True)
    if dependencies != {name: hashlib.sha256((source / name).read_bytes()).hexdigest() for name in dependencies}:
        raise SystemExit("Vendoring changed production dependency manifests")
    modifications = (
        "# Patched Kube-OVN corresponding source\n\n"
        f"Upstream target: {bundle['target']}\n\nUpstream commit: {bundle['sourceCommit']}\n\n"
        f"Patch SHA256: {bundle['patchSHA256']}\n\n"
        "Modified by Global VPC Controller contributors. The adjacent native.patch\n"
        "and source-lock.json identify the exact upstream files and changes.\n"
        "The production source contains the applied patch and project overlay.\n"
        "Vendored Go dependencies retain their original license files.\n"
        "This archive does not claim to contain all OS-package source from the base image.\n"
    ).encode()
    if args.output.exists():
        raise SystemExit("Refusing to overwrite an existing source artifact")
    with args.output.open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                def normalized(member):
                    if ".git" in Path(member.name).parts:
                        return None
                    member.uid = member.gid = member.mtime = 0
                    member.uname = member.gname = ""
                    return member
                archive.add(source, arcname="source", filter=normalized)
                for name in ("native.patch", "source-lock.json", "qualification.json", "bundle.json"):
                    archive.add(build / name, arcname=name, filter=normalized)
                info = tarfile.TarInfo("MODIFICATIONS.md")
                info.mode = 0o644
                info.size = len(modifications)
                archive.addfile(info, io.BytesIO(modifications))


if __name__ == "__main__":
    main()
