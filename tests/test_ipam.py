"""Failure and ownership semantics of the external IPAM contract."""
import json
import sys
import unittest
from gvpc.ipam import ExecProvider, IPAMError
from plugins.netbox import NetBox


class FakeNetBox(NetBox):
    def __init__(self):
        self.items = {1: [], 2: []}
        self.next_id = 1
        self.lose_response = False

    def request(self, path, data=None):
        if path.startswith('ipam/vrfs/'):
            return {'description': 'gvpc-scope:vpc-'+path.split('/')[2]}
        if path == 'ipam/prefixes/':
            obj = {'id': self.next_id, **data}
            self.next_id += 1
            self.items[data['vrf']].append(obj)
            if self.lose_response:
                self.lose_response = False
                raise TimeoutError('Server committed before response loss')
            return obj
        raise AssertionError(path)

    def prefixes(self, vrf_id):
        return self.items[vrf_id]


def claim(scope=1, uid='site-a', cidr='10.240.1.0/28'):
    return {'operation': 'EnsurePrefix', 'scopeRef': str(scope), 'vpcUID': 'vpc-'+str(scope),
            'claimUID': uid, 'cidr': cidr}


class IPAMTests(unittest.TestCase):
    def test_retry_recovers_committed_allocation_after_lost_response(self):
        provider = FakeNetBox()
        provider.lose_response = True
        with self.assertRaises(TimeoutError):
            provider.execute(claim())
        receipt = provider.execute(claim())
        self.assertEqual(receipt['allocationID'], '1')
        self.assertEqual(len(provider.items[1]), 1)

    def test_same_cidr_isolated_by_vpc_scope(self):
        provider = FakeNetBox()
        first = provider.execute(claim(1))
        second = provider.execute(claim(2))
        self.assertNotEqual(first['allocationID'], second['allocationID'])
        with self.assertRaisesRegex(ValueError, 'PrefixConflict'):
            provider.execute(claim(1, 'another-claim'))

    def test_claim_cannot_silently_change_prefix(self):
        provider = FakeNetBox()
        provider.execute(claim())
        with self.assertRaisesRegex(ValueError, 'ImmutableClaimConflict'):
            provider.execute(claim(cidr='10.240.2.0/28'))

    def test_scope_ownership_is_required(self):
        request = claim()
        request['vpcUID'] = 'another-vpc'
        with self.assertRaisesRegex(ValueError, 'ScopeOwnershipConflict'):
            FakeNetBox().execute(request)

    def test_release_is_explicitly_deferred(self):
        with self.assertRaisesRegex(ValueError, 'ReleaseDeferred'):
            FakeNetBox().execute({'operation':'ReleasePrefix'})

    def test_provider_crash_does_not_expose_stderr_or_imply_absence(self):
        provider = ExecProvider([sys.executable, '-c', 'import sys; print("private-diagnostic",file=sys.stderr);sys.exit(1)'])
        with self.assertRaises(IPAMError) as result:
            provider.call('GetPrefix')
        self.assertNotIn('private-diagnostic', str(result.exception))
        self.assertIn('preserve', str(result.exception))

    def test_misrouted_provider_response_is_rejected(self):
        response={'apiVersion':'ipam.globalvpc.io/v1alpha1','ok':True,'result':{
            'claimUID':'claim','vpcUID':'wrong-vpc','scopeRef':'1','cidr':'10.240.1.0/28',
            'allocationID':'1','releasePolicy':'Retain'}}
        provider=ExecProvider([sys.executable,'-c','print('+repr(json.dumps(response))+')'])
        with self.assertRaisesRegex(IPAMError,'wrong identity'):
            provider.call('EnsurePrefix',claimUID='claim',vpcUID='right-vpc',scopeRef='1',cidr='10.240.1.0/28')


if __name__ == '__main__':
    unittest.main()
