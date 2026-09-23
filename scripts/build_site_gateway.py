#!/usr/bin/env python3
"""Build an isolated gateway OCI archive with a disposable Kubernetes builder.

No registry push or host Docker socket is used. The namespace must be new or
carry this tool's ownership label. Call cleanup after exporting the archive.
"""
import argparse
import json
from pathlib import Path
import subprocess

LABEL = 'build.globalvpc.io/owner'


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--kubeconfig', required=True)
    p.add_argument('--namespace', required=True)
    p.add_argument('--image', default='globalvpc/site-gateway:v0.1.0')
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('operation', choices=['start', 'status', 'export', 'cleanup'])
    a = p.parse_args()
    base = ['kubectl', '--kubeconfig', a.kubeconfig, '--request-timeout=30s']

    def kube(*args, obj=None, binary=False):
        result = subprocess.run(base + list(args), input=(json.dumps(obj).encode() if obj is not None else None), capture_output=True, timeout=120)
        if result.returncode:
            raise RuntimeError('Kubernetes build operation failed')
        return result.stdout if binary else result.stdout.decode()

    raw = kube('get', 'namespace', a.namespace, '--ignore-not-found', '-o', 'json')
    if raw:
        if json.loads(raw)['metadata'].get('labels', {}).get(LABEL) != a.namespace:
            raise RuntimeError('Refusing foreign build namespace')
    elif a.operation != 'start':
        raise RuntimeError('Build namespace is absent')

    if a.operation == 'start':
        if raw:
            raise RuntimeError('Build namespace already exists; inspect or clean up first')
        kube('create', '-f', '-', obj={'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': a.namespace, 'labels': {LABEL: a.namespace}}})
        kube('create', '-n', a.namespace, '-f', '-', obj={'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'context'}, 'data': {'Dockerfile': (Path(__file__).resolve().parents[1] / 'gateway/Dockerfile').read_text()}})
        pod = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'builder'}, 'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
            'containers': [
                {'name': 'build', 'image': 'gcr.io/kaniko-project/executor:v1.23.2', 'args': ['--context=dir:///context', '--dockerfile=/context/Dockerfile', '--destination=' + a.image, '--no-push', '--tar-path=/out/gateway.tar'], 'resources': {'requests': {'cpu': '100m', 'memory': '256Mi'}, 'limits': {'cpu': '1', 'memory': '1Gi'}}, 'volumeMounts': [{'name': 'context', 'mountPath': '/context'}, {'name': 'output', 'mountPath': '/out'}]},
                {'name': 'export', 'image': 'alpine:3.21', 'command': ['sleep', '86400'], 'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'}, 'limits': {'memory': '128Mi'}}, 'volumeMounts': [{'name': 'output', 'mountPath': '/out'}]}],
            'volumes': [{'name': 'context', 'configMap': {'name': 'context'}}, {'name': 'output', 'emptyDir': {}}]}}
        kube('create', '-n', a.namespace, '-f', '-', obj=pod)
        print('Isolated build started.')
    elif a.operation == 'status':
        pod = json.loads(kube('get', 'pod', 'builder', '-n', a.namespace, '-o', 'json'))
        print(json.dumps({'phase': pod['status']['phase'], 'containers': [{'name': c['name'], 'state': c['state']} for c in pod['status'].get('containerStatuses', [])]}))
        print(kube('logs', 'builder', '-n', a.namespace, '-c', 'build', '--tail=12'))
    elif a.operation == 'export':
        pod = json.loads(kube('get', 'pod', 'builder', '-n', a.namespace, '-o', 'json'))
        done = next(c for c in pod['status']['containerStatuses'] if c['name'] == 'build')['state'].get('terminated', {})
        if done.get('exitCode') != 0:
            raise RuntimeError('Build has not completed successfully')
        a.output.parent.mkdir(parents=True, exist_ok=True)
        with a.output.open('wb') as stream:
            result = subprocess.run(base + ['-n', a.namespace, 'exec', 'builder', '-c', 'export', '--', 'cat', '/out/gateway.tar'], stdout=stream, stderr=subprocess.PIPE, timeout=180)
        if result.returncode:
            raise RuntimeError('Archive export failed')
        print('Gateway archive exported:', a.output.stat().st_size, 'bytes')
    else:
        uid = json.loads(raw)['metadata']['uid']
        # Re-read immediately before deletion, and refuse namespace replacement.
        check = json.loads(kube('get', 'namespace', a.namespace, '-o', 'json'))
        if check['metadata']['uid'] != uid:
            raise RuntimeError('Build namespace identity changed')
        kube('delete', '--raw', '/api/v1/namespaces/' + a.namespace, '-f', '-', obj={
            'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': uid, 'resourceVersion': check['metadata']['resourceVersion']}})
        print('Build cleanup requested.')


if __name__ == '__main__':
    main()
