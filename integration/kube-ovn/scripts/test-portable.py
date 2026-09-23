#!/usr/bin/env python3
"""Run the native OVSDB tests locally on Darwin in a disposable source copy.

The production patch is Linux-targeted. This supplementary test replaces the
unavailable Darwin unix.ETH_P_IPV6 constant with its identical 0x86dd value and
retains only the original upstream in-memory NB fixtures plus extension tests.
It does not validate Linux netlink/NDP or the complete native controller runtime.
"""
import argparse
import json
from pathlib import Path
import os
import platform
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("source", type=Path)
parser.add_argument("--go", default="go")
parser.add_argument("--full", action="store_true", help="Linux only: also run planner/controller tests and controller build")
args = parser.parse_args()
if args.full and platform.system() != "Linux":
    raise SystemExit("Full native tests require Linux")
with tempfile.TemporaryDirectory(prefix="gvpc-native-portable-") as directory:
    copy = Path(directory) / "source"
    shutil.copytree(args.source, copy, ignore=shutil.ignore_patterns(".git", "vendor", "*.test"))
    # Keep the in-memory test server's socket inside the caller's TMPDIR.
    fixture_path = copy / "pkg/ovs/ovn-nb-suite_test.go"
    fixture_source = fixture_path.read_text()
    socket_expression = 'fmt.Sprintf("/tmp/ovsdb-%s.sock", name)'
    if fixture_source.count(socket_expression) != 1:
        raise SystemExit("Refusing unexpected upstream OVSDB socket fixture")
    fixture_path.write_text(fixture_source.replace(
        socket_expression, 'fmt.Sprintf("%s/ovsdb-%s.sock", os.TempDir(), name)'))
    if platform.system() == "Darwin":
        path = copy / "pkg/util/ndp.go"
        source = path.read_text()
        if source.count("unix.ETH_P_IPV6") != 3:
            raise SystemExit("Refusing unexpected upstream NDP source")
        path.write_text(source.replace("unix.ETH_P_IPV6", "0x86dd"))
    ovs = copy / "pkg/ovs"
    # This pinned in-memory test server compares set-valued wait columns as
    # ordered Go slices. Correct that test-only server behavior to RFC7047 sets;
    # the production OVN server and libovsdb client are not modified.
    metadata = json.loads(subprocess.check_output([args.go, "list", "-m", "-json", "github.com/ovn-kubernetes/libovsdb"], cwd=copy, text=True))
    dependency = metadata["Replace"]
    if dependency["Version"] != "v0.0.0-20260902074400-457a5d8cc172":
        raise SystemExit("Refusing unexpected libovsdb test-server version")
    testlib = Path(directory) / "test-libovsdb"
    shutil.copytree(dependency["Dir"], testlib)
    wait = testlib / "database/transaction/transaction.go"
    original = wait.read_text()
    old = "if !reflect.DeepEqual(x, y) {"
    if original.count(old) != 1:
        raise SystemExit("Refusing unexpected test-server wait implementation")
    fixed = original.replace(old, """equal := reflect.DeepEqual(x,y)
                    if columnSchema.Type == ovsdb.TypeSet {
                        forward,_ := ovsdb.ConditionIncludes.Evaluate(x,y)
                        reverse,_ := ovsdb.ConditionIncludes.Evaluate(y,x)
                        equal = forward && reverse
                    }
                    if !equal {""")
    fixed = fixed.replace("if ovsdb.IsDefaultValue(columnSchema, x) {", "if _, present := r[column]; !present {")
    fixed = fixed.replace("dbModel.Mapper.GetRowData(&r, i)", "dbModel.Mapper.GetRowDataWithUUID(&r, i)")
    wait.chmod(0o644)
    wait.write_text(fixed)
    subprocess.run([args.go, "mod", "edit", "-replace", "github.com/ovn-kubernetes/libovsdb=" + str(testlib)], cwd=copy, check=True)
    if platform.system() == "Darwin":
        fixture = (ovs / "ovn-nb-suite_test.go").read_text()
        functions = fixture[fixture.index("func newOVSDBServer("):fixture.index("func newOvnSbClient(")]
        imports = fixture[:fixture.index("type OvnClientTestSuite")]
        imports = imports.replace('"github.com/stretchr/testify/suite"', "").replace('"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"', "")
        for path in ovs.glob("*_test.go"):
            if path.name != "ovn-nb-destination-route_test.go":
                path.unlink()
        (ovs / "native_portable_fixture_test.go").write_text(imports + functions)
    commands = [[args.go, "test", "./pkg/ovs", "-run", "^TestNativeDestinationRoutes$", "-count=1", "-v"]]
    if args.full:
        commands += [[args.go, "test", "./pkg/destinationroute", "-count=1"],
                     [args.go, "test", "./pkg/controller", "-run", "^TestDestinationRoute(Validation|OrphanGuardGC)$", "-count=1"],
                     [args.go, "build", "./cmd/controller"]]
    for command in commands:
        result = subprocess.run(command, cwd=copy, env=os.environ)
        if result.returncode:
            raise SystemExit(result.returncode)
