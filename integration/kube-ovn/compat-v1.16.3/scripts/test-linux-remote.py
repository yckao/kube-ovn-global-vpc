#!/usr/bin/env python3
"""Run approved native unit/OVSDB tests in one SSH host's private temporary directory.

This does not install a controller or connect to the host's production OVN DB.
Credentials remain in an external SSH config. Existing test evidence is preserved.
"""
import argparse
import datetime
import hashlib
import json
import pathlib
import re
import shlex
import subprocess
import sys
import time

CASES = {
    "pkg/ovs": (
        r"^TestNativeDestinationRoutes$",
        ["TestNativeDestinationRoutes"],
        [
            "TestNativeDestinationRoutes/bfd-absence-wire-and-atomic-guard",
            "TestNativeDestinationRoutes/snapshot-guard-rolls-back-mutation",
            "TestNativeDestinationRoutes/apply-idempotency-rotation-cleanup",
            "TestNativeDestinationRoutes/foreign-bfd-refused",
            "TestNativeDestinationRoutes/generated-child-collision-refused",
            "TestNativeDestinationRoutes/replacement-uid-refused",
            "TestNativeDestinationRoutes/foreign-consumer-blocks-cleanup",
        ],
    ),
    "pkg/controller": (
        r"^TestDestinationRoute(Validation|OrphanGuardGC)$",
        ["TestDestinationRouteValidation", "TestDestinationRouteOrphanGuardGC"],
        [],
    ),
}
CASES["pkg/destinationroute"] = (r".*", ["TestCanonicalHash", "TestValidation", "TestGuardExpansion", "TestReachableGateway"], [])
PROCESS_SNAPSHOT = (
    "ps -eo pid,comm | awk '$2 ~ /^(ovn-northd|ovn-controller|ovs-vswitchd|ovsdb-server)$/ {print}'"
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, help="Approved alias in external SSH config")
    parser.add_argument("--ssh-config", required=True, type=pathlib.Path)
    parser.add_argument("--build-evidence", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path, help="New result directory")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", args.host):
        parser.error("invalid SSH host alias")
    root = pathlib.Path(__file__).resolve().parents[1]
    build = json.loads(args.build_evidence.read_text())
    lock = json.loads((root / "source-lock.json").read_text())
    patch_hash = hashlib.sha256((root / "native.patch").read_bytes()).hexdigest()
    if build["patchSHA256"] != patch_hash:
        raise RuntimeError("prepared binaries do not match current native patch")
    # Compare against the source-lock's pinned commit.
    locked_commit = lock.get("sourceCommit", lock.get("commit"))
    if build["sourceCommit"] != locked_commit:
        raise RuntimeError("prepared source does not match source lock")
    fixture = build.get("testOnlyAdjustments", {}).get("fixtureAdjustment", {})
    if fixture.get("file") != "pkg/ovs/ovn-nb-suite_test.go" or "os.TempDir()" not in fixture.get("change", ""):
        raise RuntimeError("require the documented TMPDIR-isolated OVSDB test fixture")
    binaries = {item["package"]: item for item in build["binaries"]}
    if set(binaries) != set(CASES):
        raise RuntimeError("require exactly the OVS, controller and planner native test binaries")
    for item in binaries.values():
        path = pathlib.Path(item["path"])
        with path.open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if digest != item["sha256"]:
            raise RuntimeError("prepared native binary hash mismatch")

    args.output.mkdir(parents=True, exist_ok=False)
    (args.output / "build-evidence.json").write_text(json.dumps(build, indent=2) + "\n")
    ssh = ["ssh", "-F", str(args.ssh_config), "-o", "BatchMode=yes",
           "-o", "StrictHostKeyChecking=yes", args.host]

    def remote(command, *, stdin=None, timeout=150, check=True):
        result = subprocess.run(ssh + [command], stdin=stdin, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError("remote command failed with exit " + str(result.returncode))
        return result

    evidence = {
        "recordedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "sourceCommit": build["sourceCommit"], "patchSHA256": patch_hash,
        "host": args.host,
        "scope": "Linux native controller unit and isolated in-memory OVSDB transaction tests",
        "buildEvidence": "build-evidence.json",
        "tests": [], "cleanup": {"completed": False},
        "dataplane": {"tested": False},
        "sharedControllerDeployment": {"performed": False},
    }
    temp = None
    try:
        evidence["kernel"] = remote("uname -srm").stdout.decode().strip()
        evidence["uid"] = remote("id -u").stdout.decode().strip()
        evidence["productionProcessesBefore"] = remote(PROCESS_SNAPSHOT).stdout.decode().splitlines()
        if not evidence["kernel"].startswith("Linux ") or not evidence["kernel"].endswith("x86_64"):
            raise RuntimeError("prepared binaries require Linux x86_64")
        temp = remote("mktemp -d /tmp/gvpc-native-test.XXXXXXXX").stdout.decode().strip()
        if not re.fullmatch(r"/tmp/gvpc-native-test\.[A-Za-z0-9]{8}", temp):
            temp = None
            raise RuntimeError("refusing unexpected temporary directory")
        evidence["remoteTemporaryDirectory"] = temp
        for package, (pattern, top_tests, subtests) in CASES.items():
            item = binaries[package]
            name = package.split("/")[-1]
            dest = temp + "/" + name + ".test"
            with pathlib.Path(item["path"]).open("rb") as stream:
                remote("umask 077; cat > " + shlex.quote(dest), stdin=stream, timeout=180)
            actual = remote("sha256sum " + shlex.quote(dest)).stdout.decode().split()[0]
            if actual != item["sha256"]:
                raise RuntimeError("remote uploaded binary hash mismatch")
            remote("chmod 700 " + shlex.quote(dest))
            prefix = "cd " + shlex.quote(temp) + " && env TMPDIR=" + shlex.quote(temp) + " "
            listing = remote(prefix + shlex.quote(dest) + " -test.list " + shlex.quote(pattern))
            listed = set(listing.stdout.decode().splitlines())
            if not set(top_tests).issubset(listed):
                raise RuntimeError("compiled binary is missing a required native test")
            start = time.monotonic()
            result = remote(prefix + shlex.quote(dest) + " -test.run " + shlex.quote(pattern) +
                            " -test.v -test.count=1 -test.timeout=90s", check=False)
            output = (result.stdout + result.stderr).decode(errors="replace")
            log_name = name + "-linux-test.log"
            (args.output / log_name).write_text(output)
            passed = re.findall(r"(?m)^\s*--- PASS: (\S+) ", output)
            complete = set(top_tests + subtests).issubset(passed)
            success = result.returncode == 0 and complete and "\nPASS\n" in "\n" + output
            evidence["tests"].append({
                "package": package, "binarySHA256": actual, "run": pattern,
                "exitCode": result.returncode, "allExpectedTestsPassed": complete,
                "durationSeconds": round(time.monotonic() - start, 3),
                "passedTests": passed, "success": success, "log": log_name,
            })
            print(package + ": " + ("PASS" if success else "FAIL") +
                  " (" + str(len(passed)) + " test/subtest results)", flush=True)
        evidence["success"] = all(item["success"] for item in evidence["tests"])
    except Exception as error:
        evidence["success"] = False
        evidence["error"] = str(error)
        print("Native Linux test run did not complete: " + str(error), file=sys.stderr)
    finally:
        if temp is not None:
            try:
                # Exact mktemp path only. No HOME change or shared runtime cleanup.
                remote("rm -rf -- " + shlex.quote(temp))
                remote("test ! -e " + shlex.quote(temp))
                evidence["cleanup"] = {"completed": True, "absenceVerified": True}
            except Exception:
                evidence["cleanup"] = {"completed": False, "reason": "remote temporary cleanup failed"}
                evidence["success"] = False
        try:
            after = remote(PROCESS_SNAPSHOT).stdout.decode().splitlines()
            evidence["productionProcessesAfter"] = after
            evidence["productionProcessIdentityUnchanged"] = after == evidence.get("productionProcessesBefore")
        except Exception:
            evidence["productionProcessIdentityUnchanged"] = None
        (args.output / "test-evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    return 0 if evidence.get("success") and evidence["cleanup"]["completed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
