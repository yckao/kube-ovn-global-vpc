"""Managed subnet advertisement and local-versus-remote readiness contracts."""
import copy
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

from gateway import gateway as runtime
from gateway import managed
from gateway.evpn import EvpnBackend
from tests.test_site_evpn import FakeAPI, configuration
from tests.test_site_transports import configuration as transport_configuration


class ManagedPrefixTests(unittest.TestCase):
    def test_legacy_single_prefix_render_is_unchanged(self):
        for profile in ('wireguard-bgp', 'geneve-bgp'):
            config = transport_configuration('geneve-bgp')
            config['transport']['profile'] = profile
            before = runtime.render_frr_ha(config)
            config['gateway']['cidrs'] = [config['gateway']['cidr']]
            self.assertEqual(before, runtime.render_frr_ha(config))
        config = configuration()
        before = EvpnBackend(config, runtime).render_frr()
        config['gateway']['cidrs'] = [config['gateway']['cidr']]
        self.assertEqual(before, EvpnBackend(config, runtime).render_frr())

    def test_all_local_prefixes_advertise_and_withdraw_together(self):
        for profile in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
            config = configuration() if profile == 'vxlan-evpn' else transport_configuration('geneve-bgp')
            config['transport']['profile'] = profile
            config['gateway']['cidrs'] = [config['gateway']['cidr'], '10.251.90.0/24']
            driver = EvpnBackend(config, runtime) if profile == 'vxlan-evpn' else None
            rendered = driver.render_frr() if driver else runtime.render_frr_ha(config)
            for prefix in config['gateway']['cidrs']:
                self.assertIn('permit '+prefix, rendered)
            for active in (True, False):
                with patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, '', '')) as run:
                    if driver:
                        driver.set_local_advertisement(active)
                    else:
                        runtime.set_local_advertisement(config, active)
                    args = run.call_args.args
                    for prefix in config['gateway']['cidrs']:
                        self.assertIn(('network ' if active else 'no network ')+prefix, args)

    def test_evpn_routes_all_local_subnets_through_native_router(self):
        config = configuration()
        config['gateway']['cidrs'] = [config['gateway']['cidr'], '10.251.90.0/24']
        api = FakeAPI()
        EvpnBackend(config, api).configure()
        for prefix in config['gateway']['cidrs']:
            self.assertTrue(any(args[:6] == ('ip', 'route', 'replace', 'vrf', 'gvpc-vrf', prefix) for args, _ in api.calls))

    def test_empty_and_duplicate_local_prefixes_rejected(self):
        config = configuration()
        for prefixes in ([], ['10.252.30.0/27']*2, ['10.252.30.1/27']):
            config['gateway']['cidrs'] = prefixes
            with self.assertRaises(ValueError):
                runtime.local_prefixes(config)

    def test_no_remote_links_has_valid_frr_maximum_paths(self):
        config = configuration()
        config['transport']['links'] = []
        self.assertIn('maximum-paths 1', EvpnBackend(config, runtime).render_frr())
        self.assertIn('maximum-paths 1', runtime.render_frr_ha(config))


class ReadinessTests(unittest.TestCase):
    def test_local_ready_does_not_depend_on_remote_controller_or_bgp(self):
        config = configuration()
        with patch.object(Path, 'exists', return_value=True), patch.object(Path, 'read_text', return_value='owner-a\n'), patch.object(EvpnBackend, 'local_bfd_up', return_value=True):
            self.assertTrue(managed.local_ready(config))
        with patch.object(Path, 'exists', return_value=True), patch.object(Path, 'read_text', return_value='another-owner\n'):
            self.assertFalse(managed.local_ready(config))

    def test_rollout_requires_direct_remote_nexthop_not_sibling_backup(self):
        config = configuration()
        route = [{'dst': '10.252.40.0/24', 'gateway': '10.253.30.3'}]
        def answer(*args, **kwargs):
            if args[0] == 'ip':
                value = route
            else:
                peer = args[-1].split()[-2]
                value = {peer: {'bgpState': 'Established'}}
            return subprocess.CompletedProcess(args, 0, json.dumps(value), '')
        with patch.object(managed, 'local_ready', return_value=True), patch.object(runtime, 'run', side_effect=answer), patch.object(EvpnBackend, 'remote_bfd_up', return_value=True):
            self.assertFalse(managed.rollout_ready(config))
            route[0] = {'dst': '10.252.40.0/24', 'nexthops': [{'gateway': '192.0.2.2'}, {'gateway': '192.0.2.3'}]}
            self.assertTrue(managed.rollout_ready(config))
            with patch.object(EvpnBackend, 'remote_bfd_up', return_value=False):
                self.assertFalse(managed.rollout_ready(config))

    def test_old_generation_probe_does_not_require_new_subnet(self):
        config = configuration()
        config['transport']['links'] = []
        with patch.object(managed, 'local_ready', return_value=True), patch.object(runtime, 'run', return_value=subprocess.CompletedProcess([], 0, '[]', '')):
            self.assertTrue(managed.rollout_ready(config))

class NativeSocketTests(unittest.TestCase):
    def test_external_cni_socket_is_rejected_and_foreign_vni_is_not_adopted(self):
        config = configuration()
        link = config['transport']['links'][0]
        socket = 'UNCONN 0 0 0.0.0.0:4789 0.0.0.0:*'
        external = {'linkinfo': {'info_kind': 'vxlan', 'info_data': {'port': 4789, 'id': 0, 'external': True}}}
        with self.assertRaisesRegex(RuntimeError, 'another encapsulation'):
            managed.check_native_inventory(config, [external], socket)
        foreign = {'linkinfo': {'info_kind': 'vxlan', 'info_data': {'port': 4789, 'id': link['vni']}}}
        with self.assertRaisesRegex(RuntimeError, 'another owner'):
            managed.check_native_inventory(config, [foreign], socket)

    def test_disjoint_fixed_vni_can_share_kernel_socket_but_userspace_cannot(self):
        config = configuration()
        existing = {'linkinfo': {'info_kind': 'vxlan', 'info_data': {'port': 4789, 'id': 999}}}
        socket = 'UNCONN 0 0 0.0.0.0:4789 0.0.0.0:*'
        managed.check_native_inventory(config, [existing], socket)
        with self.assertRaisesRegex(RuntimeError, 'unknown'):
            managed.check_native_inventory(config, [existing], socket+' users:(("test",pid=4,fd=9))')
        with self.assertRaisesRegex(RuntimeError, 'unknown'):
            managed.check_native_inventory(config, [], socket)


if __name__ == '__main__':
    unittest.main()
