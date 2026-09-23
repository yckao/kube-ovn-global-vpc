"""Native Geneve setup and recovery with explicit host/tenant ownership."""
import unittest

from gateway.overlay import GeneveBackend
from tests.test_site_evpn import FakeAPI
from tests.test_site_transports import configuration


class GeneveRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        self.driver = GeneveBackend(configuration(), self.api)

    def test_configure_and_cleanup_keep_host_operations_to_owned_links(self):
        self.driver.configure()
        self.assertEqual(set(self.driver.interfaces),
                         {name for (host, name) in self.api.links if not host and name != 'eth0'})
        host_calls = [args[5:] for args, _ in self.api.calls if args[0] == 'nsenter']
        self.assertTrue(all(c[:3] in (('ip', '-j', 'link'), ('ip', 'link', 'add'), ('ip', 'link', 'set'))
                            for c in host_calls))
        creations = [c for c in host_calls if c[:3] == ('ip', 'link', 'add')]
        self.assertEqual(len(creations), 2)
        self.assertTrue(all('geneve' in c and 'remote' in c and 'dstport' in c for c in creations))
        self.assertTrue(all('local' not in c for c in creations))
        self.assertIn((('ip', 'route', 'replace', 'unreachable', 'default', 'metric', '42760'), {}), self.api.calls)
        self.driver.cleanup()
        self.assertEqual(set(self.api.links), {(False, 'eth0')})

    def test_same_pod_restart_rebuilds_alias_marked_links(self):
        self.driver.configure()
        first = {name: self.api.links[(False, name)]['ifindex'] for name in self.driver.interfaces}
        self.driver.configure()
        self.assertTrue(all(self.api.links[(False, name)]['ifindex'] != index for name, index in first.items()))
        self.assertFalse(any(host for host, _ in self.api.links))
        for link in self.driver.links:
            self.assertEqual(self.api.links[(False, link['interface'])]['ifalias'], link['alias'])

    def test_partial_namespace_move_is_recovered_only_with_owner_alias(self):
        link = self.driver.links[0]
        for host in (False, True):
            with self.subTest(host=host):
                self.api.links[(host, link['staged'])] = {'ifname': link['staged'], 'ifindex': 20,
                                                         'ifalias': link['alias']}
                self.driver.configure()
                self.assertNotIn((host, link['staged']), self.api.links)
                self.driver.cleanup()

    def test_unmarked_staging_link_is_preserved_and_cleanup_reports_it(self):
        link = self.driver.links[1]
        foreign = {'ifname': link['staged'], 'ifindex': 20}
        self.api.links[(True, link['staged'])] = foreign.copy()
        with self.assertRaisesRegex(RuntimeError, 'foreign'):
            self.driver.configure()
        with self.assertRaisesRegex(RuntimeError, 'cleanup incomplete'):
            self.driver.cleanup()
        self.assertEqual(self.api.links[(True, link['staged'])], foreign)
        self.assertEqual(set(self.api.links), {(False, 'eth0'), (True, link['staged'])})

    def test_cleanup_continues_after_one_foreign_link(self):
        self.driver.configure()
        first = self.driver.links[0]
        self.api.links[(False, first['interface'])]['ifalias'] = 'foreign-owner'
        with self.assertRaisesRegex(RuntimeError, 'cleanup incomplete'):
            self.driver.cleanup()
        self.assertEqual(set(self.api.links), {(False, 'eth0'), (False, first['interface'])})


if __name__ == '__main__':
    unittest.main()
