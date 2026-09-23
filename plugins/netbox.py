#!/usr/bin/env python3
"""NetBox REST provider for explicit, retained prefix claims.

This prototype uses externally provisioned VRFs and API credentials. It requires
one writer per scope; NetBox REST does not provide an atomic claim-key upsert.
"""
import ipaddress
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


class NetBox:
    def __init__(self):
        self.url = os.environ.get('GVPC_NETBOX_URL', 'http://192.168.100.18:30980').rstrip('/')
        self.token = Path(os.environ['GVPC_NETBOX_TOKEN_FILE']).read_text().strip()

    def request(self, path, data=None):
        req = urllib.request.Request(self.url+'/api/'+path,
              data=None if data is None else json.dumps(data).encode(),
              headers={'Authorization': 'Bearer '+self.token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(req, timeout=20) as response:
            return json.load(response)

    def prefixes(self, vrf_id):
        path = f'ipam/prefixes/?vrf_id={int(vrf_id)}&limit=1000'
        items = []
        while path:
            result = self.request(path)
            items.extend(result['results'])
            next_url = result.get('next')
            if next_url and not next_url.startswith(self.url+'/api/'):
                raise ValueError('Unexpected pagination origin')
            path = next_url[len(self.url+'/api/'):] if next_url else None
        return items

    def execute(self, request):
        operation = request['operation']
        if operation == 'Capabilities':
            return {'provider': 'netbox', 'operations': ['Capabilities', 'EnsurePrefix', 'GetPrefix', 'ReleasePrefix'],
                    'scope': 'vrf', 'releasePolicy': 'Retain', 'atomicClaimUpsert': False}
        if operation == 'ReleasePrefix':
            raise ValueError('ReleaseDeferred: external reservations are retained')
        if operation not in ('EnsurePrefix', 'GetPrefix'):
            raise ValueError('Unsupported operation')
        vrf_id = int(request['scopeRef'])
        scope = self.request(f'ipam/vrfs/{vrf_id}/')
        scope_key = 'gvpc-scope:'+request['vpcUID']
        if scope.get('description') != scope_key:
            raise ValueError('ScopeOwnershipConflict')
        claim_key = 'gvpc-claim:'+request['claimUID']
        wanted = ipaddress.ip_network(request['cidr'], strict=True)
        items = self.prefixes(vrf_id)
        owned = [item for item in items if item['description'] == claim_key]
        if len(owned) > 1:
            raise ValueError('DuplicateClaim')
        if owned:
            allocation = owned[0]
            if allocation['prefix'] != str(wanted):
                raise ValueError('ImmutableClaimConflict')
        else:
            if operation == 'GetPrefix':
                raise ValueError('ClaimNotFound')
            if any(wanted.overlaps(ipaddress.ip_network(item['prefix'])) for item in items):
                raise ValueError('PrefixConflict')
            allocation = self.request('ipam/prefixes/', {'prefix': str(wanted), 'vrf': vrf_id,
                    'status': 'reserved', 'description': claim_key})
        return {'provider': 'netbox', 'allocationID': str(allocation['id']), 'scopeRef': str(vrf_id),
                'claimUID': request['claimUID'], 'vpcUID': request['vpcUID'], 'cidr': allocation['prefix'],
                'releasePolicy': 'Retain'}


def main():
    request = json.load(sys.stdin)
    response = {'apiVersion': 'ipam.globalvpc.io/v1alpha1'}
    try:
        if request.get('apiVersion') != response['apiVersion']:
            raise ValueError('Unsupported contract version')
        response.update(ok=True, result=NetBox().execute(request))
    except urllib.error.HTTPError as error:
        response.update(ok=False, error=f'NetBoxHTTP{error.code}')
    except (urllib.error.URLError, TimeoutError):
        response.update(ok=False, error='NetBoxUnavailable')
    except (ValueError, KeyError) as error:
        response.update(ok=False, error=str(error))
    print(json.dumps(response))


if __name__ == '__main__':
    main()
