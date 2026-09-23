"""Read-only rollout adapter compatible with the installed v1 managed runtime.

The controller embeds this module and supplies each survivor's desired rendered
configuration on stdin. The installed configuration remains authoritative for
actual interfaces and next hops. Only explicitly withdrawn peers and prefixes
are removed from a memory copy before calling its existing native health probe.
"""
import copy
import json
from pathlib import Path
import sys

MAX_CONFIG_BYTES = 1 << 20


def retained_config(installed, desired):
    """Keep old-generation traffic that is still delegated to the same peer.

    Added peers/prefixes cannot be required of a generation that predates them.
    Once its replacement is a survivor, these are present in both configurations
    and become mandatory before the next retirement. Retained peers still need
    BGP/BFD even when all their old prefixes have explicitly been withdrawn.
    """
    for key in ('ownerUID', 'gatewayID', 'globalVpcID', 'siteID', 'attachmentID',
                'vpcName', 'transitName', 'transitCIDR', 'routerIP', 'gatewayIP'):
        if installed['gateway'].get(key) != desired['gateway'].get(key):
            raise ValueError('rollout gateway identity changed')
    if installed['gateway']['bfd']['sourceIP'] != desired['gateway']['bfd']['sourceIP']:
        raise ValueError('rollout local BFD identity changed')
    for key in ('profile', 'localASN', 'routerID', 'healthIP', 'vni', 'vxlanPort',
                'routeTarget', 'routeDistinguisher', 'localVtepIP', 'underlayIP'):
        if installed['transport'].get(key) != desired['transport'].get(key):
            raise ValueError('rollout transport identity changed')
    wanted = {link['id']: link for link in desired['transport']['links']}
    if len(wanted) != len(desired['transport']['links']):
        raise ValueError('duplicate desired peer identity')
    result = copy.deepcopy(installed)
    retained = []
    for old in result['transport']['links']:
        new = wanted.get(old['id'])
        if new is None:
            continue
        for key in ('remoteSiteID', 'remoteAttachmentID'):
            if old[key] != new[key]:
                raise ValueError('rollout delegation owner changed')
        old['delegatedPrefixes'] = [prefix for prefix in old['delegatedPrefixes']
                                   if prefix in new['delegatedPrefixes']]
        retained.append(old)
    result['transport']['links'] = retained
    return result


def main():
    sys.path.insert(0, '/app')
    import managed
    data = sys.stdin.buffer.read(MAX_CONFIG_BYTES + 1)
    if len(data) > MAX_CONFIG_BYTES:
        raise ValueError('desired configuration is too large')
    desired = managed.validate(json.loads(data))
    installed = managed.validate(json.loads(Path('/config/gateway.json').read_text()))
    config = retained_config(installed, desired)
    return 0 if managed.rollout_ready(config) else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception:
        # Never print configuration, peer arguments, or subprocess output.
        sys.exit(1)
