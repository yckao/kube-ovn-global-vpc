"""Transport grants, packaging and native overlay safety contracts.

Unit contracts complement the separate kernel/FRR and packet-based Lab checks.
"""
import copy
import json
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from gateway import gateway as runtime
from gateway import overlay
from scripts.site_gateway import Backend, HASH, validate
from scripts.transport_profiles import validate_socket_allocations
from tests.test_site_gateway import FakeBackend, FakeCluster, ha_registration, ha_request


def registration(profile='geneve-bgp'):
    config = ha_registration()
    grant = config['grants'][0]
    grant.update(profile=profile, trustedUnderlay=True)
    for local, member in enumerate(grant['gateways']):
        member.pop('privateKeySecretName')
        outer = '192.0.2.' + str(11 + local)
        if profile == 'geneve-bgp':
            member['underlayIP'] = outer
        else:
            member.update(localVtepIP=outer, healthIP='10.254.121.'+str(local+1),
                          vni=11001, routeDistinguisher='65101:'+str(local+1),
                          routeTarget='65000:1001', vxlanPort=4789)
        for remote, link in enumerate(member['links']):
            link.pop('publicKey')
            link.update(listenPort=4789 if profile == 'vxlan-evpn' else 6081,
                        endpoint='192.0.2.'+str(21+remote)+(':'+str(4789 if profile == 'vxlan-evpn' else 6081)),
                        vni=12001+local*2+remote)
            if profile == 'vxlan-evpn':
                link['remoteHealthIP'] = '10.254.121.'+str(remote+3)
    return config


def configuration(profile='geneve-bgp'):
    member = Backend(registration(profile), ha_request()).members[0]
    return {'gateway': member['gateway'], 'transport': member['transport']}


class RegistrationTests(unittest.TestCase):
    def test_profiles_package_only_their_modules_and_no_key_mount(self):
        for profile, modules in [('geneve-bgp', {'overlay.py'}), ('vxlan-evpn', {'overlay.py', 'evpn.py'})]:
            with self.subTest(profile=profile):
                cluster = FakeCluster()
                result = FakeBackend(cluster, registration(profile), ha_request()).run()
                self.assertTrue(result['ready'])
                self.assertEqual(len(result['resources']), 4)
                for (kind, _, _), resource in cluster.resources.items():
                    if kind == 'Pod':
                        self.assertEqual([v['name'] for v in resource['spec']['volumes']], ['config'])
                        self.assertFalse(resource['spec']['hostNetwork'])
                        self.assertFalse(resource['spec']['automountServiceAccountToken'])
                    else:
                        self.assertEqual(set(resource['data']), {'gateway.json', 'gateway.py'} | modules)
                        transport = json.loads(resource['data']['gateway.json'])['transport']
                        self.assertEqual(transport['profile'], profile)
                        self.assertEqual(len(transport['siblings']), 1)
                        self.assertNotIn('privateKeySecretName', transport)

    def test_omitted_and_explicit_wireguard_preserve_runtime_hash(self):
        old = Backend(ha_registration(), ha_request())
        config = ha_registration()
        config['grants'][0]['profile'] = 'wireguard-bgp'
        explicit = Backend(config, ha_request())
        self.assertEqual(old.member_manifests(old.members[0], 'runtime'),
                         explicit.member_manifests(explicit.members[0], 'runtime'))

    def test_profile_change_requires_drain_before_mutation(self):
        cluster = FakeCluster()
        receipts = FakeBackend(cluster, registration(), ha_request()).run()['resources']
        before = copy.deepcopy(cluster.resources)
        with self.assertRaisesRegex(ValueError, 'configuration changed'):
            FakeBackend(cluster, registration('vxlan-evpn'), ha_request(expected=receipts)).run()
        self.assertEqual(before, cluster.resources)

    def test_trust_profile_and_legacy_version_are_explicit(self):
        mutations = [lambda c: c['grants'][0].pop('trustedUnderlay'),
                     lambda c: c['grants'][0].update(trustedUnderlay=False),
                     lambda c: c['grants'][0].update(profile='vxlan-static'),
                     lambda c: c['grants'][0].update(remoteKubeconfig='forbidden')]
        for mutate in mutations:
            config = registration()
            mutate(config)
            with self.assertRaises((ValueError, KeyError)):
                Backend(config, ha_request())
        req = ha_request()
        req['gateway']['version'] = 'v1'
        with self.assertRaises(ValueError):
            validate(registration(), req)

    def test_invalid_overlay_fields_fail_before_kubernetes_calls(self):
        changes = [('underlayIP', '10.252.1.2'), ('underlayIP', '127.0.0.1'),
                   ('underlayIP', '10.254.110.1'), ('underlayIP', '192.0.2.011'),
                   ('mtu', 9001), ('mtu', True), ('localASN', True),
                   ('nodeName', 'node-2'), ('siblings', [])]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                config, cluster = registration(), FakeCluster()
                config['grants'][0]['gateways'][0][field] = value
                with self.assertRaises((ValueError, TypeError)):
                    FakeBackend(cluster, config, ha_request())
                self.assertEqual(cluster.calls, [])

    def test_invalid_peer_fields_and_delegations_are_rejected(self):
        changes = [('vni', 0), ('vni', True), ('vni', 16777216),
                   ('listenPort', 6082), ('endpoint', '192.0.2.21:06081'),
                   ('endpoint', '10.252.2.1:6081'), ('endpoint', '192.0.2.11:6081'),
                   ('endpoint', '192.0.2.21:6081\n'), ('localTunnelIP', '10.253.1.2'),
                   ('tunnelIP', '10.254.120.1'), ('asn', 65101),
                   ('delegatedPrefixes', ['10.252.1.0/24']), ('publicKey', 'unsupported')]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                config = registration()
                config['grants'][0]['gateways'][0]['links'][0][field] = value
                with self.assertRaises((ValueError, TypeError)):
                    Backend(config, ha_request())

    def test_evpn_identity_scope_and_oam_addresses(self):
        changes = [('routeDistinguisher', '65101:2'), ('routeDistinguisher', '65536:1'),
                   ('routeTarget', '65000:2\n permit any'), ('routeTarget', '65000:2'),
                   ('vni', 11002), ('vxlanPort', 4790),
                   ('healthIP', '10.254.121.2'), ('healthIP', '10.254.121.3'),
                   ('healthIP', '192.0.2.21'), ('healthIP', '10.252.2.1')]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                config = registration('vxlan-evpn')
                config['grants'][0]['gateways'][0][field] = value
                with self.assertRaises(ValueError):
                    Backend(config, ha_request())
        for value in ('10.254.121.1', '10.254.121.4', '10.254.121.9', '10.252.2.1'):
            config = registration('vxlan-evpn')
            config['grants'][0]['gateways'][0]['links'][0]['remoteHealthIP'] = value
            with self.assertRaises(ValueError):
                Backend(config, ha_request())

    def test_same_port_different_vni_can_share_overlay_socket(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            validate_socket_allocations(registration(profile))
            config = registration(profile)
            member = config['grants'][0]['gateways'][0]
            member['links'][1]['vni'] = member['links'][0]['vni']
            with self.assertRaisesRegex(ValueError, 'collision'):
                validate_socket_allocations(config)

    def test_cross_grant_socket_collision_and_data_control_collision(self):
        config = registration()
        second = copy.deepcopy(config['grants'][0])
        second['globalVpcID'] = 'tenant-two'
        config['grants'].append(second)
        with self.assertRaises(ValueError):
            validate_socket_allocations(config)
        config = registration('vxlan-evpn')
        config['grants'][0]['gateways'][0]['links'][0]['vni'] = 11001
        with self.assertRaises(ValueError):
            validate_socket_allocations(config)
        config = registration()
        wg = ha_registration()['grants'][0]
        wg['gateways'][0]['links'][0]['listenPort'] = 6081
        config['grants'].append(wg)
        with self.assertRaises(ValueError):
            validate_socket_allocations(config)


class PreflightAPI:
    def __init__(self, source='192.0.2.11', mtu=1500):
        self.source, self.mtu, self.calls = source, mtu, []

    def run(self, *args, **kwargs):
        self.calls.append(args)
        command = args[5:]
        if command[:2] == ('env', 'LC_ALL=C'):
            command = command[2:]
        if command[:4] == ('ip', '-j', 'address', 'show'):
            body = [{'addr_info': [{'family': 'inet', 'local': '192.0.2.11'}]}]
        elif command[:4] == ('ip', '-j', 'route', 'get'):
            body = [{'dev': 'eth0', 'prefsrc': self.source}]
        elif command[:4] == ('ip', '-j', 'link', 'show'):
            body = [{'mtu': self.mtu}]
        else:
            raise AssertionError(args)
        return subprocess.CompletedProcess(args, 0, json.dumps(body), '')


class OverlayRuntimeTests(unittest.TestCase):
    def preflight(self, config, api):
        with patch.object(overlay.os, 'stat', side_effect=[SimpleNamespace(st_ino=1), SimpleNamespace(st_ino=2)]):
            return overlay.host_preflight(config, api)

    def test_unreachable_peer_does_not_block_healthy_peer_startup_checks(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            config = configuration(profile)
            first = config['transport']['links'][0]
            class MissingRoute(PreflightAPI):
                def run(self, *args, **kwargs):
                    if 'route' in args and first['endpoint'].split(':')[0] in args:
                        self.calls.append(args)
                        self.last_check = kwargs['check']
                        return subprocess.CompletedProcess(args, 2, '', 'RTNETLINK answers: Network is unreachable\n')
                    return super().run(*args, **kwargs)
            api = MissingRoute()
            with self.subTest(profile=profile):
                self.assertEqual(self.preflight(config, api), [first['id']])
                self.assertIs(api.last_check, False)
                self.assertEqual(len([c for c in api.calls if 'route' in c]), 2)
                self.assertEqual(len([c for c in api.calls if 'link' in c]), 1)
                self.assertTrue(all('LC_ALL=C' in c for c in api.calls if 'route' in c))
                with self.assertRaisesRegex(RuntimeError, 'MTU'):
                    self.preflight(config, MissingRoute(mtu=1400))

    def test_explicit_reject_routes_are_unavailable_but_bad_inspection_is_fatal(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            config = configuration(profile)
            class RouteResult(PreflightAPI):
                def run(self, *args, **kwargs):
                    if 'route' in args:
                        self.calls.append(args)
                        return subprocess.CompletedProcess(args, *self.response)
                    return super().run(*args, **kwargs)
            for kind in ('blackhole', 'unreachable', 'prohibit'):
                api = RouteResult()
                api.response = (0, json.dumps([{'type': kind}]), '')
                with self.subTest(profile=profile, route=kind):
                    self.assertEqual(self.preflight(config, api), [l['id'] for l in config['transport']['links']])
            for response in ((2, '', 'RTNETLINK answers: Operation not permitted'),
                             (2, '', 'RTNETLINK answers: Permission denied'),
                             (2, '', 'RTNETLINK answers: Invalid argument'),
                             (2, 'unexpected', 'RTNETLINK answers: Network is unreachable'),
                             (1, '', 'RTNETLINK answers: Network is unreachable'),
                             (0, 'invalid JSON', ''), (0, '[]', ''),
                             (0, '[{"type":"unknown"}]', '')):
                api = RouteResult()
                api.response = response
                with self.subTest(profile=profile, response=response), self.assertRaises((RuntimeError, ValueError)):
                    self.preflight(config, api)

    def test_preflight_is_read_only_and_enforces_geneve_route_source(self):
        api = PreflightAPI()
        self.preflight(configuration(), api)
        self.assertEqual(len(api.calls), 5)
        self.assertTrue(all(c[:5] == ('nsenter', '--target', '1', '--net', '--') for c in api.calls))
        with self.assertRaisesRegex(RuntimeError, 'source differs'):
            self.preflight(configuration(), PreflightAPI(source='192.0.2.99'))
        with self.assertRaisesRegex(RuntimeError, 'MTU'):
            self.preflight(configuration(), PreflightAPI(mtu=1400))

    def test_preflight_requires_local_address_and_distinct_namespace(self):
        config = configuration()
        config['transport']['underlayIP'] = '192.0.2.99'
        with self.assertRaisesRegex(RuntimeError, 'local host address'):
            self.preflight(config, PreflightAPI())
        with patch.object(overlay.os, 'stat', return_value=SimpleNamespace(st_ino=1)):
            with self.assertRaisesRegex(RuntimeError, 'tenant network namespace'):
                overlay.host_preflight(configuration(), PreflightAPI())

    def test_vxlan_preflight_uses_explicit_registered_source(self):
        api = PreflightAPI(source='192.0.2.99')
        self.preflight(configuration('vxlan-evpn'), api)
        routes = [c for c in api.calls if 'route' in c]
        self.assertTrue(all(c[-2:] == ('from', '192.0.2.11') for c in routes))

    def test_source_policy_is_namespace_local_atomic_and_exact(self):
        calls = []
        class API:
            def run(self, *args, **kwargs):
                calls.append(args)
                return subprocess.CompletedProcess(args, 1, '', 'No such file or directory')
            def run_input(self, args, body):
                calls.append((args, body))
        config = configuration()
        overlay.install_source_policy(config, API(), [('gn123', ['10.252.2.0/24', '10.254.110.2/32'])])
        self.assertEqual(calls[1][0], ['nft', '--check', '-f', '-'])
        self.assertEqual(calls[2][0], ['nft', '-f', '-'])
        self.assertEqual(calls[1][1], calls[2][1])
        body = calls[2][1]
        self.assertIn('ip saddr { 10.252.2.0/24, 10.254.110.2/32 } counter accept', body)
        self.assertIn('iifname "gn123" counter drop', body)
        self.assertNotIn('nsenter', str(calls))

    def test_foreign_source_policy_is_preserved(self):
        class API:
            def run(self, *args, **kwargs):
                return subprocess.CompletedProcess(args, 0, json.dumps({'nftables': [
                    {'table': {'name': args[-1], 'comment': 'foreign'}}]}), '')
            def run_input(self, *args):
                raise AssertionError('must not mutate foreign table')
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            overlay.install_source_policy(configuration(), API(), [])
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            overlay.cleanup_source_policy(configuration(), API())

    def test_tunnel_creation_marks_after_success_and_requires_readback(self):
        calls = []
        class API:
            def run(self, *args):
                calls.append(args)
            def require_owned_link(self, name, alias, host=False):
                calls.append(('verified', name, alias, host))
        overlay.create_tunnel_link(API(), 'gd123', 'globalvpc:owner', 'geneve', 'id', '100')
        self.assertIn('add', calls[0])
        self.assertNotIn('alias', calls[0])
        self.assertEqual(calls[1][-2:], ('alias', 'globalvpc:owner'))
        self.assertEqual(calls[2], ('verified', 'gd123', 'globalvpc:owner', True))
        calls.clear()
        class Failed(API):
            def run(self, *args):
                calls.append(args)
                raise TimeoutError('unknown create outcome')
        with self.assertRaises(TimeoutError):
            overlay.create_tunnel_link(Failed(), 'gd123', 'globalvpc:owner', 'geneve', 'id', '100')
        self.assertEqual(len(calls), 1)

    def test_geneve_bgp_exports_only_local_prefix_after_local_bfd(self):
        driver = overlay.GeneveBackend(configuration(), runtime)
        frr = driver.render_frr()
        self.assertNotIn('network 10.252.1.0/28', frr)
        self.assertIn('ip prefix-list LOCAL seq 10 permit 10.252.1.0/28', frr)
        self.assertIn('maximum-paths 2', frr)
        self.assertNotIn('redistribute', frr)


if __name__ == '__main__':
    unittest.main()
