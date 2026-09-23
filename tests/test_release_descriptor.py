"""Reject mutable or unqualified artifacts before generating a release pointer."""

import importlib.util
from pathlib import Path
import tempfile
import unittest
import shutil
import json
import subprocess
import sys
import tarfile
from unittest import mock


SPEC = importlib.util.spec_from_file_location(
    "assemble_release", Path(__file__).resolve().parents[1] / "scripts/assemble-release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)
EVIDENCE_SPEC = importlib.util.spec_from_file_location(
    "collect_image_evidence", Path(__file__).resolve().parents[1] / "scripts/collect-image-evidence.py")
evidence = importlib.util.module_from_spec(EVIDENCE_SPEC)
EVIDENCE_SPEC.loader.exec_module(evidence)
CHART_SPEC = importlib.util.spec_from_file_location(
    "package_helm_release", Path(__file__).resolve().parents[1] / "scripts/package-helm-release.py")
charts = importlib.util.module_from_spec(CHART_SPEC)
CHART_SPEC.loader.exec_module(charts)
IMAGE = "ghcr.io/example/image@sha256:" + "a" * 64


class ReleaseDescriptorTest(unittest.TestCase):
    def fixture(self, target="v1.16.3"):
        commit, go = release.TARGETS[target]
        bundle = {"apiVersion": release.API_VERSION, "kind": "NativeBundle", "target": target,
                  "sourceCommit": commit, "patchSHA256": release.PATCH_SHA,
                  "platform": "linux/amd64", "image": IMAGE, "baseImage": IMAGE,
                  "binarySHA256": "b" * 64, "schema": {"spec": {}, "status": {}},
                  "qualification": {"nativeUnitTests": "passed"}}
        qualification = {"status": "built", "nativeUnitTests": "passed", "goVersion": go,
                         "sourceCommit": commit, "image": IMAGE, "binarySHA256": "b" * 64}
        return bundle, qualification

    def test_both_native_targets_require_exact_source_and_toolchain(self):
        for target in release.TARGETS:
            bundle, qualification = self.fixture(target)
            self.assertEqual(release.native_bundle(bundle, qualification), bundle)
            qualification["goVersion"] = "go1.26.5"
            with self.assertRaises(ValueError):
                release.native_bundle(bundle, qualification)

    def test_mutable_tags_and_unqualified_images_are_rejected(self):
        for image in ("image:latest", IMAGE + "\n", "image@sha256:abc", "$(id)@sha256:" + "a" * 64):
            with self.subTest(image=image), self.assertRaises(ValueError):
                release.require_image(image)
        bundle, qualification = self.fixture()
        bundle["qualification"]["nativeUnitTests"] = "not-run"
        with self.assertRaises(ValueError):
            release.native_bundle(bundle, qualification)

    def test_descriptor_requires_complete_native_matrix(self):
        bundles = {target: self.fixture(target)[0] for target in release.TARGETS}
        descriptor = release.make_descriptor("v0.1.0", "c" * 40, IMAGE, IMAGE, bundles)
        self.assertEqual(descriptor["kind"], "Release")
        self.assertEqual(descriptor["nativeBundles"], bundles)
        bundles.pop("v1.16.4")
        with self.assertRaises(ValueError):
            release.make_descriptor("v0.1.0", "c" * 40, IMAGE, IMAGE, bundles)

    def test_checksums_cover_descriptor_and_assets_but_not_themselves(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "release.json").write_text("{}\n")
            (root / "binary.tar.gz").write_bytes(b"binary")
            release.write_checksums(root)
            first = (root / "SHA256SUMS").read_text()
            release.write_checksums(root)
            self.assertEqual(first, (root / "SHA256SUMS").read_text())
            self.assertIn("  release.json\n", first)
            self.assertEqual(len(first.splitlines()), 2)
            (root / "unsafe").symlink_to(root / "release.json")
            with self.assertRaises(ValueError):
                release.write_checksums(root)

    def test_image_evidence_requires_real_sbom_and_provenance(self):
        index = {"manifests": [{"annotations": {"vnd.docker.reference.type": "attestation-manifest"}}]}
        sbom = {"linux/amd64": {"SPDX": {"spdxVersion": "SPDX-2.3"}}}
        provenance = {"linux/amd64": {"SLSA": {"buildType": "https://mobyproject.org/buildkit@v1"}}}
        evidence.require_attestations(index, sbom, provenance)
        evidence.require_attestations(index, sbom["linux/amd64"], provenance["linux/amd64"])
        for invalid in (({}, sbom, provenance), (index, {}, provenance), (index, sbom, {})):
            with self.assertRaises(ValueError):
                evidence.require_attestations(*invalid)

    def test_image_digest_comes_from_build_metadata(self):
        self.assertEqual(evidence.image_from_metadata("ghcr.io/example/image", {
            "containerimage.digest": "sha256:" + "a" * 64}), IMAGE)
        with self.assertRaises(ValueError):
            evidence.image_from_metadata("ghcr.io/example/image", {})

    def test_runtime_check_uses_chart_helper_path_and_exact_release_identity(self):
        with mock.patch.object(evidence.subprocess, "check_output", return_value="vpcctl v0.2.0 (commit=" + "c" * 40 + ", sourceDate=fixture)\n") as run:
            evidence.verify_runtime(IMAGE, "cli", "v0.2.0", "c" * 40)
            argv = run.call_args.args[0]
            self.assertIn("/vpcctl", argv)
            self.assertEqual(argv[argv.index("--network") + 1], "none")
        with mock.patch.object(evidence.subprocess, "check_output", return_value="vpcctl v0.1.0 (commit=unknown)\n"):
            with self.assertRaises(ValueError):
                evidence.verify_runtime(IMAGE, "cli", "v0.2.0", "c" * 40)

    def test_chart_defaults_preserve_settings_and_pin_all_images(self):
        values = {"image": {"pullPolicy": "IfNotPresent"}, "native": {"target": "v1.16.3"},
                  "site": {"location": ""}, "replicaCount": 1}
        prepared = charts.populate(values, "global-vpc-site", IMAGE, IMAGE, IMAGE)
        self.assertEqual(prepared["image"]["pullPolicy"], "IfNotPresent")
        for name in ("image", "cliImage", "gatewayImage"):
            self.assertEqual(prepared[name]["repository"], "ghcr.io/example/image")
            self.assertEqual(prepared[name]["digest"], "sha256:" + "a" * 64)
        self.assertEqual(prepared["site"], {"location": ""})
        self.assertEqual(values["image"], {"pullPolicy": "IfNotPresent"})
        with self.assertRaises(ValueError):
            charts.image_values("image:latest")

    @unittest.skipUnless(shutil.which("helm"), "Helm is required to exercise its values parser")
    def test_helm_values_parser_handles_comments_and_preserves_types(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            source.mkdir()
            (source / "values.yaml").write_text("# operator settings\nimage:\n  digest: ''\nreplicaCount: 1\noptional: null\nenabled: false\nports: [443]\n")
            values = charts.read_values(source, root)
            # Helm 3 retains default null keys; Helm 4 coalesces them away.
            # Both have the same nil behavior in the chart's templates.
            self.assertIsNone(values.pop("optional", None))
            self.assertEqual(values, {
                "image": {"digest": ""}, "replicaCount": 1,
                "enabled": False, "ports": [443]})

    @unittest.skipUnless(shutil.which("helm"), "Helm is required to package release charts")
    def test_packaged_charts_embed_images_and_bundles_without_modifying_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "charts"
            output = root / "out"
            output.mkdir()
            for name in charts.CHARTS:
                chart = source / name
                chart.mkdir(parents=True)
                (chart / "Chart.yaml").write_text(f"apiVersion: v2\nname: {name}\nversion: 0.1.0\n")
                (chart / "values.yaml").write_text("image: {repository: '', digest: ''}\ncliImage: {repository: '', digest: ''}\n")
            native_outputs = []
            for target in release.TARGETS:
                build = root / target
                build.mkdir()
                (build / "bundle.json").write_text(json.dumps(self.fixture(target)[0]))
                native_outputs.extend(["--native-output", str(build)])
            subprocess.run([sys.executable, str(CHART_SPEC.origin), "--charts-root", str(source),
                            "--output", str(output), "--version", "v0.2.0", "--controller-image", IMAGE,
                            "--gateway-image", IMAGE, "--cli-image", IMAGE, *native_outputs],
                           check=True, capture_output=True, text=True)
            for name in charts.CHARTS:
                self.assertIn("repository: ''", (source / name / "values.yaml").read_text())
                with tarfile.open(output / f"{name}-0.2.0.tgz") as archive:
                    values = json.load(archive.extractfile(f"{name}/values.yaml"))
                    self.assertEqual(values["cliImage"]["digest"], "sha256:" + "a" * 64)
                    if name == "global-vpc-site":
                        self.assertEqual(values["gatewayImage"]["repository"], "ghcr.io/example/image")
                    if name == "kube-ovn-global-vpc-extension":
                        for target in release.TARGETS:
                            native = json.load(archive.extractfile(f"{name}/files/native-{target}.json"))
                            self.assertEqual(native["target"], target)

    @unittest.skipUnless(shutil.which("helm"), "Helm is required to render packaged repository charts")
    def test_actual_release_charts_render_without_user_image_or_bundle_inputs(self):
        repository = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "out"
            output.mkdir()
            native_outputs = []
            for target in release.TARGETS:
                build = root / target
                build.mkdir()
                (build / "bundle.json").write_text(json.dumps(self.fixture(target)[0]))
                native_outputs.extend(["--native-output", str(build)])
            subprocess.run([sys.executable, str(CHART_SPEC.origin), "--charts-root", str(repository / "charts"),
                            "--output", str(output), "--version", "v0.2.0", "--controller-image", IMAGE,
                            "--gateway-image", IMAGE, "--cli-image", IMAGE, *native_outputs],
                           check=True, capture_output=True, text=True)
            for name, fixture in (("global-vpc", "platform.json"), ("global-vpc-site", "site-a.json")):
                config = json.loads((repository / "config/examples/managed" / fixture).read_text())
                if name == "global-vpc":
                    config.pop("registryNamespace", None)
                    values = {"platform": config}
                else:
                    for key in ("namespace", "authorityKubeconfig", "gatewayImage"):
                        config.pop(key, None)
                    values = {"local": config, "authorityAccess": {"existingSecret": "location-access"}}
                values_path = root / f"{name}.json"
                values_path.write_text(json.dumps(values))
                asset = output / f"{name}-0.2.0.tgz"
                rendered = subprocess.check_output(["helm", "template", "demo", str(asset), "--namespace",
                                                    "custom-system", "--values", str(values_path)], text=True)
                self.assertIn(IMAGE, rendered)
                subprocess.run(["helm", "lint", str(asset), "--strict", "--values", str(values_path)],
                               check=True, capture_output=True, text=True)
            asset = output / "kube-ovn-global-vpc-extension-0.2.0.tgz"
            for target in release.TARGETS:
                rendered = subprocess.check_output(["helm", "template", "native", str(asset), "--namespace",
                                                    "kube-system", "--set", "native.target=" + target], text=True)
                line = next(line for line in rendered.splitlines() if line.startswith("  bundle.json: "))
                bundle = json.loads(json.loads(line.split(": ", 1)[1]))
                self.assertEqual(bundle["target"], target)
                self.assertEqual(bundle["sourceCommit"], release.TARGETS[target][0])
                self.assertIn(IMAGE, rendered)
                subprocess.run(["helm", "lint", str(asset), "--strict", "--set", "native.target=" + target],
                               check=True, capture_output=True, text=True)


if __name__ == "__main__":
    unittest.main()
