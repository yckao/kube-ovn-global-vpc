"""Local gateway ownership, crash recovery, drain and route-policy contracts.

The fake Kubernetes server models UID/resourceVersion preconditions and committed
response loss. These tests do not claim live kernel or routing convergence proof.
"""
import base64
import copy
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import gateway.gateway as runtime
from gateway.gateway import render_frr
from scripts.site_gateway import Backend, HASH, MEMBER, OWNER, ROLE, decode, validate


ROOT = Path(__file__).resolve().parents[1]


def registration():
    return {
        'revision': 'wireguard-v1', 'siteID': 'site-a', 'attachmentID': 'infra-a',
        'kubeconfig': 'test-only-unused', 'clusterUID': 'cluster-uid',
        'namespace': 'gateway-system', 'namespaceUID': 'namespace-uid',
        'image': 'example.invalid/gateway:test', 'runtimePath': str(ROOT/'gateway/gateway.py'),
        'grants': [{
            'globalVpcID': 'tenant-one', 'nodeName': 'node-a', 'listenPort': 32150,
            'privateKeySecretName': 'gateway-private-key', 'localTunnelIP': '10.254.100.1',
            'localASN': 65101, 'mtu': 1380,
            'peers': [{
                'endpoint': '192.0.2.2:32150',
                'publicKey': base64.b64encode(bytes([1])*32).decode(),
                'tunnelIP': '10.254.100.2', 'asn': 65102,
                'delegatedPrefixes': ['10.252.2.0/24'],
            }],
        }],
    }


def request(operation='Ensure', expected=None):
    result = {'version': 'v1', 'operation': operation, 'gateway': {
        'version': 'v1', 'gatewayRevision': 'wireguard-v1', 'globalVpcID': 'tenant-one',
        'ownerUID': 'owner-uid', 'siteID': 'site-a', 'attachmentID': 'infra-a',
        'vpcName': 'vpc-one', 'subnetName': 'subnet-one', 'transitName': 'transit-one',
        'cidr': '10.252.1.0/28', 'delegatedPrefixes': ['10.252.1.0/24'],
        'transitCIDR': '10.253.1.0/30', 'routerIP': '10.253.1.1', 'gatewayIP': '10.253.1.2',
    }}
    if expected is not None:
        result['expectedResources'] = copy.deepcopy(expected)
    return result


class FakeCluster:
    def __init__(self):
        self.resources = {}
        self.pods = []
        self.ips = []
        self.calls = []
        self.next_uid = 1
        self.cluster_uid = 'cluster-uid'
        self.namespace_uid = 'namespace-uid'
        self.lose_response_kind = None
        self.fail_create_kind = None
        self.defer_delete = False


class FakeBackend(Backend):
    def __init__(self, cluster, config=None, req=None):
        self.cluster = cluster
        super().__init__(config or registration(), req or request())

    def kube(self, *args, data=None):
        self.cluster.calls.append((args, copy.deepcopy(data)))
        if args[0] == 'get':
            kind = args[1]
            if kind == 'namespace':
                uid = self.cluster.cluster_uid if args[2] == 'kube-system' else self.cluster.namespace_uid
                return json.dumps({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': args[2], 'uid': uid}})
            if kind == 'pods':
                owned = [copy.deepcopy(value) for (kind, _, _), value in self.cluster.resources.items() if kind == 'Pod']
                return json.dumps({'items': self.cluster.pods + owned})
            if kind == 'ips.kubeovn.io':
                return json.dumps({'items': self.cluster.ips})
            namespace = args[args.index('-n')+1]
            obj = self.cluster.resources.get((kind, namespace, args[2]))
            return json.dumps(obj) if obj else ''
        if args[0] == 'create':
            if self.cluster.fail_create_kind == data['kind']:
                self.cluster.fail_create_kind = None
                raise TimeoutError('simulated failure before create')
            obj = copy.deepcopy(data)
            metadata = obj['metadata']
            key = (obj['kind'], metadata['namespace'], metadata['name'])
            if key in self.cluster.resources:
                raise RuntimeError('AlreadyExists')
            metadata['uid'] = 'uid-'+str(self.cluster.next_uid)
            metadata['resourceVersion'] = str(self.cluster.next_uid)
            self.cluster.next_uid += 1
            if obj['kind'] == 'Pod':
                obj['status'] = {'conditions': [{'type': 'Ready', 'status': 'True'}]}
            self.cluster.resources[key] = obj
            if self.cluster.lose_response_kind == data['kind']:
                self.cluster.lose_response_kind = None
                raise TimeoutError('simulated committed response loss')
            return json.dumps(obj)
        if args[0] == 'replace':
            key = (data['kind'], data['metadata']['namespace'], data['metadata']['name'])
            current = self.cluster.resources[key]
            if current['metadata']['resourceVersion'] != data['metadata']['resourceVersion']:
                raise RuntimeError('Conflict')
            obj = copy.deepcopy(data)
            obj['metadata']['uid'] = current['metadata']['uid']
            obj['metadata']['resourceVersion'] = str(self.cluster.next_uid)
            self.cluster.next_uid += 1
            self.cluster.resources[key] = obj
            return json.dumps(obj)
        if args[0] == 'delete':
            path = args[2].split('/')
            kind = {'pods': 'Pod', 'configmaps': 'ConfigMap'}[path[5]]
            key = (kind, path[4], path[6])
            current = self.cluster.resources[key]
            if data['preconditions']['uid'] != current['metadata']['uid']:
                raise RuntimeError('Conflict')
            if self.cluster.defer_delete:
                current['metadata']['deletionTimestamp'] = '2026-01-01T00:00:00Z'
            else:
                del self.cluster.resources[key]
            return '{}'
        raise AssertionError(('unexpected kubectl command', args))


class BackendTestCase(unittest.TestCase):
    def setUp(self):
        self.cluster = FakeCluster()

    def execute(self, operation='Ensure', expected=None, config=None):
        return FakeBackend(self.cluster, config=config, req=request(operation, expected)).run()

    def object(self, kind):
        return next(obj for (stored_kind, _, _), obj in self.cluster.resources.items() if stored_kind == kind)

    def mutations(self):
        return [call for call in self.cluster.calls if call[0][0] in ('create', 'replace', 'delete')]


class BackendLifecycleTests(BackendTestCase):
    def test_only_local_resources_are_created_and_replayed(self):
        first = self.execute()
        self.assertTrue(first['ready'])
        self.assertFalse(first['absent'])
        self.assertEqual({r['kind'] for r in first['resources']}, {'Pod', 'ConfigMap'})
        self.assertEqual(len(self.mutations()), 2)
        second = self.execute(expected=first['resources'])
        self.assertEqual(first['resources'], second['resources'])
        self.assertEqual(len(self.mutations()), 2)
        pod = self.object('Pod')
        self.assertFalse(pod['spec']['automountServiceAccountToken'])
        self.assertEqual(pod['spec']['volumes'][1]['secret']['secretName'], 'gateway-private-key')
        self.assertFalse(any(r['kind'] == 'Secret' for r in first['resources']))

    def test_foreign_owner_and_replaced_uid_block_before_mutation(self):
        for change in ('owner', 'role', 'uid'):
            with self.subTest(change=change):
                self.cluster = FakeCluster()
                receipts = self.execute()['resources']
                pod = self.object('Pod')
                if change == 'owner':
                    pod['metadata']['labels'][OWNER] = 'other-owner'
                elif change == 'role':
                    pod['metadata']['labels'][ROLE] = 'false'
                else:
                    pod['metadata']['uid'] = 'replacement-uid'
                before = len(self.mutations())
                for operation in ('Get', 'Ensure', 'Delete', 'EndpointsEmpty'):
                    with self.assertRaises(ValueError):
                        self.execute(operation, receipts)
                self.assertEqual(len(self.mutations()), before)

    def test_cluster_and_namespace_identity_guard(self):
        for field in ('cluster_uid', 'namespace_uid'):
            with self.subTest(field=field):
                self.cluster = FakeCluster()
                setattr(self.cluster, field, 'replacement')
                with self.assertRaises(ValueError):
                    self.execute()
                self.assertEqual(self.mutations(), [])

    def test_committed_response_loss_is_rediscovered_without_replacement(self):
        for kind in ('ConfigMap', 'Pod'):
            with self.subTest(kind=kind):
                self.cluster = FakeCluster()
                self.cluster.lose_response_kind = kind
                with self.assertRaises(TimeoutError):
                    self.execute()
                observed = self.execute('Get')
                self.assertFalse(observed['absent'])
                old_receipts = observed['resources']
                recovered = self.execute(expected=old_receipts)
                self.assertTrue(recovered['ready'])
                for receipt in old_receipts:
                    self.assertIn(receipt, recovered['resources'])
                self.assertEqual(len(self.mutations()), 2)

    def test_partial_loss_requires_durable_absence_before_recreation(self):
        initial = self.execute()['resources']
        pod_key = next(key for key in self.cluster.resources if key[0] == 'Pod')
        del self.cluster.resources[pod_key]
        with self.assertRaisesRegex(ValueError, 'recorded gateway resource missing'):
            self.execute(expected=initial)
        observed = self.execute('Get', initial)
        self.assertFalse(observed['absent'])
        self.assertFalse(observed['ready'])
        self.assertEqual([r['kind'] for r in observed['resources']], ['ConfigMap'])
        self.cluster.lose_response_kind = 'Pod'
        with self.assertRaises(TimeoutError):
            self.execute(expected=observed['resources'])
        recovered = self.execute('Get', observed['resources'])
        self.assertTrue(recovered['ready'])
        old_cm = next(r for r in initial if r['kind'] == 'ConfigMap')
        self.assertIn(old_cm, recovered['resources'])
        self.assertNotIn(next(r for r in initial if r['kind'] == 'Pod'), recovered['resources'])

    def test_config_hash_drift_blocks_without_mutating_resources(self):
        receipts = self.execute()['resources']
        cfg = registration()
        cfg['image'] = 'example.invalid/gateway:changed'
        before = len(self.mutations())
        with self.assertRaisesRegex(ValueError, 'configuration changed'):
            self.execute(expected=receipts, config=cfg)
        self.assertEqual(len(self.mutations()), before)

    def test_configmap_content_drift_is_repaired_with_resource_version(self):
        receipts = self.execute()['resources']
        configmap = self.object('ConfigMap')
        original_uid = configmap['metadata']['uid']
        configmap['data']['gateway.json'] = '{}'
        self.execute(expected=receipts)
        current = self.object('ConfigMap')
        self.assertEqual(current['metadata']['uid'], original_uid)
        self.assertEqual(json.loads(current['data']['gateway.json'])['gateway']['globalVpcID'], 'tenant-one')
        replace = next(data for args, data in self.mutations() if args[0] == 'replace')
        self.assertIn('resourceVersion', replace['metadata'])

    def test_pod_spec_drift_does_not_report_healthy_gateway(self):
        receipts = self.execute()['resources']
        self.object('Pod')['spec']['containers'][0]['image'] = 'example.invalid/unmanaged:image'
        before = len(self.mutations())
        with self.assertRaises(ValueError):
            self.execute(expected=receipts)
        self.assertEqual(len(self.mutations()), before)

    def test_pod_network_annotation_drift_does_not_report_healthy_gateway(self):
        receipts = self.execute()['resources']
        self.object('Pod')['metadata']['annotations']['ovn.kubernetes.io/logical_switch'] = 'foreign-transit'
        self.assertFalse(self.execute('Get', receipts)['ready'])
        with self.assertRaises(ValueError):
            self.execute(expected=receipts)

    def test_api_defaulted_pod_fields_do_not_trigger_false_drift(self):
        receipts = self.execute()['resources']
        pod = self.object('Pod')
        pod['spec']['dnsPolicy'] = 'ClusterFirst'
        pod['spec']['schedulerName'] = 'default-scheduler'
        pod['spec']['containers'][0]['terminationMessagePath'] = '/dev/termination-log'
        pod['spec'].pop('hostNetwork', None)
        self.assertTrue(self.execute(expected=receipts)['ready'])

    def test_delete_is_uid_pinned_and_retains_external_secret(self):
        receipts = self.execute()['resources']
        result = self.execute('Delete', receipts)
        self.assertTrue(result['absent'])
        self.assertFalse(result['ready'])
        self.assertEqual(result['resources'], [])
        deleted = [(args, data) for args, data in self.mutations() if args[0] == 'delete']
        self.assertEqual(len(deleted), 2)
        self.assertIn('/pods/', deleted[0][0][2])
        for _, options in deleted:
            self.assertIn(options['preconditions']['uid'], {r['uid'] for r in receipts})
        self.assertFalse(any('/secrets/' in args[2] for args, _ in deleted))

    def test_terminating_resources_do_not_report_absence(self):
        receipts = self.execute()['resources']
        self.cluster.defer_delete = True
        result = self.execute('Delete', receipts)
        self.assertFalse(result['absent'])
        self.assertFalse(result['ready'])
        self.assertEqual(len(result['resources']), 2)


class EndpointDrainTests(BackendTestCase):
    def pod(self, uid='workload-uid', subnet='subnet-one', namespace='workloads', name='workload'):
        return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {
            'name': name, 'namespace': namespace, 'uid': uid,
            'annotations': {'ovn.kubernetes.io/logical_switch': subnet},
        }}

    def ip(self, **overrides):
        spec = {'subnet': 'subnet-one', 'podName': 'workload', 'namespace': 'workloads', 'v4IpAddress': '10.252.1.2'}
        spec.update(overrides)
        return {'apiVersion': 'kubeovn.io/v1', 'kind': 'IP', 'metadata': {'name': 'test-ip', 'uid': 'ip-uid'}, 'spec': spec}

    def test_only_exact_owned_gateway_pod_is_excluded(self):
        receipts = self.execute()['resources']
        gateway_pod = self.object('Pod')
        self.assertTrue(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
        # Same name/labels and address cannot exempt a different Pod UID.
        foreign = copy.deepcopy(gateway_pod)
        foreign['metadata']['uid'] = 'foreign-uid'
        self.cluster.pods.append(foreign)
        self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])

    def test_workload_transit_multus_and_terminating_pods_block_delete(self):
        receipts = self.execute()['resources']
        for pod in (self.pod(), self.pod(subnet='transit-one'), self.pod(subnet='unrelated')):
            if pod['metadata']['annotations']['ovn.kubernetes.io/logical_switch'] == 'unrelated':
                pod['metadata']['annotations']['attachment.example/logical_switch'] = 'subnet-one'
            pod['metadata']['deletionTimestamp'] = '2026-01-01T00:00:00Z'
            self.cluster.pods = [pod]
            self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
            before = len(self.mutations())
            with self.assertRaisesRegex(ValueError, 'endpoints still exist'):
                self.execute('Delete', receipts)
            self.assertEqual(len(self.mutations()), before)

    def test_ip_allocations_and_secondary_subnets_block_delete(self):
        receipts = self.execute()['resources']
        for record in (self.ip(), self.ip(subnet='unrelated', attachSubnets=['subnet-one']), self.ip(subnet='transit-one')):
            self.cluster.ips = [record]
            self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
        self.cluster.ips = [self.ip(subnet='unrelated')]
        self.assertTrue(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])

    def test_gateway_ip_exception_requires_owned_pod_exact_address_and_no_secondary_network(self):
        receipts = self.execute()['resources']
        pod = self.object('Pod')
        exact = self.ip(subnet='transit-one', podName=pod['metadata']['name'], namespace='gateway-system', v4IpAddress='10.253.1.2')
        exact['metadata']['name'] = pod['metadata']['name']+'.gateway-system'
        self.cluster.ips = [exact]
        self.assertTrue(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
        duplicate = copy.deepcopy(exact)
        duplicate['metadata']['name'] = 'foreign-duplicate'
        self.cluster.ips.append(duplicate)
        self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
        for overrides in ({'namespace': 'other'}, {'podName': 'other'}, {'v4IpAddress': '10.253.1.1'}, {'attachSubnets': ['subnet-one']}):
            altered = copy.deepcopy(exact)
            altered['spec'].update(overrides)
            self.cluster.ips = [altered]
            self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])
        self.cluster.ips = [exact]
        del self.cluster.resources[next(key for key in self.cluster.resources if key[0] == 'Pod')]
        self.assertFalse(self.execute('EndpointsEmpty', receipts)['endpointsEmpty'])


class ValidationAndPolicyTests(unittest.TestCase):
    def test_valid_contract(self):
        gateway, transport = validate(registration(), request())
        self.assertEqual(gateway['cidr'], '10.252.1.0/28')
        self.assertEqual(transport['localASN'], 65101)

    def test_duplicate_json_is_rejected(self):
        for text in ('{"version":"v1","version":"v1"}', '{"gateway":{"ownerUID":"one","ownerUID":"two"}}'):
            with self.assertRaises(ValueError):
                decode(text)

    def test_malformed_or_injected_gateway_fields(self):
        changes = [
            ('ownerUID', 'owner\nrouter bgp 123'), ('ownerUID', 'invalid..identity'), ('globalVpcID', 'other-tenant'),
            ('siteID', 'site-b'), ('attachmentID', 'infra-b'), ('gatewayRevision', 'changed'),
            ('cidr', '10.252.1.1/28'), ('cidr', '10.252.9.0/24'),
            ('cidr', '10.252.1.0/28\n network 0.0.0.0/0'), ('cidr', '::/0'),
            ('transitCIDR', '10.252.1.0/28'), ('routerIP', '10.253.1.0'),
            ('gatewayIP', '10.253.1.3'), ('gatewayIP', '10.253.1.1'), ('routerIP', 184353025),
            ('cidr', 184353024), ('delegatedPrefixes', ['10.252.1.0/255.255.255.0']),
        ]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                req = request()
                req['gateway'][field] = value
                with self.assertRaises((ValueError, TypeError)):
                    validate(registration(), req)

    def test_malformed_or_injected_transport_values(self):
        changes = [
            ('localASN', '65101\n network 0.0.0.0/0'), ('localASN', 0), ('localASN', 4294967295),
            ('localASN', True), ('localASN', 65101.5), ('listenPort', 0), ('listenPort', 32150.5),
            ('mtu', 1280.5), ('mtu', 1500), ('localTunnelIP', '10.252.1.3'), ('localTunnelIP', 184353025),
        ]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                cfg = registration()
                cfg['grants'][0][field] = value
                with self.assertRaises((ValueError, TypeError)):
                    validate(cfg, request())

    def test_malformed_or_injected_peer_values(self):
        changes = [
            ('endpoint', '192.0.2.2:32150\n'), ('endpoint', '192.0.2.2:32150; touch /tmp/unexpected'),
            ('endpoint', 'example.invalid:32150'), ('endpoint', '192.0.2.2:70000'),
            ('asn', '65102\n network 0.0.0.0/0'), ('asn', 65101), ('asn', 65102.5), ('asn', True),
            ('publicKey', 'not-a-key'), ('tunnelIP', '10.254.100.1'), ('tunnelIP', '10.252.1.2'), ('tunnelIP', 184353026),
            ('delegatedPrefixes', []), ('delegatedPrefixes', ['10.252.1.0/24']),
            ('delegatedPrefixes', ['10.252.2.1/24']), ('delegatedPrefixes', ['10.252.2.0/24\n permit 0.0.0.0/0']),
        ]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                cfg = registration()
                cfg['grants'][0]['peers'][0][field] = value
                with self.assertRaises((ValueError, TypeError)):
                    validate(cfg, request())

    def test_overlapping_peer_delegations_are_rejected(self):
        cfg = registration()
        second = copy.deepcopy(cfg['grants'][0]['peers'][0])
        second.update(tunnelIP='10.254.100.3', endpoint='192.0.2.3:32150', asn=65103,
                      publicKey=base64.b64encode(bytes([2])*32).decode())
        cfg['grants'][0]['peers'].append(second)
        with self.assertRaisesRegex(ValueError, 'overlapping prefix ownership'):
            validate(cfg, request())

    def test_duplicate_wireguard_peer_key_is_rejected(self):
        cfg = registration()
        second = copy.deepcopy(cfg['grants'][0]['peers'][0])
        second.update(tunnelIP='10.254.100.3', endpoint='192.0.2.3:32150', asn=65103,
                      delegatedPrefixes=['10.252.3.0/24'])
        cfg['grants'][0]['peers'].append(second)
        with self.assertRaises(ValueError):
            validate(cfg, request())

    def test_frr_exports_only_local_prefix_and_imports_each_peer_delegation(self):
        cfg = registration()
        cfg['grants'][0]['peers'].append({
            'endpoint': '192.0.2.3:32150', 'publicKey': base64.b64encode(bytes([2])*32).decode(),
            'tunnelIP': '10.254.100.3', 'asn': 65103, 'delegatedPrefixes': ['10.252.3.0/24'],
        })
        gateway, transport = validate(cfg, request())
        lines = render_frr({'gateway': gateway, 'transport': transport}).splitlines()
        self.assertIn('ip prefix-list LOCAL seq 10 permit 10.252.1.0/28', lines)
        self.assertNotIn('ip prefix-list LOCAL seq 10 permit 10.252.1.0/28 le 32', lines)
        self.assertIn('ip prefix-list REMOTE0 seq 10 permit 10.252.2.0/24 le 32', lines)
        self.assertIn('ip prefix-list REMOTE1 seq 10 permit 10.252.3.0/24 le 32', lines)
        self.assertEqual([line for line in lines if line.strip().startswith('network ')], ['  network 10.252.1.0/28'])
        for i, peer in enumerate(transport['peers']):
            self.assertIn(f'  neighbor {peer["tunnelIP"]} prefix-list REMOTE{i} in', lines)
            self.assertIn(f'  neighbor {peer["tunnelIP"]} prefix-list LOCAL out', lines)
        self.assertFalse(any('redistribute' in line or 'default-originate' in line for line in lines))


class RuntimeRecoveryTests(unittest.TestCase):
    def run_with_network(self, initial_host=None, initial_local=None, fail='rename'):
        gateway, transport = validate(registration(), request())
        config = {'gateway': gateway, 'transport': transport}
        interface = 'gd'+runtime.hashlib.sha256(gateway['ownerUID'].encode()).hexdigest()[:10]
        alias = 'globalvpc:'+gateway['ownerUID']
        networks = {'host': dict(initial_host or {}), 'local': dict(initial_local or {})}
        calls = []

        class FakePath:
            def __init__(self, path):
                self.path = str(path)

            def read_text(self):
                if self.path != '/config/gateway.json':
                    raise AssertionError(self.path)
                return json.dumps(config)

            def unlink(self, **_):
                if self.path != '/run/gateway-ready':
                    raise AssertionError(self.path)

        def run(*args, check=True):
            namespace = 'host' if args[0] == 'nsenter' else 'local'
            command = args[args.index('--')+1:] if namespace == 'host' else args
            calls.append((namespace, command))
            links = networks[namespace]
            if command[:5] == ('ip', '-j', 'link', 'show', 'dev'):
                name = command[5]
                if name not in links:
                    return subprocess.CompletedProcess(command, 1, '', '')
                return subprocess.CompletedProcess(command, 0, json.dumps([{'ifalias': links[name]}]), '')
            if command[:3] == ('ip', 'link', 'delete'):
                del links[command[3]]
            elif command[:4] == ('ip', 'link', 'add', 'name'):
                self.assertEqual(namespace, 'host')
                self.assertEqual(command[5:7], ('alias', alias))
                self.assertNotIn(command[4], links)
                links[command[4]] = ''
                if fail == 'create-response-lost':
                    raise RuntimeError('simulated create response loss')
            elif command[:4] == ('ip', 'link', 'set', 'dev'):
                name = command[4]
                if command[5] == 'alias':
                    if fail == 'mark':
                        raise RuntimeError('simulated mark failure')
                    links[name] = command[6]
                    if fail == 'mark-response-lost':
                        raise RuntimeError('simulated mark response loss')
                elif command[5] == 'netns':
                    if fail == 'move':
                        raise RuntimeError('simulated move failure')
                    self.assertEqual(namespace, 'host')
                    self.assertNotIn(name, networks['local'])
                    links.pop(name)
                    # Simulate alias loss on move, then apply IFLA_IFALIAS.
                    networks['local'][name] = ''
                    if fail == 'move-partial':
                        raise RuntimeError('simulated partial netlink failure')
                    self.assertEqual(command[7:], ('alias', alias))
                    networks['local'][name] = command[8]
                    if fail == 'move-response-lost':
                        raise RuntimeError('simulated move response loss')
                elif command[5] == 'name':
                    if fail == 'rename':
                        raise RuntimeError('simulated rename failure after namespace move')
                    links[command[6]] = links.pop(name)
                    self.assertEqual(command[7:], ('alias', alias))
                    links[command[6]] = command[8]
                    if fail == 'rename-response-lost':
                        raise RuntimeError('simulated rename response loss')
                else:
                    raise AssertionError(command)
            elif command[:2] == ('wg', 'set'):
                raise RuntimeError('simulated WireGuard configuration failure')
            else:
                raise AssertionError(command)
            return subprocess.CompletedProcess(command, 0, '', '')

        with patch.object(runtime, 'Path', FakePath), patch.object(runtime, 'run', run), patch.object(runtime.signal, 'signal'):
            with self.assertRaises(RuntimeError):
                runtime.main()
        return networks, calls, interface, alias

    def test_failed_move_or_rename_leaves_no_owned_staging_interface(self):
        for fail in ('move', 'rename', 'configure', 'mark-response-lost', 'move-response-lost', 'rename-response-lost'):
            with self.subTest(fail=fail):
                networks, _, _, _ = self.run_with_network(fail=fail)
                self.assertEqual(networks, {'host': {}, 'local': {}})

    def test_partial_move_without_alias_is_preserved_and_never_configured(self):
        networks, calls, interface, _ = self.run_with_network(fail='move-partial')
        self.assertEqual(networks, {'host': {}, 'local': {interface: ''}})
        self.assertFalse(any(args[:2] == ('wg', 'set') for _, args in calls))

    def test_unmarked_create_remnant_is_preserved(self):
        for failure in ('create-response-lost', 'mark'):
            with self.subTest(failure=failure):
                networks, calls, interface, _ = self.run_with_network(fail=failure)
                self.assertEqual(networks, {'host': {interface: ''}, 'local': {}})
                self.assertFalse(any(args[:2] == ('wg', 'set') for _, args in calls))

    def test_restart_recovers_owned_staged_interface_left_in_pod_namespace(self):
        owner = request()['gateway']['ownerUID']
        interface = 'gd'+runtime.hashlib.sha256(owner.encode()).hexdigest()[:10]
        networks, calls, _, _ = self.run_with_network(initial_local={interface: 'globalvpc:'+owner})
        self.assertEqual(networks, {'host': {}, 'local': {}})
        removals = [(namespace, args) for namespace, args in calls if args[:3] == ('ip', 'link', 'delete')]
        self.assertEqual(removals.count(('local', ('ip', 'link', 'delete', interface))), 2)

    def test_foreign_staged_or_wireguard_interfaces_are_preserved(self):
        owner = request()['gateway']['ownerUID']
        interface = 'gd'+runtime.hashlib.sha256(owner.encode()).hexdigest()[:10]
        for namespace, name in (('host', interface), ('local', interface), ('local', 'wg0')):
            for alias in ('foreign-owner', ''):
                with self.subTest(namespace=namespace, name=name, alias=alias):
                    initial = {'initial_'+namespace: {name: alias}}
                    networks, calls, _, _ = self.run_with_network(**initial)
                    self.assertEqual(networks[namespace], {name: alias})
                    self.assertFalse(any(args[:3] == ('ip', 'link', 'delete') for _, args in calls))


def ha_registration():
    cfg = registration()
    cfg['revision'] = 'wireguard-ha-v2'
    members = []
    for local in range(2):
        member = {'id': 'gw-'+str(local+1), 'nodeName': 'node-'+str(local+1),
                  'privateKeySecretName': 'key-'+str(local+1), 'localASN': 65101,
                  'routerID': '10.253.1.'+str(local+2), 'mtu': 1380, 'links': []}
        for remote in range(2):
            pair = local*2+remote
            member['links'].append({
                'id': 'a'+str(local+1)+'-b'+str(remote+1), 'listenPort': 32250+remote,
                'localTunnelIP': '10.254.110.'+str(pair*2+1),
                'tunnelIP': '10.254.110.'+str(pair*2+2),
                'endpoint': '192.0.2.'+str(remote+2)+':'+str(32250+local),
                'publicKey': base64.b64encode(bytes([remote+1])*32).decode(),
                'asn': 65102, 'remoteSiteID': 'site-b', 'remoteAttachmentID': 'infra-b',
                'delegatedPrefixes': ['10.252.2.0/24'],
            })
        members.append(member)
    cfg['grants'] = [{'globalVpcID': 'tenant-one', 'gateways': members}]
    return cfg


def ha_request(operation='Ensure', expected=None):
    req = request(operation, expected)
    gateway = req['gateway']
    gateway.pop('gatewayIP')
    gateway.update(version='v2', gatewayRevision='wireguard-ha-v2', transitCIDR='10.253.1.0/29',
                   gateways=[{'id': 'gw-1', 'ip': '10.253.1.2'}, {'id': 'gw-2', 'ip': '10.253.1.3'}],
                   bfd={'sourceIP': '10.254.120.1', 'minRX': 300, 'minTX': 300, 'multiplier': 3})
    return req


def ha_runtime():
    member = Backend(ha_registration(), ha_request()).members[0]
    return {'gateway': member['gateway'], 'transport': member['transport']}


class HABackendTests(BackendTestCase):
    def execute(self, operation='Ensure', expected=None, config=None):
        return FakeBackend(self.cluster, config=config or ha_registration(), req=ha_request(operation, expected)).run()

    def member_object(self, kind, member):
        return next(obj for (stored_kind, _, _), obj in self.cluster.resources.items()
                    if stored_kind == kind and obj['metadata']['labels'][MEMBER] == member)

    def test_independent_members_report_exact_ready_ids_and_derived_siblings(self):
        result = self.execute()
        self.assertEqual((result['readyGateways'], result['desiredGateways']), (2, 2))
        self.assertEqual(result['readyGatewayIDs'], ['gw-1', 'gw-2'])
        self.assertEqual(len(result['resources']), 4)
        for member, sibling in [('gw-1', 'gw-2'), ('gw-2', 'gw-1')]:
            cm = self.member_object('ConfigMap', member)
            config = json.loads(cm['data']['gateway.json'])
            self.assertEqual(config['gateway']['gatewayID'], member)
            self.assertEqual([s['id'] for s in config['transport']['siblings']], [sibling])
        self.member_object('Pod', 'gw-1')['status'] = {}
        observed = self.execute('Get', result['resources'])
        self.assertTrue(observed['ready'])
        self.assertEqual(observed['readyGateways'], 1)
        self.assertEqual(observed['readyGatewayIDs'], ['gw-2'])
        self.member_object('Pod', 'gw-2')['status'] = {}
        observed = self.execute('Get', result['resources'])
        self.assertFalse(observed['ready'])
        self.assertEqual(observed['readyGatewayIDs'], [])

    def test_late_member_uid_conflict_blocks_all_mutations(self):
        initial = self.execute()['resources']
        self.member_object('Pod', 'gw-2')['metadata']['uid'] = 'replacement'
        self.member_object('ConfigMap', 'gw-1')['data']['gateway.json'] = '{}'
        before = len(self.mutations())
        for operation in ('Ensure', 'Delete', 'Get', 'EndpointsEmpty'):
            with self.assertRaisesRegex(ValueError, 'UID changed'):
                self.execute(operation, initial)
        self.assertEqual(len(self.mutations()), before)

    def test_partial_gateway_loss_and_committed_recreation_retains_other_receipts(self):
        initial = self.execute()['resources']
        lost = self.member_object('Pod', 'gw-2')
        del self.cluster.resources[('Pod', lost['metadata']['namespace'], lost['metadata']['name'])]
        with self.assertRaisesRegex(ValueError, 'recorded gateway resource missing'):
            self.execute(expected=initial)
        observed = self.execute('Get', initial)
        self.assertEqual(observed['readyGatewayIDs'], ['gw-1'])
        self.assertEqual(len(observed['resources']), 3)
        self.cluster.lose_response_kind = 'Pod'
        with self.assertRaises(TimeoutError):
            self.execute(expected=observed['resources'])
        recovered = self.execute('Get', observed['resources'])
        self.assertEqual(recovered['readyGatewayIDs'], ['gw-1', 'gw-2'])
        for receipt in observed['resources']:
            self.assertIn(receipt, recovered['resources'])
        self.assertEqual(self.execute('Ensure', recovered['resources']), recovered)

    def test_drift_on_second_gateway_is_preflighted_before_first_repair(self):
        initial = self.execute()['resources']
        self.member_object('ConfigMap', 'gw-1')['data']['gateway.json'] = '{}'
        self.member_object('Pod', 'gw-2')['spec']['nodeName'] = 'other-node'
        before = len(self.mutations())
        with self.assertRaisesRegex(ValueError, 'drifted'):
            self.execute(expected=initial)
        self.assertEqual(len(self.mutations()), before)
        observed = self.execute('Get', initial)
        self.assertEqual(observed['readyGatewayIDs'], [])

    def test_drain_exempts_both_exact_gateway_identities_only(self):
        result = self.execute()
        for member in ('gw-1', 'gw-2'):
            pod = self.member_object('Pod', member)
            self.cluster.ips.append({'metadata': {'name': pod['metadata']['name']+'.gateway-system'}, 'spec': {
                'podName': pod['metadata']['name'], 'namespace': 'gateway-system',
                'subnet': 'transit-one', 'v4IpAddress': pod['metadata']['annotations']['ovn.kubernetes.io/ip_address'],
            }})
        self.assertTrue(self.execute('EndpointsEmpty', result['resources'])['endpointsEmpty'])
        self.cluster.ips[1]['spec']['v4IpAddress'] = '10.253.1.2'
        self.assertFalse(self.execute('EndpointsEmpty', result['resources'])['endpointsEmpty'])
        self.cluster.ips = []
        impostor = copy.deepcopy(self.member_object('Pod', 'gw-2'))
        impostor['metadata']['uid'] = 'not-owned-uid'
        self.cluster.pods.append(impostor)
        self.assertFalse(self.execute('EndpointsEmpty', result['resources'])['endpointsEmpty'])
        self.cluster.pods = []
        deleted = self.execute('Delete', result['resources'])
        self.assertTrue(deleted['absent'])
        self.assertEqual(deleted['readyGatewayIDs'], [])
        self.assertEqual(deleted['resources'], [])


class HAValidationAndPolicyTests(unittest.TestCase):
    def test_same_remote_owner_pools_and_member_public_keys_can_repeat_on_distinct_links(self):
        validate(ha_registration(), ha_request())

    def test_siblings_require_the_same_remote_coverage_but_link_counts_may_differ(self):
        cfg = ha_registration()
        cfg['grants'][0]['gateways'][1]['links'].pop()
        validate(cfg, ha_request())
        for link in cfg['grants'][0]['gateways'][1]['links']:
            link.update(remoteSiteID='site-c', remoteAttachmentID='infra-c', asn=65103,
                        delegatedPrefixes=['10.252.3.0/24'])
        with self.assertRaisesRegex(ValueError, 'same remote delegated owners'):
            validate(cfg, ha_request())

    def test_member_failures_and_colocation_are_rejected(self):
        for mutate in (
            lambda members: members.pop(),
            lambda members: members[1].update(id='gw-1'),
            lambda members: members[1].update(nodeName=members[0]['nodeName']),
            lambda members: members[1].update(privateKeySecretName=members[0]['privateKeySecretName']),
            lambda members: members[1].update(localASN=65103),
            lambda members: members[1].update(routerID=members[0]['routerID']),
            lambda members: members[0].update(siblings=[]),
        ):
            cfg = ha_registration()
            mutate(cfg['grants'][0]['gateways'])
            with self.assertRaises(ValueError):
                validate(cfg, ha_request())

    def test_remote_delegation_asn_identity_and_link_endpoint_guards(self):
        changes = [
            {'remoteSiteID': 'site-c'}, {'remoteAttachmentID': 'infra-c'},
            {'asn': 65103}, {'asn': True}, {'asn': 65102.5},
            {'delegatedPrefixes': ['10.252.2.0/25']}, {'delegatedPrefixes': ['10.252.1.0/24']},
            {'delegatedPrefixes': ['10.252.2.0/24\n permit 0.0.0.0/0']},
            {'listenPort': 32250}, {'listenPort': 32251.0},
            {'localTunnelIP': '10.254.110.1'}, {'tunnelIP': '10.253.1.2'},
            {'endpoint': '192.0.2.2:32250'}, {'endpoint': '192.0.2.2:032250'},
            {'endpoint': '192.0.2.2:32250\n'}, {'publicKey': 'invalid'},
            {'id': 'a1-b1'}, {'id': 'a.b'}, {'remoteSiteID': 'site-b\nrouter bgp 1'},
        ]
        for changeset in changes:
            with self.subTest(changes=changeset):
                cfg = ha_registration()
                cfg['grants'][0]['gateways'][0]['links'][1].update(changeset)
                with self.assertRaises((ValueError, TypeError)):
                    validate(cfg, ha_request())

    def test_bfd_source_timers_and_gateway_ips_are_guarded(self):
        for changes in ({'sourceIP': '10.253.1.2'}, {'sourceIP': '10.252.2.2'},
                        {'sourceIP': '10.254.110.1'}, {'minRX': True}, {'minTX': 10.5},
                        {'minRX': 99}, {'minTX': 99}, {'multiplier': 1}, {'multiplier': 256}):
            req = ha_request()
            req['gateway']['bfd'].update(changes)
            with self.assertRaises(ValueError):
                validate(ha_registration(), req)
        for member in ({'id': 'gw-1', 'ip': '10.253.1.3'}, {'id': 'gw-3', 'ip': '10.253.1.2'},
                       {'id': 'gw-1', 'ip': '10.253.1.1'}, {'id': 'a'*64, 'ip': '10.253.1.2'}):
            req = ha_request()
            req['gateway']['gateways'][0] = member
            with self.assertRaises(ValueError):
                validate(ha_registration(), req)

    def test_per_link_interfaces_are_stable_and_do_not_share_allowed_ips_tables(self):
        config = ha_runtime()
        first = runtime.wireguard_links(config)
        config['transport']['links'].reverse()
        self.assertEqual(first, runtime.wireguard_links(config))
        self.assertEqual(len({link['interface'] for link in first}), 2)
        self.assertEqual(len({link['alias'] for link in first}), 2)
        self.assertTrue(all(len(link['interface']) <= 15 for link in first))
        self.assertEqual(first[0]['delegatedPrefixes'], first[1]['delegatedPrefixes'])

    def test_ecmp_frr_filters_bfd_and_sibling_fallback(self):
        config = ha_runtime()
        lines = render_frr(config).splitlines()
        self.assertIn('  maximum-paths 2', lines)
        self.assertIn(' peer 10.254.120.1 local-address 10.253.1.2 interface eth0', lines)
        self.assertIn(' peer 10.253.1.3 local-address 10.253.1.2 interface eth0', lines)
        self.assertIn('  neighbor 10.253.1.3 next-hop-self', lines)
        self.assertIn('  neighbor 10.253.1.3 prefix-list SIBLING in', lines)
        self.assertIn('  neighbor 10.253.1.3 prefix-list SIBLING out', lines)
        self.assertIn('ip prefix-list SIBLING seq 10 permit 10.252.2.0/24 le 32', lines)
        self.assertEqual(len([line for line in lines if line.startswith('ip prefix-list SIBLING ')]), 1)
        self.assertEqual([line for line in lines if line.strip().startswith('network ')], [])
        for i, link in enumerate(config['transport']['links']):
            self.assertIn(f' peer {link["tunnelIP"]} multihop local-address {link["localTunnelIP"]}', lines)
            self.assertIn(f'  neighbor {link["tunnelIP"]} prefix-list REMOTE{i} in', lines)
            self.assertIn(f'  neighbor {link["tunnelIP"]} prefix-list LOCAL out', lines)
            self.assertIn(f' neighbor {link["tunnelIP"]} graceful-restart-disable', lines)
        self.assertFalse(any(word in line for line in lines for word in ('route-reflector', 'allowas-in', 'multipath-relax', 'redistribute', 'default-originate')))


class HACompatibilityTests(unittest.TestCase):
    def test_ttl_compatibility_rule_is_exact_and_never_enters_host_namespace(self):
        config = ha_runtime()
        calls = []
        def run(*args, check=True):
            calls.append(args)
            return subprocess.CompletedProcess(args, 1 if args[1:3] == ('-j', 'list') else 0, '', '')
        with patch.object(runtime, 'run', run):
            runtime.install_bfd_compat(config)
        self.assertTrue(all(call[0] == 'nft' for call in calls))
        rule = next(call for call in calls if call[1:3] == ('add', 'rule'))
        self.assertIn(('iifname', 'eth0'), list(zip(rule, rule[1:])))
        self.assertIn(('saddr', '10.254.120.1'), list(zip(rule, rule[1:])))
        self.assertIn(('daddr', '10.253.1.2'), list(zip(rule, rule[1:])))
        self.assertIn(('dport', '3784'), list(zip(rule, rule[1:])))
        self.assertIn(('ttl', '254'), list(zip(rule, rule[1:])))
        self.assertIn(('set', '255'), list(zip(rule, rule[1:])))
        self.assertIn('counter', rule)

    def test_foreign_nft_table_is_preserved_and_owned_table_is_replaced_and_cleaned(self):
        config = ha_runtime()
        name, alias = runtime.nft_identity(config)
        for owner in ('foreign', alias):
            calls = []
            def run(*args, check=True):
                calls.append(args)
                return subprocess.CompletedProcess(args, 0, json.dumps({'nftables': [{'table': {'name': name, 'comment': owner}}]}), '')
            with self.subTest(owner=owner), patch.object(runtime, 'run', run):
                if owner == 'foreign':
                    with self.assertRaisesRegex(RuntimeError, 'foreign'):
                        runtime.install_bfd_compat(config)
                    runtime.cleanup_bfd_compat(config)
                    self.assertFalse(any(call[1] in ('add', 'delete') for call in calls))
                else:
                    runtime.install_bfd_compat(config)
                    runtime.cleanup_bfd_compat(config)
                    self.assertEqual(len([call for call in calls if call[1] == 'delete']), 2)

    def test_stats_excludes_wireguard_key_material(self):
        config = ha_runtime()
        calls = []
        def run(*args, check=True):
            calls.append(args)
            text = 'test-public-key 123 456\n' if args[0] == 'wg' else '[]'
            return subprocess.CompletedProcess(args, 0, text, '')
        with patch.object(runtime, 'run', run):
            evidence = runtime.counters(config)
        self.assertNotIn('test-public-key', json.dumps(evidence))
        self.assertTrue(all(call[-1] == 'transfer' for call in calls if call[0] == 'wg'))
        self.assertEqual(evidence['links'][0]['wireguardTransfers'], [{'rxBytes': 123, 'txBytes': 456}])


class HARuntimeRecoveryTests(unittest.TestCase):
    def test_alias_readback_rejects_unmarked_foreign_and_ambiguous_interfaces(self):
        for records in ([], [{}], [{'ifalias': ''}], [{'ifalias': 'foreign'}],
                        [{'ifalias': 'expected'}, {'ifalias': 'expected'}], {}):
            with self.subTest(records=records), patch.object(runtime, 'run',
                    return_value=subprocess.CompletedProcess([], 0, json.dumps(records), '')):
                with self.assertRaises(RuntimeError):
                    runtime.require_owned_link('wg-test', 'expected')

    def run_network(self, failure, foreign=False):
        config = ha_runtime()
        desired = runtime.wireguard_links(config)
        networks = {'host': {}, 'local': {}}
        if foreign:
            networks['host'][desired[1]['staged']] = 'foreign-owner'
        calls = []
        class FakePath:
            def __init__(self, path):
                self.path = str(path)
            def unlink(self, **_):
                if self.path != '/run/gateway-ready':
                    raise AssertionError(self.path)
        def run(*args, check=True):
            namespace = 'host' if args[0] == 'nsenter' else 'local'
            command = args[args.index('--')+1:] if namespace == 'host' else args
            calls.append((namespace, command))
            links = networks[namespace]
            if command[:5] == ('ip', '-j', 'link', 'show', 'dev'):
                name = command[5]
                return subprocess.CompletedProcess(command, 0 if name in links else 1,
                    json.dumps([{'ifalias': links[name]}]) if name in links else '', '')
            if command[:3] == ('ip', 'link', 'delete'):
                del links[command[3]]
            elif command[:4] == ('ip', 'link', 'add', 'name'):
                name = command[4]
                self.assertEqual(namespace, 'host')
                self.assertNotIn(name, links)
                self.assertEqual(command[5], 'alias')
                links[name] = ''
                if failure == 'create-response-lost' and name == desired[1]['staged']:
                    raise RuntimeError('simulated create response loss')
            elif command[:4] == ('ip', 'link', 'set', 'dev'):
                name = command[4]
                if command[5] == 'alias':
                    if failure == 'mark' and name == desired[1]['staged']:
                        raise RuntimeError('simulated mark failure')
                    links[name] = command[6]
                    if failure == 'mark-response-lost' and name == desired[1]['staged']:
                        raise RuntimeError('simulated mark response loss')
                elif command[5] == 'netns':
                    if failure == 'move' and name == desired[1]['staged']:
                        raise RuntimeError('simulated move failure')
                    expected = links.pop(name)
                    networks['local'][name] = ''
                    if failure == 'move-partial' and name == desired[1]['staged']:
                        raise RuntimeError('simulated partial netlink failure')
                    self.assertEqual(command[7:], ('alias', expected))
                    networks['local'][name] = command[8]
                    if failure == 'move-response-lost' and name == desired[1]['staged']:
                        raise RuntimeError('simulated move response loss')
                elif command[5] == 'name':
                    if failure == 'rename' and name == desired[1]['staged']:
                        raise RuntimeError('simulated rename failure')
                    links[command[6]] = links.pop(name)
                    self.assertEqual(command[7:], ('alias', links[command[6]]))
                    links[command[6]] = command[8]
                    if failure == 'rename-response-lost' and name == desired[1]['staged']:
                        raise RuntimeError('simulated rename response loss')
                elif command[5] != 'mtu':
                    raise AssertionError(command)
            elif command[:2] == ('wg', 'set'):
                self.assertEqual(command.count('peer'), 1)
                if failure == 'configure' and command[2] == desired[1]['interface']:
                    raise RuntimeError('simulated configure failure')
            elif command[:2] not in (('ip', 'address'), ('ip', 'route')):
                raise AssertionError(command)
            return subprocess.CompletedProcess(command, 0, '', '')
        with patch.object(runtime, 'Path', FakePath), patch.object(runtime, 'run', run), \
                patch.object(runtime, 'cleanup_bfd_compat'), patch.object(runtime.signal, 'signal'):
            with self.assertRaises(RuntimeError):
                runtime.main_ha(config)
        return networks, calls, desired

    def test_second_link_failure_removes_all_owned_links_in_both_namespaces(self):
        for failure in ('move', 'rename', 'configure', 'mark-response-lost', 'move-response-lost', 'rename-response-lost'):
            with self.subTest(failure=failure):
                networks, calls, desired = self.run_network(failure)
                self.assertEqual(networks, {'host': {}, 'local': {}})
                configured = [args for _, args in calls if args[:2] == ('wg', 'set')]
                self.assertEqual(configured[0][2], desired[0]['interface'])
                self.assertIn('10.252.2.0/24', configured[0][configured[0].index('allowed-ips')+1])
                if failure == 'configure':
                    self.assertEqual(len({args[2] for args in configured}), 2)

    def test_partial_move_preserves_unmarked_link_and_cleans_other_owned_links(self):
        networks, calls, desired = self.run_network('move-partial')
        self.assertEqual(networks, {'host': {}, 'local': {desired[1]['staged']: ''}})
        configured = [args[2] for _, args in calls if args[:2] == ('wg', 'set')]
        self.assertEqual(configured, [desired[0]['interface']])

    def test_unmarked_create_remnant_is_preserved_and_other_links_cleaned(self):
        for failure in ('create-response-lost', 'mark'):
            with self.subTest(failure=failure):
                networks, calls, desired = self.run_network(failure)
                self.assertEqual(networks, {'host': {desired[1]['staged']: ''}, 'local': {}})
                configured = [args[2] for _, args in calls if args[:2] == ('wg', 'set')]
                self.assertEqual(configured, [desired[0]['interface']])

    def test_second_link_foreign_staging_interface_survives_and_first_link_is_cleaned(self):
        networks, _, desired = self.run_network(None, foreign=True)
        self.assertEqual(networks, {'host': {desired[1]['staged']: 'foreign-owner'}, 'local': {}})


class HALocalAdvertisementTests(unittest.TestCase):
    def test_failure_diagnostics_cannot_echo_subprocess_or_arbitrary_error_data(self):
        marker='sensitive-test-marker'
        errors=[RuntimeError(marker),subprocess.CalledProcessError(1,[marker],marker,marker),
                subprocess.TimeoutExpired([marker],2,marker,marker),
                json.JSONDecodeError(marker,marker,0)]
        self.assertEqual([runtime.failure_code(error) for error in errors],
                         ['runtime-failure','command-failed','command-timeout','invalid-json'])

    def test_integrated_configuration_is_dispatched_by_vtysh_and_errors_are_fatal(self):
        with patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, '', '')) as call:
            runtime.load_runtime_config()
            self.assertEqual(call.call_args.args, ('vtysh', '--no-fork', '-f', '/run/frr/frr.conf'))
        with patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, '% Unknown command', '')):
            with self.assertRaises(RuntimeError):
                runtime.load_runtime_config()

    def peer(self, status='up'):
        return {'peer': '10.254.120.1', 'local': '10.253.1.2', 'interface': 'eth0',
                'multihop': False, 'vrf': 'default', 'status': status}

    def test_only_the_exact_ovn_singlehop_peer_controls_advertisement(self):
        config = ha_runtime()
        for status in ('up', 'down', 'init', 'shutdown'):
            result = subprocess.CompletedProcess([], 0, json.dumps(self.peer(status)), '')
            with patch.object(runtime, 'run', return_value=result) as call:
                self.assertEqual(runtime.local_bfd_up(config), status == 'up')
                self.assertEqual(call.call_args.kwargs['timeout'], 2)
        for change in ({'peer': '10.253.1.3'}, {'local': '10.253.1.3'}, {'interface': 'wg0'},
                       {'multihop': True}, {'vrf': 'other'}, {'status': 'unknown'}):
            peer = self.peer()
            peer.update(change)
            result = subprocess.CompletedProcess([], 0, json.dumps(peer), '')
            with patch.object(runtime, 'run', return_value=result), self.assertRaises(RuntimeError):
                runtime.local_bfd_up(config)

    def test_initial_down_up_down_and_recovery_are_idempotent(self):
        config = ha_runtime()
        state = False
        with patch.object(runtime, 'local_bfd_up', side_effect=[False, True, True, False, False, True]), \
                patch.object(runtime, 'set_local_advertisement') as write, patch('builtins.print'):
            for _ in range(6):
                state = runtime.refresh_local_advertisement(config, state)
        self.assertTrue(state)
        self.assertEqual([call.args[1] for call in write.call_args_list], [True, False, True])

    def test_advertisement_is_exact_local_prefix_and_never_saves_config(self):
        config = ha_runtime()
        with patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, '', '')) as call:
            runtime.set_local_advertisement(config, False)
            self.assertIn('no network 10.252.1.0/28', call.call_args.args)
            self.assertNotIn('write memory', call.call_args.args)
            runtime.set_local_advertisement(config, True)
            self.assertIn('network 10.252.1.0/28', call.call_args.args)

    def test_read_parse_timeout_and_write_errors_propagate_to_runtime_shutdown(self):
        config = ha_runtime()
        for output in ('not-json', '[]', '{}', 'x'*65537):
            with patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, output, '')):
                with self.assertRaises((ValueError, RuntimeError)):
                    runtime.refresh_local_advertisement(config, True)
        with patch.object(runtime, 'run', side_effect=subprocess.TimeoutExpired('vtysh', 2)):
            with self.assertRaises(subprocess.TimeoutExpired):
                runtime.refresh_local_advertisement(config, True)
        for result in (subprocess.CompletedProcess([], 0, '% Unknown command', ''),
                       subprocess.CompletedProcess([], 0, '', 'daemon unavailable')):
            with patch.object(runtime, 'run', return_value=result):
                with self.assertRaises(RuntimeError):
                    runtime.set_local_advertisement(config, False)

    def test_runtime_terminates_all_routing_daemons_when_health_evidence_is_uncertain(self):
        config = ha_runtime()
        for failure in ('read', 'timeout', 'write'):
            processes = []
            class Process:
                terminated = False
                def __init__(self, *_):
                    processes.append(self)
                def poll(self):
                    return 0 if self.terminated else None
                def terminate(self):
                    self.terminated = True
                def wait(self, **_):
                    return 0
            def run(*args, **_):
                output = ''
                if args[0] == 'vtysh' and args[2] == 'bfdd':
                    if failure == 'timeout':
                        raise subprocess.TimeoutExpired('vtysh', 2)
                    output = 'not-json' if failure == 'read' else json.dumps(self.peer())
                elif args[0] == 'vtysh' and args[2] == 'bgpd':
                    output = '% Unknown command'
                return subprocess.CompletedProcess(args, 0, output, '')
            with self.subTest(failure=failure), patch.object(runtime, 'run', run), \
                    patch.object(runtime, 'Path') as path, patch.object(runtime, 'delete_owned_link'), \
                    patch.object(runtime, 'create_owned_link'), patch.object(runtime, 'move_owned_link'), \
                    patch.object(runtime, 'install_bfd_compat'), patch.object(runtime, 'cleanup_bfd_compat'), \
                    patch.object(runtime.shutil, 'chown'), patch.object(runtime.signal, 'signal'), \
                    patch.object(runtime.time, 'sleep'), patch.object(runtime.subprocess, 'Popen', Process), \
                    patch('builtins.print'):
                with self.assertRaises((ValueError, RuntimeError, subprocess.TimeoutExpired)):
                    runtime.main_ha(config)
                self.assertEqual(len(processes), 3)
                self.assertTrue(all(process.terminated for process in processes))
                self.assertEqual(path.return_value.unlink.call_count, 2)


if __name__ == '__main__':
    unittest.main()
