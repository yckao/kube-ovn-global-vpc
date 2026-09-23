"""Deletion rollout regression and compatibility with the installed v1 probe."""
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from gateway import gateway as runtime
from gateway import managed
from gateway.evpn import EvpnBackend
from tests.test_site_evpn import configuration

spec = importlib.util.spec_from_file_location(
    'rollout_probe', Path(__file__).resolve().parents[1] /
    'internal/managedgateway/rollout_probe.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def config(profile):
    value = configuration()
    value['managedVersion'] = 'v1'
    value['gateway']['transitCIDR'] = '10.253.30.0/29'
    value['transport']['profile'] = profile
    value['transport']['localASN'] = 4200000001
    for link in value['transport']['links']:
        link.update(remoteSiteID='site-b', remoteAttachmentID='site-b')
        link['delegatedPrefixes'].append('10.252.41.0/24')
    return managed.validate(value)


class RolloutWithdrawalTests(unittest.TestCase):
    def check(self, installed, desired, prefixes, *, local=True, remote=True,
              bgp=True, sibling_only=False):
        calls = []
        def answer(*args, **kwargs):
            calls.append(args)
            if args[0] == 'ip':
                hops = [link['endpoint'].rsplit(':', 1)[0]
                        if installed['transport']['profile'] == 'vxlan-evpn'
                        else link['tunnelIP']
                        for link in installed['transport']['links']]
                if sibling_only:
                    hops = ['10.253.30.3']
                value = [{'dst': prefix, 'nexthops': [{'gateway': hop} for hop in hops]}
                         for prefix in prefixes]
            elif args[2] == 'bgpd':
                peer = args[-1].split()[3]
                value = {peer: {'bgpState': 'Established' if bgp else 'Idle'}}
            else:
                words = args[-1].split()
                value = {'peer': words[3], 'local': words[6], 'multihop': True,
                         'status': 'up' if remote else 'down'}
            return subprocess.CompletedProcess(args, 0, json.dumps(value), '')
        with patch.object(managed, 'local_ready', return_value=local), \
                patch.object(runtime, 'run', side_effect=answer), \
                patch.object(EvpnBackend, 'remote_bfd_up', return_value=remote):
            filtered = probe.retained_config(installed, desired)
            ready = managed.rollout_ready(filtered)
        return ready, calls

    def test_withdrawn_subnet_does_not_deadlock_old_generation(self):
        for profile in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
            with self.subTest(profile=profile):
                old = config(profile)
                desired = copy.deepcopy(old)
                for link in desired['transport']['links']:
                    link['delegatedPrefixes'].remove('10.252.41.0/24')
                # Reproduces the old probe's permanent failure after source ack:
                # installed config expects a route the source already withdrew.
                self.assertFalse(self.check(old, old, ['10.252.40.0/24'])[0])
                self.assertTrue(self.check(old, desired, ['10.252.40.0/24'])[0])
                self.assertFalse(self.check(old, desired, [])[0])
                self.assertFalse(self.check(old, desired, ['10.252.40.0/24'], sibling_only=True)[0])
                self.assertFalse(self.check(old, desired, ['10.252.40.0/24'], remote=False)[0])
                self.assertFalse(self.check(old, desired, ['10.252.40.0/24'], bgp=False)[0])
                self.assertFalse(self.check(old, desired, ['10.252.40.0/24'], local=False)[0])
                self.assertEqual(old, config(profile), 'installed input was modified')

    def test_explicit_peer_withdrawal_keeps_local_bfd_mandatory(self):
        for profile in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
            old = config(profile)
            desired = copy.deepcopy(old)
            desired['transport']['links'] = []
            ready, calls = self.check(old, desired, [], remote=False, bgp=False)
            self.assertTrue(ready)
            self.assertFalse(any(args[0] == 'vtysh' for args in calls))
            self.assertFalse(self.check(old, desired, [], local=False)[0])

    def test_retained_peer_without_old_prefixes_still_needs_direct_health(self):
        old = config('wireguard-bgp')
        desired = copy.deepcopy(old)
        for link in desired['transport']['links']:
            link['delegatedPrefixes'] = ['10.252.42.0/24']
        self.assertTrue(self.check(old, desired, [])[0])
        self.assertFalse(self.check(old, desired, [], remote=False)[0])
        self.assertFalse(self.check(old, desired, [], bgp=False)[0])

    def test_one_withdrawn_peer_does_not_exempt_the_surviving_peer(self):
        old = config('wireguard-bgp')
        desired = copy.deepcopy(old)
        desired['transport']['links'] = desired['transport']['links'][1:]
        prefixes = ['10.252.40.0/24', '10.252.41.0/24']
        ready, calls = self.check(old, desired, prefixes)
        self.assertTrue(ready)
        neighbors = [args[-1] for args in calls if args[0] == 'vtysh' and args[2] == 'bgpd']
        self.assertEqual(neighbors, ['show bgp neighbors 10.254.110.4 json'])
        self.assertFalse(self.check(old, desired, prefixes, remote=False)[0])
        self.assertFalse(self.check(old, desired, [], remote=True)[0])

    def test_new_delegation_required_after_replacement_becomes_survivor(self):
        old = config('geneve-bgp')
        desired = copy.deepcopy(old)
        for link in desired['transport']['links']:
            link['delegatedPrefixes'].append('10.252.42.0/24')
        existing = ['10.252.40.0/24', '10.252.41.0/24']
        self.assertTrue(self.check(old, desired, existing)[0])
        self.assertFalse(self.check(desired, desired, existing)[0])
        self.assertTrue(self.check(desired, desired, existing + ['10.252.42.0/24'])[0])

    def test_wrong_sibling_or_owner_is_rejected(self):
        old = config('vxlan-evpn')
        for section, key in (('gateway', 'gatewayID'), ('gateway', 'ownerUID'),
                             ('transport', 'profile')):
            desired = copy.deepcopy(old)
            desired[section][key] = 'changed'
            with self.assertRaises(ValueError):
                probe.retained_config(old, desired)
        desired = copy.deepcopy(old)
        desired['transport']['links'][0]['remoteAttachmentID'] = 'foreign'
        with self.assertRaises(ValueError):
            probe.retained_config(old, desired)

    def test_inline_adapter_uses_only_legacy_installed_runtime_api(self):
        old = config('wireguard-bgp')
        desired = copy.deepcopy(old)
        desired['transport']['links'] = []
        # This stub deliberately provides no new CLI or desired-aware API.
        observed = []
        legacy = SimpleNamespace(validate=managed.validate,
                                 rollout_ready=lambda value: observed.append(value) or True)
        stdin = SimpleNamespace(buffer=io.BytesIO(json.dumps(desired).encode()))
        with patch.dict(sys.modules, {'managed': legacy}), patch.object(sys, 'stdin', stdin), \
                patch.object(Path, 'read_text', return_value=json.dumps(old)):
            self.assertEqual(probe.main(), 0)
        self.assertEqual(observed[0]['gateway'], old['gateway'])
        self.assertEqual(observed[0]['transport']['links'], [])

    def test_inline_payload_is_bounded_and_validated_before_health_check(self):
        for payload in (b'{', b'x' * (probe.MAX_CONFIG_BYTES + 1), b'null'):
            legacy = SimpleNamespace(validate=managed.validate)
            stdin = SimpleNamespace(buffer=io.BytesIO(payload))
            with patch.dict(sys.modules, {'managed': legacy}), patch.object(sys, 'stdin', stdin):
                with self.assertRaises((ValueError, AttributeError)):
                    probe.main()


if __name__ == '__main__':
    unittest.main()
