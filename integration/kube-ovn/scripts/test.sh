#!/usr/bin/env bash
# Run only on the patched native source. Requires Linux for upstream netlink code.
set -euo pipefail
if [[ $# != 1 ]]; then echo 'Usage: test.sh PATCHED_KUBE_OVN_SOURCE' >&2; exit 2; fi
task_script_dir=$(cd "$(dirname "$0")" && pwd)
python3 "$task_script_dir/test-portable.py" "$1" --full
