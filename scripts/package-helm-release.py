#!/usr/bin/env python3
"""Package primary charts with immutable release image defaults and native bundles."""

import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


CHARTS = ("global-vpc", "global-vpc-site", "kube-ovn-global-vpc-extension")


def image_values(reference):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}", reference):
        raise ValueError("Chart defaults require an immutable image reference")
    repository, digest = reference.rsplit("@", 1)
    return {"repository": repository, "digest": digest}


def read_values(chart, temporary):
    # Use Helm's parser, including its default-value merge semantics. No extra
    # YAML implementation or build tool is required on a chart consumer's host.
    parser_chart = temporary / "values-reader"
    (parser_chart / "templates").mkdir(parents=True)
    (parser_chart / "Chart.yaml").write_text("apiVersion: v2\nname: values-reader\nversion: 0.0.0\n")
    shutil.copyfile(chart / "values.yaml", parser_chart / "values.yaml")
    (parser_chart / "templates/values.json").write_text("{{ .Values | toJson }}\n")
    rendered = subprocess.check_output(["helm", "template", "values-reader", str(parser_chart)], text=True)
    values = json.loads("\n".join(line for line in rendered.splitlines()
                                  if line and not line.startswith("#") and line != "---"))
    shutil.rmtree(parser_chart)
    return values


def populate(values, name, controller, gateway, cli):
    values = dict(values)
    if name != "kube-ovn-global-vpc-extension":
        values["image"] = dict(values.get("image", {}), **image_values(controller))
    values["cliImage"] = dict(values.get("cliImage", {}), **image_values(cli))
    if name == "global-vpc-site":
        values["gatewayImage"] = dict(values.get("gatewayImage", {}), **image_values(gateway))
    return values


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--charts-root", type=Path, default=Path("charts"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--controller-image", required=True)
    parser.add_argument("--gateway-image", required=True)
    parser.add_argument("--cli-image", required=True)
    parser.add_argument("--native-output", action="append", type=Path, required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", args.version):
        raise ValueError("Version must be vMAJOR.MINOR.PATCH")
    bundles = {}
    for output in args.native_output:
        bundle = json.loads((output / "bundle.json").read_text())
        bundles[bundle["target"]] = bundle
    if set(bundles) != {"v1.16.3", "v1.16.4"}:
        raise ValueError("Both native bundle targets are required")
    with tempfile.TemporaryDirectory(prefix="global-vpc-charts-") as temp:
        stage = Path(temp)
        for name in CHARTS:
            source = args.charts_root / name
            chart = stage / name
            shutil.copytree(source, chart)
            values = populate(read_values(source, stage), name, args.controller_image,
                              args.gateway_image, args.cli_image)
            (chart / "values.yaml").write_text(json.dumps(values, indent=2, sort_keys=True) + "\n")
            if name == "kube-ovn-global-vpc-extension":
                (chart / "files").mkdir(exist_ok=True)
                for target, bundle in bundles.items():
                    (chart / "files" / f"native-{target}.json").write_text(json.dumps(bundle, indent=2, sort_keys=True) + "\n")
            subprocess.run(["helm", "package", str(chart), "--version", args.version[1:],
                            "--app-version", args.version, "--destination", str(stage)], check=True)
            asset = f"{name}-{args.version[1:]}.tgz"
            shutil.copyfile(stage / asset, args.output / asset)


if __name__ == "__main__":
    main()
