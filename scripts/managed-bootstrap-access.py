#!/usr/bin/env python3
"""Issue a short-lived, location-scoped development kubeconfig without logging it."""
import argparse
import json
import os
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True, help="Authority kubectl context")
    parser.add_argument("--namespace", required=True, help="Dedicated location binding namespace")
    parser.add_argument("--output", required=True, help="New private file outside version control")
    args = parser.parse_args()
    # O_EXCL prevents accidentally overwriting an existing credential.
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        def kubectl(*argv):
            result = subprocess.run(["kubectl", "--context", args.context, *argv],
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
            if result.returncode:
                raise RuntimeError("kubectl request failed; verify context and location RBAC")
            return result.stdout.decode()

        source = json.loads(kubectl("config", "view", "--minify", "--flatten", "--raw",
                                    "-o", "jsonpath={.clusters[0].cluster}"))
        if not source.get("server", "").startswith("https://") or not source.get("certificate-authority-data"):
            raise RuntimeError("authority access requires HTTPS and an explicit trusted CA")
        cluster = {k: source[k] for k in ("server", "certificate-authority-data")}
        token = kubectl("-n", args.namespace, "create", "token", "binding-reporter", "--duration=1h").strip()
        if not token:
            raise RuntimeError("authority returned an empty token")
        output = {
            "apiVersion": "v1", "kind": "Config", "current-context": "authority",
            "clusters": [{"name": "authority", "cluster": cluster}],
            "contexts": [{"name": "authority", "context": {
                "cluster": "authority", "user": "binding-reporter", "namespace": args.namespace}}],
            "users": [{"name": "binding-reporter", "user": {"token": token}}],
        }
        with os.fdopen(fd, "w") as stream:
            fd = None
            json.dump(output, stream)
            stream.write("\n")
        print("Wrote private development access file; requested token lifetime is one hour.")
    except Exception:
        if fd is not None:
            os.close(fd)
        os.unlink(args.output)
        raise


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        raise SystemExit(str(error)) from None
