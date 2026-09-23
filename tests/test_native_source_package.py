"""Preserve modified source/licenses without silently altering locked dependencies."""

import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "scripts/package-native-source.py"


class NativeSourcePackageTest(unittest.TestCase):
    def fixture(self, root, mutate=False):
        build = root / "build-output"
        source = build / "build/production-source"
        source.mkdir(parents=True)
        (source / ".git").mkdir()
        (source / ".git/config").write_text("do not archive local Git metadata")
        (source / "go.mod").write_text("module example.com/native\ngo 1.26.6\n")
        (source / "go.sum").write_text("unchanged locks\n")
        (source / "LICENSE").write_text("original upstream license")
        (source / "changed.go").write_text("package native // applied patch\n")
        bundle = {"target": "v1.16.3", "sourceCommit": "a" * 40, "patchSHA256": "b" * 64}
        (build / "bundle.json").write_text(json.dumps(bundle))
        (build / "qualification.json").write_text(json.dumps({"status": "built", "goVersion": "go1.26.6"}))
        (build / "source-lock.json").write_text(json.dumps({"commit": "a" * 40}))
        (build / "native.patch").write_text("reviewed patch")
        go = root / "fixture-go"
        go.write_text("#!/bin/sh\nset -eu\nif [ \"$1\" = env ]; then echo go1.26.6; exit; fi\n"
                      "mkdir -p vendor/example\nprintf 'dependency license' > vendor/example/LICENSE\n"
                      + ("printf 'changed manifest' >> go.mod\n" if mutate else ""))
        go.chmod(0o755)
        output = root / "native-source.tar.gz"
        return [sys.executable, str(SCRIPT), "--native-output", str(build), "--go", str(go), "--output", str(output)], output

    def test_source_archive_retains_changed_code_locks_and_dependency_licenses(self):
        with tempfile.TemporaryDirectory() as temporary:
            command, output = self.fixture(Path(temporary))
            subprocess.run(command, check=True, capture_output=True, text=True)
            with tarfile.open(output) as archive:
                self.assertIn("source/changed.go", archive.getnames())
                self.assertIn("source/vendor/example/LICENSE", archive.getnames())
                self.assertIn("source/LICENSE", archive.getnames())
                self.assertIn("native.patch", archive.getnames())
                self.assertIn("source-lock.json", archive.getnames())
                self.assertNotIn("source/.git/config", archive.getnames())
                self.assertIn(b"does not claim", archive.extractfile("MODIFICATIONS.md").read())

    def test_dependency_mutation_stops_before_source_artifact_is_written(self):
        with tempfile.TemporaryDirectory() as temporary:
            command, output = self.fixture(Path(temporary), mutate=True)
            result = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("changed production dependency manifests", result.stderr)
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
