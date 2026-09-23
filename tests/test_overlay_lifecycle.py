"""Regression checks for transport selection and fail-closed ACL lifecycle."""
from contextlib import ExitStack
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

from gateway import overlay
from tests.test_site_gateway import FakeBackend, FakeCluster, ha_registration, ha_request
from tests.test_site_transports import configuration


class OverlayLifecycleTests(unittest.TestCase):
    def test_member_cannot_override_validated_wireguard_profile(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            for explicit_grant in (False, True):
                with self.subTest(profile=profile, explicit_grant=explicit_grant):
                    config, cluster = ha_registration(), FakeCluster()
                    grant = config['grants'][0]
                    if explicit_grant:
                        grant['profile'] = 'wireguard-bgp'
                    for member in grant['gateways']:
                        member.update(profile=profile, trustedUnderlay=True,
                                      underlayIP='192.0.2.11')
                        for link in member['links']:
                            link['vni'] = 10001
                    with self.assertRaises(ValueError):
                        FakeBackend(cluster, config, ha_request()).run()
                    self.assertEqual(cluster.calls, [])
                    self.assertEqual(cluster.resources, {})

    def test_wireguard_overlay_fields_are_rejected_at_every_scope(self):
        for location, values in [
                ('grant', {'trustedUnderlay': True}),
                ('member', {'localVtepIP': '192.0.2.11'}),
                ('member', {'profile': 'wireguard-bgp'}),
                ('link', {'vni': 10001}),
                ('link', {'remoteHealthIP': '10.254.121.1'})]:
            with self.subTest(location=location, values=values):
                config, cluster = ha_registration(), FakeCluster()
                target = config['grants'][0]
                if location in ('member', 'link'):
                    target = target['gateways'][0]
                if location == 'link':
                    target = target['links'][0]
                target.update(values)
                with self.assertRaises(ValueError):
                    FakeBackend(cluster, config, ha_request()).run()
                self.assertEqual(cluster.calls, [])

    def _lifecycle(self, profile, configure_error=False, cleanup_error=False, verify_error=False):
        config, events, handlers = configuration(profile), [], {}
        driver, api = MagicMock(), MagicMock()
        driver.interfaces = ['tunnel-test']
        driver.bridge = 'bridge-test'
        driver.links = [dict(link, interface='link-' + str(index))
                        for index, link in enumerate(config['transport']['links'])]
        driver.render_frr.return_value = 'frr defaults traditional\n'

        def configure():
            events.append('configure')
            if configure_error:
                raise ValueError('partial setup failed')

        def cleanup():
            events.append('delete-links')
            if cleanup_error:
                raise RuntimeError('owned link deletion unconfirmed')

        driver.configure.side_effect = configure
        driver.cleanup.side_effect = cleanup
        def verify():
            events.append('verify-local-frr')
            if verify_error:
                raise RuntimeError('native L3VNI missing')
        driver.verify_local_frr.side_effect = verify
        api.failure_code.return_value = 'runtime-failure'
        api.install_bfd_compat.side_effect = lambda _: events.append('install-bfd')
        api.cleanup_bfd_compat.side_effect = lambda _: events.append('cleanup-bfd')

        def loaded():
            events.append('load-routing')
            handlers[overlay.signal.SIGTERM]()

        api.load_runtime_config.side_effect = loaded
        processes = [MagicMock() for _ in range(4 if profile == 'vxlan-evpn' else 3)]
        for process in processes:
            process.poll.return_value = None
        failure = None
        with ExitStack() as stack:
            stack.enter_context(patch.object(overlay, 'require_tenant_namespace'))
            stack.enter_context(patch.object(overlay, 'backend', return_value=driver))
            stack.enter_context(patch.object(overlay, 'host_preflight'))
            def path_object(path):
                item = MagicMock()
                if str(path) == '/run/gateway-ready':
                    item.write_text.side_effect = lambda _: events.append('ready')
                return item
            stack.enter_context(patch.object(overlay, 'Path', side_effect=path_object))
            stack.enter_context(patch.object(overlay.shutil, 'chown'))
            stack.enter_context(patch.object(overlay.time, 'sleep'))
            stack.enter_context(patch.object(overlay.signal, 'signal',
                side_effect=lambda number, handler: handlers.update({number: handler})))
            def start(command):
                daemon = command[0].rsplit('/', 1)[-1]
                events.append('start-' + daemon)
                if daemon == 'mgmtd':
                    self.assertNotIn('-f', command)
                else:
                    self.assertIn('-f', command)
                return processes.pop(0)
            stack.enter_context(patch.object(overlay.subprocess, 'Popen', side_effect=start))
            stack.enter_context(patch.object(overlay, 'install_source_policy',
                side_effect=lambda *args: events.append('install-policy')))
            stack.enter_context(patch.object(overlay, 'cleanup_source_policy',
                side_effect=lambda *args: events.append('delete-policy')))
            stack.enter_context(patch('builtins.print'))
            try:
                overlay.main(config, api)
            except (RuntimeError, ValueError) as error:
                failure = error
        return events, failure

    def test_policy_exists_before_any_tunnel_setup_for_both_profiles(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            with self.subTest(profile=profile):
                events, failure = self._lifecycle(profile)
                self.assertIsNone(failure)
                self.assertLess(events.index('install-policy'), events.index('configure'))
                self.assertLess(events.index('configure'), events.index('load-routing'))

    def test_policy_remains_until_owned_links_are_deleted(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            with self.subTest(profile=profile):
                events, failure = self._lifecycle(profile)
                self.assertIsNone(failure)
                self.assertLess(events.index('delete-links'), events.index('delete-policy'))

    def test_unconfirmed_tunnel_cleanup_retains_source_policy(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            for partial_setup in (False, True):
                with self.subTest(profile=profile, partial_setup=partial_setup):
                    events, failure = self._lifecycle(profile, partial_setup, cleanup_error=True)
                    self.assertIsInstance(failure, RuntimeError)
                    self.assertIn('install-policy', events)
                    self.assertIn('delete-links', events)
                    self.assertNotIn('delete-policy', events)

    def test_host_namespace_rejection_calls_no_mutation_or_cleanup(self):
        for profile in ('geneve-bgp', 'vxlan-evpn'):
            api = MagicMock()
            with self.subTest(profile=profile), ExitStack() as stack:
                stack.enter_context(patch.object(overlay.os, 'stat', return_value=SimpleNamespace(st_ino=123)))
                backend = stack.enter_context(patch.object(overlay, 'backend'))
                files = stack.enter_context(patch.object(overlay, 'Path'))
                policy = stack.enter_context(patch.object(overlay, 'cleanup_source_policy'))
                with self.assertRaisesRegex(RuntimeError, 'tenant network namespace'):
                    overlay.main(configuration(profile), api)
                self.assertEqual(api.mock_calls, [])
                backend.assert_not_called()
                files.assert_not_called()
                policy.assert_not_called()

    def test_evpn_supervises_management_before_loading_and_checks_mapping(self):
        events, failure = self._lifecycle('vxlan-evpn')
        self.assertIsNone(failure)
        self.assertLess(events.index('start-mgmtd'), events.index('start-zebra'))
        self.assertLess(events.index('load-routing'), events.index('verify-local-frr'))
        self.assertLess(events.index('verify-local-frr'), events.index('ready'))
        geneve, failure = self._lifecycle('geneve-bgp')
        self.assertIsNone(failure)
        self.assertNotIn('start-mgmtd', geneve)
        self.assertNotIn('verify-local-frr', geneve)

    def test_missing_native_l3vni_never_publishes_ready(self):
        events, failure = self._lifecycle('vxlan-evpn', verify_error=True)
        self.assertIsInstance(failure, RuntimeError)
        self.assertNotIn('ready', events)
        self.assertIn('delete-links', events)


if __name__ == '__main__':
    unittest.main()
