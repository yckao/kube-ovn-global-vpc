#!/usr/bin/env python3
"""Apply the native extension only to the verified, unmodified source baseline."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("source", type=Path, help="Unmodified Kube-OVN source directory")
parser.add_argument("--check", action="store_true", help="Verify applicability without edits")
args = parser.parse_args()
base = Path(__file__).resolve().parents[1]
source = args.source.resolve()
lock = json.loads((base / "source-lock.json").read_text())
for name, expected in lock["files"].items():
    path = source / name
    if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
        raise SystemExit(f"Refusing source mismatch: {name}; expected {lock['commit']}")
if (source / ".git").exists():
    commit = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
    if commit != lock["commit"]:
        raise SystemExit("Refusing Git commit mismatch")
patch = str(base / "native.patch")
subprocess.run(["git", "apply", "--check", patch], cwd=source, check=True)
if not args.check:
    subprocess.run(["git", "apply", patch], cwd=source, check=True)
print("Patch applicability verified" if args.check else "Native patch applied; build and test before deployment")
