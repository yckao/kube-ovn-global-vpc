#!/usr/bin/env python3
"""Import a gateway image using an isolated, temporary containerd loader Pod."""
import argparse
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubeconfig', required=True)
    parser.add_argument('--node', required=True)
    parser.add_argument('--namespace', required=True)
    parser.add_argument('--archive', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    base = ['kubectl', '--kubeconfig', args.kubeconfig, '--request-timeout=30s']

    def kube(*argv, obj=None):
        result = subprocess.run(base + list(argv), input=json.dumps(obj).encode() if obj is not None else None, capture_output=True, timeout=60)
        if result.returncode:
            raise RuntimeError('Image loader Kubernetes operation failed')
        return result.stdout.decode()

    if kube('get', 'namespace', args.namespace, '--ignore-not-found', '-o', 'name'):
        raise RuntimeError('Image loader namespace must be new')
    namespace = json.loads(kube('create', '-f', '-', '-o', 'json', obj={'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': args.namespace, 'labels': {'build.globalvpc.io/owner': args.namespace, 'pod-security.kubernetes.io/enforce': 'privileged'}}}))
    args.evidence.parent.mkdir(parents=True, exist_ok=True)
    ledger = {'namespace': args.namespace, 'namespaceUID': namespace['metadata']['uid'], 'node': args.node}
    args.evidence.write_text(json.dumps(ledger, indent=2) + '\n')
    pod = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'loader', 'labels': {'build.globalvpc.io/owner': args.namespace}}, 'spec': {
        'nodeName': args.node, 'hostNetwork': True, 'automountServiceAccountToken': False, 'restartPolicy': 'Never',
        'containers': [{'name': 'loader', 'image': 'docker.io/kubeovn/kube-ovn:v1.16.4', 'command': ['sleep', '86400'],
                        'resources': {'requests': {'cpu': '10m', 'memory': '32Mi'}, 'limits': {'memory': '256Mi'}},
                        'volumeMounts': [{'name': 'socket', 'mountPath': '/run/containerd/containerd.sock'}, {'name': 'ctr', 'mountPath': '/host-ctr', 'readOnly': True}]}],
        'volumes': [{'name': 'socket', 'hostPath': {'path': '/run/containerd/containerd.sock', 'type': 'Socket'}}, {'name': 'ctr', 'hostPath': {'path': '/usr/bin/ctr', 'type': 'File'}}]}}
    created = json.loads(kube('create', '-n', args.namespace, '-f', '-', '-o', 'json', obj=pod))
    ledger['podUID'] = created['metadata']['uid']
    args.evidence.write_text(json.dumps(ledger, indent=2) + '\n')
    kube('wait', '-n', args.namespace, 'pod/loader', '--for=condition=Ready', '--timeout=45s')
    with args.archive.open('rb') as stream:
        result = subprocess.run(base + ['exec', '-i', '-n', args.namespace, 'loader', '--', '/host-ctr', '--address', '/run/containerd/containerd.sock', '--namespace', 'k8s.io', 'images', 'import', '-'], stdin=stream, capture_output=True, timeout=120)
    if result.returncode:
        print(result.stderr.decode()[-1000:])
        raise RuntimeError('Image import failed; owned loader namespace retained for inspection')
    ledger['imported'] = True
    ledger['result'] = result.stdout.decode()
    args.evidence.write_text(json.dumps(ledger, indent=2) + '\n')
    print('Gateway image imported on', args.node)
    current = json.loads(kube('get', 'namespace', args.namespace, '-o', 'json'))
    if current['metadata']['uid'] != ledger['namespaceUID']:
        raise RuntimeError('Loader namespace replaced; refusing cleanup')
    kube('delete', '--raw', '/api/v1/namespaces/' + args.namespace, '-f', '-', obj={
        'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': ledger['namespaceUID'], 'resourceVersion': current['metadata']['resourceVersion']}})


if __name__ == '__main__':
    main()
