"""Release boundaries: deterministic archives and an explicit source allowlist."""

import importlib.util
from pathlib import Path
import tarfile
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location(
    "package_release", Path(__file__).resolve().parents[1] / "scripts/package-release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleasePackageTest(unittest.TestCase):
    def test_versions_cannot_inject_paths_or_shell_syntax(self):
        self.assertEqual(release.validate_version("0.1.0"), "0.1.0")
        for invalid in ("v0.1.0", "../0.1.0", "0.1.0;id", "00.1.0", "0.1", "0.1.0-dev", ""):
            with self.subTest(value=invalid), self.assertRaises(ValueError):
                release.validate_version(invalid)

    def test_archive_is_repeatable_and_normalizes_metadata(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            first, second = root / "first.tar.gz", root / "second.tar.gz"
            release.write_archive(first, {"runtime/main": b"program", "VERSION": b"0.1.0\n"}, ("runtime/main",))
            release.write_archive(second, {"VERSION": b"0.1.0\n", "runtime/main": b"program"}, ("runtime/main",))
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with tarfile.open(first, "r:gz") as archive:
                self.assertEqual(archive.getnames(), ["VERSION", "runtime/main"])
                for member in archive:
                    self.assertEqual((member.uid, member.gid, member.mtime), (0, 0, 0))
                self.assertEqual(archive.getmember("runtime/main").mode, 0o755)
                self.assertEqual(archive.getmember("VERSION").mode, 0o644)

    def test_archive_rejects_parent_traversal(self):
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(ValueError):
                release.write_archive(Path(temporary) / "unsafe.tar.gz", {"../secret": b"invalid"})

    def fixture(self, root):
        tracked = set(release.EXACT_INPUTS)
        for name in tracked:
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("generic input")
        return tracked

    def test_unrelated_private_files_are_not_packaged(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tracked = self.fixture(root)
            (root / "private-capture.json").write_text("excluded")
            tracked.add("private-capture.json")
            (root / "config/managed/private.yaml").write_text("excluded")
            tracked.add("config/managed/private.yaml")
            contents = release.read_inputs(root, tracked)
            self.assertNotIn("private-capture.json", contents)
            self.assertNotIn("config/managed/private.yaml", contents)
            self.assertIn("RELEASE-CONTENTS.md", contents)

    def test_untracked_installation_file_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tracked = self.fixture(root)
            tracked.remove("config/managed/site.yaml")
            with self.assertRaisesRegex(ValueError, "must be tracked"):
                release.read_inputs(root, tracked)

    def test_symlink_cannot_expand_the_allowlist(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tracked = self.fixture(root)
            path = root / "config/managed/site.yaml"
            path.unlink()
            path.symlink_to(root / "VERSION")
            tracked.add(path.relative_to(root).as_posix())
            with self.assertRaisesRegex(ValueError, "Symbolic links"):
                release.read_inputs(root, tracked)


if __name__ == "__main__":
    unittest.main()
