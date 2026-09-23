"""EVPN route policy, namespace ownership and local-health guard contracts.

These tests exercise the runtime's emitted commands. They do not substitute for
native FRR/kernel convergence and packet tests in the isolated live Lab.
"""
import copy
import json
import subprocess
import unittest
from unittest.mock import patch

from gateway.evpn import EvpnBackend, _link_record
from gateway.overlay import delete_owned_link


def configuration():
    return {'gateway': {
        'version': 'v2', 'ownerUID': 'owner-a', 'gatewayID': 'gw-1',
        'globalVpcID': 'tenant-a', 'gatewayIP': '10.253.30.2',
        'routerIP': '10.253.30.1', 'cidr': '10.252.30.0/27',
        'bfd': {'sourceIP': '10.254.120.1', 'minRX': 300, 'minTX': 300, 'multiplier': 3}},
        'transport': {
            'profile': 'vxlan-evpn', 'trustedUnderlay': True,
            'localVtepIP': '192.0.2.1', 'localASN': 65101, 'routerID': '10.253.30.2',
            'healthIP': '10.254.121.1',
            'vni': 11001, 'routeDistinguisher': '192.0.2.1:1001',
            'routeTarget': '65000:1001', 'vxlanPort': 4789, 'mtu': 1380,
            'links': [
                {'id': 'a1-b1', 'endpoint': '192.0.2.2:4789', 'listenPort': 4789,
                 'vni': 12001, 'localTunnelIP': '10.254.110.1', 'tunnelIP': '10.254.110.2',
                 'remoteHealthIP': '10.254.121.2',
                 'asn': 65102, 'delegatedPrefixes': ['10.252.40.0/24']},
                {'id': 'a1-b2', 'endpoint': '192.0.2.3:4789', 'listenPort': 4789,
                 'vni': 12002, 'localTunnelIP': '10.254.110.3', 'tunnelIP': '10.254.110.4',
                 'remoteHealthIP': '10.254.121.3',
                 'asn': 65102, 'delegatedPrefixes': ['10.252.40.0/24']}],
            'siblings': [{'ip': '10.253.30.3', 'id': 'gw-2'}]}}


class FakeAPI:
    from gateway.gateway import local_prefixes
    local_prefixes = staticmethod(local_prefixes)

    def __init__(self):
        self.calls = []
        self.links = {(False, 'eth0'): {'ifindex': 2, 'ifname': 'eth0'}}
        self.response = None
        self.responses = {}
        self.delete_failures = set()
        self.next_index = 10

    def run(self, *args, **options):
        self.calls.append((args, options))
        if args[0] == 'vtysh':
            if args[-1] in self.responses:
                return self.responses[args[-1]]
            return self.response or subprocess.CompletedProcess(args, 0, '', '')
        host = args[0] == 'nsenter'
        command = args[5:] if host else args
        if command[:4] == ('ip', '-j', 'link', 'show'):
            link = self.links.get((host, command[-1]))
            return subprocess.CompletedProcess(args, 0 if link else 1,
                                               json.dumps([link]) if link else '',
                                               '' if link else 'Device does not exist.')
        if command[:3] == ('ip', 'link', 'add'):
            name = command[4]
            if (host, name) in self.links:
                raise RuntimeError('already exists')
            self.next_index += 1
            self.links[(host, name)] = {'ifname': name, 'ifindex': self.next_index}
        if command[:3] == ('ip', 'link', 'set'):
            name = command[command.index('dev') + 1]
            item = self.links[(host, name)]
            if 'alias' in command:
                item['ifalias'] = command[command.index('alias') + 1]
            if 'master' in command:
                item['master'] = command[command.index('master') + 1]
            if 'nomaster' in command:
                item.pop('master', None)
        if command[:3] == ('ip', 'link', 'delete'):
            name = command[-1]
            if name in self.delete_failures:
                raise subprocess.TimeoutExpired(args, 15)
            self.links.pop((host, name), None)
        return subprocess.CompletedProcess(args, 0, '', '')

    def create_tunnel_link(self, name, alias, *kind):
        self.calls.append((('create_tunnel_link', name, alias, *kind), {}))
        if (True, name) in self.links:
            raise RuntimeError('already exists')
        self.links[(True, name)] = {'ifname': name, 'ifalias': alias, 'ifindex': self.next_index}
        self.next_index += 1

    def move_owned_link(self, staged, interface, alias):
        self.require_owned_link(staged, alias, host=True)
        item = self.links.pop((True, staged))
        item['ifname'] = interface
        self.links[(False, interface)] = item

    def require_owned_link(self, name, alias, host=False):
        if self.links.get((host, name), {}).get('ifalias') != alias:
            raise RuntimeError('foreign gateway interface')

    def delete_owned_link(self, name, alias, host=False, strict=False):
        if name in self.delete_failures:
            raise TimeoutError('simulated delete timeout')
        item = self.links.get((host, name))
        if item and item.get('ifalias') != alias:
            if strict:
                raise RuntimeError('foreign gateway interface')
            return
        self.links.pop((host, name), None)


class EvpnPolicyTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        self.config = configuration()
        self.backend = EvpnBackend(self.config, self.api)

    def test_type5_uses_native_vrf_and_scoped_local_export(self):
        rendered = self.backend.render_frr()
        self.assertIn('vrf gvpc-vrf\n vni 11001', rendered)
        self.assertIn('router bgp 65101 vrf gvpc-vrf', rendered)
        self.assertIn('  rd 192.0.2.1:1001', rendered)
        self.assertIn('  route-target both 65000:1001', rendered)
        self.assertIn('  advertise ipv4 unicast route-map LOCAL_EXPORT', rendered)
        self.assertIn('route-map LOCAL_EXPORT permit 10\n match ip address prefix-list LOCAL', rendered)
        self.assertNotIn('redistribute', rendered)
        self.assertNotIn('network 10.252.30.0/27', rendered)
        self.assertNotIn('advertise-svi-ip', rendered)
        self.assertNotIn('advertise-default-gw', rendered)
        self.assertNotIn('no bgp ebgp-requires-policy', rendered)

    def test_remote_policy_requires_tenant_rt_type5_and_peer_pool(self):
        rendered = self.backend.render_frr()
        self.assertIn('bgp extcommunity-list standard TENANT permit rt 65000:1001', rendered)
        for index, link in enumerate(self.config['transport']['links']):
            self.assertIn(f'route-map EVPN_IMPORT{index} permit 20\n match evpn route-type prefix\n'
                          f' match ip address prefix-list REMOTE{index}\n match extcommunity TENANT\n'
                          f' call DATA_READY{index}', rendered)
            self.assertIn(f'route-map DATA_READY{index} deny 10', rendered)
            self.assertIn(f'neighbor {link["tunnelIP"]} route-map EVPN_IMPORT{index} in', rendered)
            self.assertIn(f'neighbor {link["tunnelIP"]} route-map EVPN_EXPORT out', rendered)
            self.assertIn(f'neighbor {link["tunnelIP"]} graceful-restart-disable', rendered)
        self.assertIn('  maximum-paths 2', rendered)

    def test_evpn_keeps_vtep_next_hop_when_control_address_differs(self):
        rendered = self.backend.render_frr()
        evpn = rendered.split(' address-family l2vpn evpn\n')[1].split(' exit-address-family')[0]
        self.assertNotEqual(self.config['transport']['localVtepIP'], self.backend.links[0]['localTunnelIP'])
        for link in self.backend.links:
            self.assertIn(f'neighbor {link["tunnelIP"]} attribute-unchanged next-hop', evpn)
            self.assertNotIn(f'neighbor {link["tunnelIP"]} next-hop-self', evpn)
        # Same-site IPv4 fallback must still resolve through its sibling.
        self.assertIn('neighbor 10.253.30.3 next-hop-self', rendered)

    def test_evpn_refresh_avoids_pinned_frr_inbound_cache_lifetime_bug(self):
        self.assertNotIn('soft-reconfiguration inbound', self.backend.render_frr())
        for link in self.backend.links:
            self._remote_response(link)
        self.backend.refresh_remote_health()
        changes = [args for args, _ in self.api.calls if 'configure terminal' in args]
        for link, command in zip(self.backend.links, changes):
            self.assertIn(f'clear bgp l2vpn evpn {link["tunnelIP"]} soft in', command)

    def test_unsupported_route_refresh_never_records_gate_as_applied(self):
        for link in self.backend.links:
            self._remote_response(link)
        self.api.response = subprocess.CompletedProcess(
            [], 1, '% Inbound soft reconfiguration not enabled', '')
        with self.assertRaisesRegex(RuntimeError, 'EVPN data health operation'):
            self.backend.refresh_remote_health()
        self.assertFalse(any(self.backend.remote_health.values()))

    def _local_frr_responses(self):
        vni = {'vni': 11001, 'type': 'L3', 'tenantVrf': 'gvpc-vrf',
               'localVtepIp': '192.0.2.1', 'vxlanIntf': self.backend.data['interface'],
               'sviIntf': 'gvpc-br', 'state': 'Up'}
        mapping = {'vrf': 'gvpc-vrf', 'vni': 11001,
                   'vxlanIntf': self.backend.data['interface'], 'sviIntf': 'gvpc-br', 'state': 'Up'}
        self.api.responses['show evpn vni 11001 json'] = subprocess.CompletedProcess(
            [], 0, json.dumps(vni), '')
        self.api.responses['show vrf gvpc-vrf vni json'] = subprocess.CompletedProcess(
            [], 0, json.dumps({'vrfs': [mapping]}), '')
        return vni, mapping

    def test_local_readiness_proves_native_l3vni_without_remote_dependency(self):
        vni, _ = self._local_frr_responses()
        self.assertFalse(any(self.backend.remote_health.values()))
        self.assertEqual(self.backend.verify_local_frr(), vni)
        self.assertEqual(len(self.api.calls), 2)
        self.assertTrue(all(args[:4] == ('vtysh', '-d', 'zebra', '-c') and
                            options['timeout'] == 2 for args, options in self.api.calls))

    def test_local_readiness_retries_initial_l2_classification(self):
        vni, _ = self._local_frr_responses()
        original = self.api.run
        seen = 0

        def initially_l2(*args, **options):
            nonlocal seen
            if args[-1] == 'show evpn vni 11001 json':
                seen += 1
                if seen == 1:
                    return subprocess.CompletedProcess([], 0, json.dumps(dict(vni, type='L2')), '')
            return original(*args, **options)

        with patch.object(self.api, 'run', side_effect=initially_l2), patch('gateway.evpn.time.sleep') as sleep:
            self.assertEqual(self.backend.verify_local_frr(), vni)
        sleep.assert_called_once_with(0.25)

    def test_local_readiness_rejects_wrong_native_identity_or_down_state(self):
        for key, value in [('vni', 11002), ('type', 'L2'), ('tenantVrf', 'default'),
                           ('localVtepIp', '192.0.2.2'), ('vxlanIntf', 'foreign'),
                           ('sviIntf', 'foreign'), ('state', 'Down')]:
            with self.subTest(key=key), patch('gateway.evpn.time.sleep') as sleep:
                vni, _ = self._local_frr_responses()
                self.api.responses['show evpn vni 11001 json'].stdout = json.dumps(dict(vni, **{key: value}))
                with self.assertRaisesRegex(RuntimeError, 'L3VNI not operational'):
                    self.backend.verify_local_frr()
                self.assertEqual(sleep.call_count, 11)

    def test_local_readiness_rejects_missing_or_ambiguous_vrf_mapping(self):
        for mappings in ([], [{}, {}], [{'vrf': 'foreign'}]):
            with self.subTest(mappings=mappings), patch('gateway.evpn.time.sleep'):
                self._local_frr_responses()
                self.api.responses['show vrf gvpc-vrf vni json'].stdout = json.dumps({'vrfs': mappings})
                with self.assertRaisesRegex(RuntimeError, 'L3VNI not operational'):
                    self.backend.verify_local_frr()

    def test_local_readiness_rejects_malformed_oversized_or_warning_response(self):
        for response in (subprocess.CompletedProcess([], 0, '[]', ''),
                         subprocess.CompletedProcess([], 0, '{}', '% warning'),
                         subprocess.CompletedProcess([], 1, '{}', ''),
                         subprocess.CompletedProcess([], 0, ' ' * 65537, '')):
            with self.subTest(response=response), patch.object(self.api, 'run', return_value=response):
                with self.assertRaisesRegex(RuntimeError, 'response invalid'):
                    self.backend.verify_local_frr()

    def test_health_prefix_bypasses_only_data_gate_and_is_not_exported_to_siblings(self):
        rendered = self.backend.render_frr()
        self.assertIn('network 10.254.121.1/32', rendered)
        self.assertIn('ip prefix-list LOCAL_HEALTH seq 10 permit 10.254.121.1/32', rendered)
        for index, link in enumerate(self.backend.links):
            health_rule = rendered.split(f'route-map EVPN_IMPORT{index} permit 10\n')[1].split('exit')[0]
            self.assertIn(f'match ip address prefix-list HEALTH{index}', health_rule)
            self.assertIn('match extcommunity TENANT', health_rule)
            self.assertNotIn('call DATA_READY', health_rule)
            self.assertIn(f'peer {link["remoteHealthIP"]} multihop local-address 10.254.121.1 vrf gvpc-vrf', rendered)
        sibling_rules = [line for line in rendered.splitlines() if line.startswith('ip prefix-list SIBLING')]
        self.assertTrue(all('10.254.121.' not in line for line in sibling_rules))

    def _remote_response(self, link, status='up', **overrides):
        command = (f'show bfd vrf gvpc-vrf peer {link["remoteHealthIP"]} '
                   'multihop local-address 10.254.121.1 json')
        peer = {'peer': link['remoteHealthIP'], 'local': '10.254.121.1',
                'multihop': True, 'vrf': 'gvpc-vrf', 'status': status}
        peer.update(overrides)
        self.api.responses[command] = subprocess.CompletedProcess([], 0, json.dumps(peer), '')

    def test_data_health_gate_recovers_without_removing_oam_routes(self):
        for link in self.backend.links:
            self._remote_response(link)
        self.assertTrue(all(self.backend.refresh_remote_health().values()))
        changes = [args for args, _ in self.api.calls if 'configure terminal' in args]
        self.assertEqual(len(changes), 2)
        self.assertIn('route-map DATA_READY0 permit 10', changes[0])
        self.assertIn('clear bgp l2vpn evpn 10.254.110.2 soft in', changes[0])
        self.backend.refresh_remote_health()
        self.assertEqual(len([args for args, _ in self.api.calls if 'configure terminal' in args]), 2)
        self._remote_response(self.backend.links[0], 'down')
        result = self.backend.refresh_remote_health()
        self.assertFalse(result['a1-b1'])
        self.assertTrue(result['a1-b2'])
        self.assertIn('route-map DATA_READY0 deny 10', self.api.calls[-1][0])
        self.assertFalse(any('no network' in str(args) or 'EVPN_IMPORT' in str(args)
                             for args, _ in self.api.calls))
        self._remote_response(self.backend.links[0])
        self.assertTrue(all(self.backend.refresh_remote_health().values()))

    def test_data_health_read_ambiguity_prevents_all_policy_changes(self):
        self._remote_response(self.backend.links[0])
        self._remote_response(self.backend.links[1], vrf='default')
        with self.assertRaisesRegex(RuntimeError, 'remote BFD response identity'):
            self.backend.refresh_remote_health()
        self.assertFalse(any('configure terminal' in args for args, _ in self.api.calls))

    def test_data_health_change_error_does_not_mark_gate_applied(self):
        for link in self.backend.links:
            self._remote_response(link)
        self.api.response = subprocess.CompletedProcess([], 0, '% invalid', '')
        with self.assertRaisesRegex(RuntimeError, 'EVPN data health operation'):
            self.backend.refresh_remote_health()
        self.assertFalse(any(self.backend.remote_health.values()))

    def test_sibling_policy_does_not_export_local_prefix(self):
        rendered = self.backend.render_frr()
        self.assertIn('ip prefix-list SIBLING seq 10 permit 10.252.40.0/24 le 32', rendered)
        self.assertIn('neighbor 10.253.30.3 prefix-list SIBLING out', rendered)
        self.assertIn('neighbor 10.253.30.3 next-hop-self', rendered)
        self.assertIn('peer 10.253.30.3 local-address 10.253.30.2 interface eth0 vrf gvpc-vrf', rendered)

    def test_order_is_deterministic_and_owner_changes_all_owned_aliases(self):
        reordered = copy.deepcopy(self.config)
        reordered['transport']['links'].reverse()
        self.assertEqual(self.backend.render_frr(), EvpnBackend(reordered, self.api).render_frr())
        changed = copy.deepcopy(self.config)
        changed['gateway']['ownerUID'] = 'other-owner'
        other = EvpnBackend(changed, self.api)
        self.assertNotEqual(self.backend.data['alias'], other.data['alias'])
        self.assertNotEqual(dict(self.backend.local), dict(other.local))
        self.assertTrue(all(len(value) < 16 for value in self.backend.interfaces))

    def test_local_advertisement_targets_tenant_vrf_only(self):
        self.backend.set_local_advertisement(True)
        self.backend.set_local_advertisement(False)
        commands = [args for args, _ in self.api.calls]
        self.assertIn('router bgp 65101 vrf gvpc-vrf', commands[0])
        self.assertIn('network 10.252.30.0/27', commands[0])
        self.assertIn('no network 10.252.30.0/27', commands[1])
        self.api.response = subprocess.CompletedProcess([], 0, '% warning', '')
        with self.assertRaisesRegex(RuntimeError, 'not clean'):
            self.backend.set_local_advertisement(True)

    def test_local_bfd_requires_exact_vrf_and_session_identity(self):
        peer = {'peer': '10.254.120.1', 'local': '10.253.30.2', 'interface': 'eth0',
                'multihop': False, 'vrf': 'gvpc-vrf', 'status': 'up'}
        self.api.response = subprocess.CompletedProcess([], 0, json.dumps(peer), '')
        self.assertTrue(self.backend.local_bfd_up())
        self.assertIn('show bfd vrf gvpc-vrf peer 10.254.120.1', self.api.calls[-1][0][-1])
        for key, value in [('vrf', 'default'), ('peer', '10.254.120.2'),
                           ('local', '10.253.30.3'), ('interface', 'eth1'),
                           ('multihop', True), ('status', 'unknown')]:
            with self.subTest(key=key):
                invalid = dict(peer, **{key: value})
                self.api.response = subprocess.CompletedProcess([], 0, json.dumps(invalid), '')
                with self.assertRaisesRegex(RuntimeError, 'identity'):
                    self.backend.local_bfd_up()
        for state in ['down', 'init', 'shutdown']:
            self.api.response = subprocess.CompletedProcess([], 0, json.dumps(dict(peer, status=state)), '')
            self.assertFalse(self.backend.local_bfd_up())

    def test_configure_uses_host_sockets_without_host_routing_mutation(self):
        self.backend.configure()
        calls = [args for args, _ in self.api.calls]
        created = [args for args in calls if args[0] == 'create_tunnel_link']
        self.assertEqual(len(created), 3)
        self.assertTrue(all('vxlan' in args and 'nolearning' in args for args in created))
        data = next(args for args in created if '11001' in args)
        self.assertNotIn('remote', data)
        self.assertTrue(all(args[5:9] == ('ip', '-j', 'link', 'show')
                            for args in calls if args[0] == 'nsenter'))
        self.assertEqual(self.api.links[(False, 'eth0')]['master'], 'gvpc-vrf')
        self.assertIn(('ip', 'route', 'replace', 'vrf', 'gvpc-vrf', '10.252.30.0/27',
                       'via', '10.253.30.1', 'dev', 'eth0'), calls)
        # Remote tenant routes are exclusively zebra's EVPN responsibility.
        self.assertFalse(any('10.252.40.0/24' in args and args[:2] == ('ip', 'route') for args in calls))
        self.assertIn(('ip', 'route', 'del', 'vrf', 'gvpc-vrf', 'default'), calls)
        self.backend.cleanup()
        self.assertEqual(self.api.links, {(False, 'eth0'): {'ifindex': 2, 'ifname': 'eth0'}})

    def test_foreign_local_resource_prevents_any_mutation(self):
        self.api.links[(False, 'gvpc-br')] = {'ifname': 'gvpc-br', 'ifalias': 'foreign'}
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            self.backend.configure()
        self.assertFalse(any(args[:3] == ('ip', 'link', 'add') for args, _ in self.api.calls))
        self.assertNotIn('master', self.api.links[(False, 'eth0')])

    def test_link_read_error_and_malformed_reply_are_never_absence(self):
        for result in [subprocess.CompletedProcess([], 2, '', 'Operation not permitted'),
                       subprocess.CompletedProcess([], 1, '', 'Netlink unavailable'),
                       subprocess.CompletedProcess([], 0, '[]', ''),
                       subprocess.CompletedProcess([], 0, '{}', '')]:
            with self.subTest(result=result), self.assertRaises(RuntimeError):
                _link_record(result)
        self.assertIsNone(_link_record(subprocess.CompletedProcess([], 1, '', 'Device does not exist.')))

    def test_foreign_host_staging_resource_prevents_owned_cleanup(self):
        staged = self.backend.links[0]['staged']
        self.api.links[(True, staged)] = {'ifname': staged, 'ifalias': 'foreign'}
        self.api.links[(False, self.backend.vrf)] = {'ifname': self.backend.vrf,
                                                   'ifalias': dict(self.backend.local)[self.backend.vrf]}
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            self.backend.configure()
        self.assertIn((False, self.backend.vrf), self.api.links)

    def test_foreign_eth0_master_is_never_reparented(self):
        self.api.links[(False, 'eth0')]['master'] = 'unrelated-vrf'
        with self.assertRaisesRegex(RuntimeError, 'foreign EVPN tenant interface master'):
            self.backend.configure()
        self.assertEqual(self.api.links[(False, 'eth0')]['master'], 'unrelated-vrf')

    def test_cleanup_attempts_remaining_resources_after_one_delete_fails(self):
        self.backend.configure()
        failed = self.backend.links[0]['interface']
        self.api.delete_failures.add(failed)
        with self.assertRaisesRegex(RuntimeError, 'cleanup incomplete'):
            self.backend.cleanup()
        self.assertIn((False, failed), self.api.links)
        self.assertNotIn((False, self.backend.data['interface']), self.api.links)
        self.assertNotIn((False, self.backend.vrf), self.api.links)
        self.assertNotIn('master', self.api.links[(False, 'eth0')])

    def test_same_pod_restart_rebuilds_only_owned_resources(self):
        self.backend.configure()
        self.backend.configure()
        self.assertEqual(self.api.links[(False, 'eth0')]['master'], 'gvpc-vrf')
        self.backend.cleanup()
        self.backend.cleanup()
        self.assertEqual(len(self.api.links), 1)


class OverlayDeletionTests(unittest.TestCase):
    def test_foreign_link_is_preserved(self):
        api = FakeAPI()
        api.links[(False, 'foreign')] = {'ifname': 'foreign', 'ifindex': 8, 'ifalias': 'other'}
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            delete_owned_link(api, 'foreign', 'wanted')
        self.assertIn((False, 'foreign'), api.links)
        self.assertFalse(any(args[:3] == ('ip', 'link', 'delete') for args, _ in api.calls))

    def test_unknown_read_prevents_delete(self):
        api = FakeAPI()
        for response in [subprocess.CompletedProcess([], 2, '', 'Operation not permitted'),
                         subprocess.CompletedProcess([], 0, '[{"ifname":"test"}]', ''),
                         subprocess.CompletedProcess([], 0, '[]', '')]:
            with self.subTest(response=response), patch.object(api, 'run', return_value=response) as run:
                with self.assertRaises(RuntimeError):
                    delete_owned_link(api, 'test', 'wanted')
                self.assertEqual(run.call_count, 1)

    def test_lost_delete_response_is_resolved_by_verified_absence(self):
        api = FakeAPI()
        api.links[(True, 'staged')] = {'ifname': 'staged', 'ifindex': 8, 'ifalias': 'wanted'}
        original = api.run

        def lose_response(*args, **options):
            result = original(*args, **options)
            if 'delete' in args:
                raise subprocess.TimeoutExpired(args, 15)
            return result

        with patch.object(api, 'run', side_effect=lose_response):
            delete_owned_link(api, 'staged', 'wanted', host=True)
        self.assertNotIn((True, 'staged'), api.links)
        self.assertTrue(all(args[:5] == ('nsenter', '--target', '1', '--net', '--') for args, _ in api.calls))

    def test_failed_delete_is_not_claimed_as_cleanup(self):
        api = FakeAPI()
        api.links[(False, 'test')] = {'ifname': 'test', 'ifindex': 8, 'ifalias': 'wanted'}
        api.delete_failures.add('test')
        with self.assertRaisesRegex(RuntimeError, 'deletion unconfirmed'):
            delete_owned_link(api, 'test', 'wanted')
        self.assertIn((False, 'test'), api.links)


if __name__ == '__main__':
    unittest.main()
