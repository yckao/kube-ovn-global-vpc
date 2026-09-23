#!/usr/bin/env python3
"""Build the exact deployed controller source plus the additive native extension.

The production executable retains the original dependency graph. Test binaries
use a separate source/dependency copy with documented test-server corrections.
No cluster access or deployment is performed.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("source", type=Path, help="Unmodified exact 98af25f source")
parser.add_argument("--go", required=True, type=Path, help="Go 1.26.6 executable")
parser.add_argument("--output", required=True, type=Path, help="New build directory")
args = parser.parse_args()
base = Path(__file__).resolve().parents[1]
lock = json.loads((base / "source-lock.json").read_text())
def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

go = str(args.go.resolve())
version = subprocess.check_output([go, "version"], text=True).strip()
if " go1.26.6 " not in version:
    raise SystemExit("The compatibility build requires the deployed Go 1.26.6 toolchain")
output = args.output.resolve()
output.mkdir(parents=True, exist_ok=False)
source = output / "production-source"
shutil.copytree(args.source, source, ignore=shutil.ignore_patterns(".git", "*.test", "vendor"))
subprocess.run(["python3", str(base / "scripts/apply.py"), str(source)], check=True)
patch_sha = sha(base / "native.patch")
original_mod_sha = sha(source / "go.mod")
original_sum_sha = sha(source / "go.sum")
env = dict(os.environ, GOTOOLCHAIN="local", CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
metadata = json.loads(subprocess.check_output([go, "list", "-m", "-json", "github.com/ovn-kubernetes/libovsdb"], cwd=source, env=env, text=True))
dependency = metadata["Replace"]
if dependency["Version"] != "v0.0.0-20251212071713-cb1c2bc5d43e":
    raise SystemExit("Refusing a changed production libovsdb dependency")
# Match the deployed production build options, adding an explicit candidate ID.
now = datetime.datetime.now(datetime.timezone.utc)
release = "v1.16.3-globalvpc.v1"
commit = "git-98af25f+globalvpc-" + patch_sha[:12]
ldflags = "-w -s -extldflags '-z now' -X github.com/kubeovn/kube-ovn/versions.COMMIT=" + commit + " -X github.com/kubeovn/kube-ovn/versions.VERSION=" + release + " -X github.com/kubeovn/kube-ovn/versions.BUILDDATE=" + now.strftime("%Y-%m-%d_%H:%M:%S")
production_command = [go, "build", "-mod=readonly", "-trimpath", "-buildmode=pie", "-ldflags", ldflags, "-o", str(output / "kube-ovn-controller"), "./cmd/controller"]
production_log = (output / "controller-build.log").open("w")
production = subprocess.Popen(production_command, cwd=source, env=env, stdout=production_log, stderr=subprocess.STDOUT)
# Do not mutate production sources or the shared module cache for fixture fixes.
test_source = output / "test-source"
shutil.copytree(source, test_source)
testlib = output / "test-libovsdb"
shutil.copytree(dependency["Dir"], testlib)
transaction = testlib / "database/transaction/transaction.go"
original_transaction = transaction.read_text()
fixed = original_transaction
corrections = [
    ("if !reflect.DeepEqual(x, y) {", """equal := reflect.DeepEqual(x,y)
                    if columnSchema.Type == ovsdb.TypeSet {
                        forward,_ := ovsdb.ConditionIncludes.Evaluate(x,y)
                        reverse,_ := ovsdb.ConditionIncludes.Evaluate(y,x)
                        equal = forward && reverse
                    }
                    if !equal {"""),
    ("if ovsdb.IsDefaultValue(columnSchema, x) {", "if _, present := r[column]; !present {"),
    ("dbModel.Mapper.GetRowData(&r, i)", "dbModel.Mapper.GetRowDataWithUUID(&r, i)"),
]
for before, after in corrections:
    if fixed.count(before) != 1:
        raise SystemExit("Unexpected pinned test-server implementation")
    fixed = fixed.replace(before, after)
transaction.chmod(0o644)
transaction.write_text(fixed)
fixture = test_source / "pkg/ovs/ovn-nb-suite_test.go"
original_fixture = fixture.read_text()
old_socket = 'tmpfile := fmt.Sprintf("/tmp/ovsdb-%s.sock", name)'
if original_fixture.count(old_socket) != 1:
    raise SystemExit("Unexpected upstream OVSDB fixture socket")
fixture.write_text(original_fixture.replace(old_socket, 'tmpfile := fmt.Sprintf("%s/ovsdb-%s.sock", os.TempDir(), name)'))
subprocess.run([go, "mod", "edit", "-replace", "github.com/ovn-kubernetes/libovsdb=" + str(testlib)], cwd=test_source, env=env, check=True)
test_dependency = json.loads(subprocess.check_output([go, "list", "-m", "-json", "github.com/ovn-kubernetes/libovsdb"], cwd=test_source, env=env, text=True))
assert test_dependency["Replace"]["Dir"] == str(testlib)
processes = []
for package, pattern in [("pkg/ovs", "^TestNativeDestinationRoutes$"), ("pkg/controller", "^TestDestinationRoute(Validation|OrphanGuardGC)$"), ("pkg/destinationroute", ".*")]:
    name = package.split("/")[-1]
    binary = output / (name + ".test")
    log = output / (name + "-test-build.log")
    command = [go, "test", "-mod=readonly", "-c", "-o", str(binary), "./" + package]
    handle = log.open("w")
    process = subprocess.Popen(command, cwd=test_source, env=env, stdout=handle, stderr=subprocess.STDOUT)
    processes.append((process, handle, package, pattern, binary, log))
status = production.wait()
production_log.close()
if status:
    print((output / "controller-build.log").read_text()[-6000:])
    raise SystemExit(status)
print("Production controller built", flush=True)
record = {"sourceCommit": lock["commit"], "sourceArchiveSHA256": lock["sourceArchiveSHA256"], "patchSHA256": patch_sha, "recordedAt": now.isoformat(), "go": version, "target": "linux/amd64", "originalGoModSHA256": original_mod_sha, "originalGoSumSHA256": original_sum_sha, "productionDependency": dependency["Path"] + "@" + dependency["Version"], "candidateVersion": release, "candidateCommit": commit, "binaries": [], "productionBuildCommand": production_command, "testOnlyAdjustments": ["In-memory wait operations: unordered set equality, explicit empty values and UUID decoding", "Upstream test socket honors TMPDIR"], "darwinSourceChanges": False, "sharedModuleCacheModified": False, "runtimeTestsExecuted": False, "deployed": False}
for process, handle, package, pattern, binary, log in processes:
    status = process.wait()
    handle.close()
    if status:
        print(log.read_text()[-6000:])
        raise SystemExit(status)
    build_info = subprocess.check_output([go, "version", "-m", str(binary)], text=True)
    # Go 1.26 omits dependency entries from test-binary build metadata. Record
    # the verified build input resolution and hashes instead of inventing them.
    assert "GOOS=linux" in build_info and "GOARCH=amd64" in build_info
    record["binaries"].append({"package": package, "path": str(binary), "sha256": sha(binary), "testPattern": pattern})
    print(package, "test binary built", flush=True)
production_info = subprocess.check_output([go, "version", "-m", str(output / "kube-ovn-controller")], text=True)
assert dependency["Version"] in production_info and str(testlib) not in production_info
assert original_mod_sha == sha(source / "go.mod") and original_sum_sha == sha(source / "go.sum")
record["productionBinary"] = {"path": str(output / "kube-ovn-controller"), "sha256": sha(output / "kube-ovn-controller"), "buildInfo": production_info}
record["testDependencyResolution"] = test_dependency
record["testFixtures"] = {"originalTransactionSHA256": hashlib.sha256(original_transaction.encode()).hexdigest(), "testTransactionSHA256": sha(transaction), "originalSocketFixtureSHA256": hashlib.sha256(original_fixture.encode()).hexdigest(), "testSocketFixtureSHA256": sha(fixture)}
record["testOnlyAdjustments"] = {"changes": record["testOnlyAdjustments"], "fixtureAdjustment": {"file": "pkg/ovs/ovn-nb-suite_test.go", "change": "Use os.TempDir() for isolated in-memory OVSDB socket", "originalSHA256": record["testFixtures"]["originalSocketFixtureSHA256"], "testSHA256": record["testFixtures"]["testSocketFixtureSHA256"]}, "productionSourceChanges": False, "moduleCacheModified": False}
(output / "build-evidence.json").write_text(json.dumps(record, indent=2) + "\n")
print("Evidence:", output / "build-evidence.json", flush=True)
