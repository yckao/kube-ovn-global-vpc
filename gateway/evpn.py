"""Native FRR EVPN Type-5 gateway confined to one tenant Pod namespace.

VXLAN sockets are created in the host namespace, then their interfaces move to
the Pod. Linux retains the socket/routing namespace; no host route, bridge,
firewall or ToR configuration is installed by this backend. Control tunnels are
separate from the multipoint L3VNI. Per-gateway health prefixes remain imported
while workload prefixes are quarantined, permitting BFD over the actual data
VNI to recover independently of workload routing.
"""
import hashlib
import json
import time


def _identity(config, kind):
    gateway = config['gateway']
    value = '\x00'.join((gateway['ownerUID'], gateway['gatewayID'], 'vxlan-evpn', kind))
    digest = hashlib.sha256(value.encode()).hexdigest()
    return digest[:11], 'globalvpc:' + digest


def _control_mac(address):
    return '02:' + ':'.join(hashlib.sha256(address.encode()).hexdigest()[i:i + 2]
                          for i in range(0, 10, 2))


def _link_record(result):
    if result.returncode:
        # Permission errors and failed netlink reads are not absence receipts.
        if result.returncode == 1 and 'does not exist' in result.stderr:
            return None
        raise RuntimeError('EVPN interface inspection failed')
    records = json.loads(result.stdout)
    if not isinstance(records, list) or len(records) != 1 or not isinstance(records[0], dict):
        raise RuntimeError('invalid EVPN interface response')
    return records[0]


class EvpnBackend:
    """The caller owns process supervision and injects guarded link helpers."""

    vrf = 'gvpc-vrf'
    bridge = 'gvpc-br'
    vtep = 'gvpc-vtep'
    health = 'gvpc-health'
    table = '10000'

    def __init__(self, config, api):
        self.config, self.api = config, api
        self.gateway, self.transport = config['gateway'], config['transport']
        self.links = []
        for link in sorted(self.transport['links'], key=lambda value: value['id']):
            suffix, alias = _identity(config, 'control:' + link['id'])
            self.links.append(dict(link, interface='vc' + suffix, staged='gd' + suffix,
                                   alias=alias, remoteVtepIP=link['endpoint'].rsplit(':', 1)[0]))
        suffix, alias = _identity(config, 'data')
        self.data = dict(interface='vl' + suffix, staged='gd' + suffix, alias=alias)
        self.local = [(name, _identity(config, kind)[1]) for name, kind in (
            (self.vrf, 'vrf'), (self.bridge, 'bridge'), (self.vtep, 'vtep'), (self.health, 'health'))]
        self.interfaces = [self.vrf, self.bridge, self.vtep, self.health, self.data['interface']]
        self.interfaces += [link['interface'] for link in self.links]
        self.remote_health = {link['id']: False for link in self.links}

    def _link(self, name):
        result = self.api.run('ip', '-j', 'link', 'show', 'dev', name, check=False)
        return _link_record(result)

    def _check_eth0_master(self):
        eth0, vrf = self._link('eth0'), self._link(self.vrf)
        if not eth0:
            raise RuntimeError('EVPN tenant interface missing')
        master = eth0.get('master')
        if not master:
            return False
        alias = dict(self.local)[self.vrf]
        if (not vrf or vrf.get('ifalias') != alias or
                master not in (self.vrf, vrf.get('ifindex'))):
            raise RuntimeError('foreign EVPN tenant interface master')
        return True

    def _detach_eth0(self):
        if not self._check_eth0_master():
            return
        self.api.run('ip', 'link', 'set', 'dev', 'eth0', 'nomaster')

    def _create_local(self, name, *kind):
        alias = dict(self.local)[name]
        self.api.run('ip', 'link', 'add', 'name', name, 'type', *kind)
        # A create-time alias alone is not reliable on the pinned kernel.
        self.api.run('ip', 'link', 'set', 'dev', name, 'alias', alias)
        self.api.require_owned_link(name, alias)

    def _remove(self, strict=False):
        try:
            from .overlay import delete_owned_link
        except ImportError:
            from overlay import delete_owned_link
        errors = []
        try:
            self._detach_eth0()
        except Exception as error:
            errors.append(error)
        resources = []
        for link in self.links + [self.data]:
            for name, host in ((link['interface'], False), (link['staged'], False),
                               (link['staged'], True)):
                resources.append((name, link['alias'], host))
        for name, alias in reversed(self.local):
            resources.append((name, alias, False))
        for name, alias, host in resources:
            try:
                delete_owned_link(self.api, name, alias, host=host, strict=strict)
            except Exception as error:
                errors.append(error)
        if errors:
            raise RuntimeError('EVPN resource cleanup incomplete') from None

    def configure(self):
        # Check every resource before modifying any existing owned resources.
        for name, alias in self.local:
            existing = self._link(name)
            if existing and existing.get('ifalias') != alias:
                raise RuntimeError('foreign gateway interface')
        for link in self.links + [self.data]:
            for name, host in ((link['interface'], False), (link['staged'], False),
                               (link['staged'], True)):
                prefix = ['nsenter', '--target', '1', '--net', '--'] if host else []
                found = self.api.run(*prefix, 'ip', '-j', 'link', 'show', 'dev', name, check=False)
                if _link_record(found) is not None:
                    self.api.require_owned_link(name, link['alias'], host=host)
        self._check_eth0_master()
        self._remove(strict=True)
        run = self.api.run
        mtu = str(self.transport.get('mtu', 1380))
        self._create_local(self.vrf, 'vrf', 'table', self.table)
        run('ip', 'link', 'set', 'dev', self.vrf, 'up')
        self._create_local(self.vtep, 'dummy')
        run('ip', 'address', 'add', self.transport['localVtepIP'] + '/32', 'dev', self.vtep)
        run('ip', 'link', 'set', 'dev', self.vtep, 'up')
        self._create_local(self.health, 'dummy')
        run('ip', 'link', 'set', 'dev', self.health, 'master', self.vrf, 'up')
        run('ip', 'address', 'add', self.transport['healthIP'] + '/32', 'dev', self.health)
        self._create_local(self.bridge, 'bridge', 'stp_state', '0', 'forward_delay', '0')
        # L3 SVI has a unique stable router MAC, but no IPv4/IPv6 address.
        rmac = _control_mac('router:' + self.transport['routeDistinguisher'])
        run('ip', 'link', 'set', 'dev', self.bridge, 'master', self.vrf,
            'addrgenmode', 'none', 'address', rmac, 'mtu', mtu)
        for link in self.links:
            self.api.create_tunnel_link(link['staged'], link['alias'], 'vxlan',
                'id', str(link['vni']), 'local', self.transport['localVtepIP'],
                'remote', link['remoteVtepIP'], 'dstport', str(link['listenPort']), 'nolearning')
            self.api.move_owned_link(link['staged'], link['interface'], link['alias'])
            run('ip', 'link', 'set', 'dev', link['interface'], 'address',
                _control_mac(link['localTunnelIP']), 'mtu', mtu, 'up')
            run('ip', 'address', 'add', link['localTunnelIP'] + '/32', 'dev', link['interface'])
            run('ip', 'route', 'replace', link['tunnelIP'] + '/32', 'dev', link['interface'])
            run('ip', 'neigh', 'replace', link['tunnelIP'], 'lladdr', _control_mac(link['tunnelIP']),
                'nud', 'permanent', 'dev', link['interface'])
            # Zebra resolves the EVPN next hop in the default VRF. The outer
            # data packet still follows the native VXLAN socket's host FIB.
            run('ip', 'route', 'replace', link['remoteVtepIP'] + '/32',
                'via', link['tunnelIP'], 'dev', link['interface'])
        link = self.data
        self.api.create_tunnel_link(link['staged'], link['alias'], 'vxlan',
            'id', str(self.transport['vni']), 'local', self.transport['localVtepIP'],
            'dstport', str(self.transport.get('vxlanPort', 4789)), 'nolearning')
        self.api.move_owned_link(link['staged'], link['interface'], link['alias'])
        run('ip', 'link', 'set', 'dev', link['interface'], 'master', self.bridge,
            'addrgenmode', 'none', 'mtu', mtu)
        run('ip', 'link', 'set', 'dev', link['interface'], 'type', 'bridge_slave',
            'neigh_suppress', 'on', 'learning', 'off')
        run('ip', 'link', 'set', 'dev', link['interface'], 'up')
        run('ip', 'link', 'set', 'dev', self.bridge, 'up')
        run('ip', 'link', 'set', 'dev', 'eth0', 'master', self.vrf)
        for prefix in self.api.local_prefixes(self.config) + [self.gateway['bfd']['sourceIP'] + '/32']:
            run('ip', 'route', 'replace', 'vrf', self.vrf, prefix,
                'via', self.gateway['routerIP'], 'dev', 'eth0')
        run('ip', 'route', 'del', 'vrf', self.vrf, 'default', check=False)
        run('ip', 'route', 'replace', 'vrf', self.vrf, 'unreachable', 'default', 'metric', '42760')
        # Enslaving eth0 moves connected routes. Remove the old CNI default
        # from both namespaces of routing, retaining fail-closed defaults.
        run('ip', 'route', 'del', 'default', check=False)
        run('ip', 'route', 'replace', 'unreachable', 'default', 'metric', '42760')

    def cleanup(self):
        self._remove()

    def verify_local_frr(self):
        # FRR 10.2.1 sends the VRF/VNI YANG command to mgmtd. An integrated
        # load can silently skip an absent daemon, so command success alone
        # does not prove that zebra classified the kernel tunnel as an L3VNI.
        # This check is entirely local: remote sites need not be reachable.
        expected = {'vni': self.transport['vni'], 'type': 'L3',
                    'tenantVrf': self.vrf, 'localVtepIp': self.transport['localVtepIP'],
                    'vxlanIntf': self.data['interface'], 'sviIntf': self.bridge, 'state': 'Up'}

        def read(command):
            result = self.api.run('vtysh', '-d', 'zebra', '-c', command, timeout=2)
            if result.returncode or result.stderr.strip() or len(result.stdout) > 65536:
                raise RuntimeError('EVPN local routing response invalid')
            record = json.loads(result.stdout)
            if not isinstance(record, dict):
                raise RuntimeError('EVPN local routing response invalid')
            return record

        # Zebra's management backend applies asynchronously. Bound retries and
        # each subprocess deadline, rather than waiting indefinitely for Ready.
        for attempt in range(12):
            vni = read(f'show evpn vni {self.transport["vni"]} json')
            if (type(vni.get('vni')) is int and
                    all(vni.get(key) == value for key, value in expected.items())):
                mapping = read(f'show vrf {self.vrf} vni json').get('vrfs')
                if (isinstance(mapping, list) and len(mapping) == 1 and
                        isinstance(mapping[0], dict) and type(mapping[0].get('vni')) is int and
                        all(mapping[0].get(key) == value for key, value in {
                            'vrf': self.vrf, 'vni': self.transport['vni'],
                            'vxlanIntf': self.data['interface'], 'sviIntf': self.bridge,
                            'state': 'Up'}.items())):
                    return vni
            if attempt < 11:
                time.sleep(0.25)
        raise RuntimeError('EVPN local L3VNI not operational')

    def render_frr(self):
        gateway, transport = self.gateway, self.transport
        bfd, siblings = gateway['bfd'], transport.get('siblings', [])
        lines = ['frr defaults traditional', 'hostname global-vpc-gateway', 'log stdout informational',
                 f'vrf {self.vrf}', f' vni {transport["vni"]}', 'exit-vrf',
                 'ip prefix-list LOCAL_HEALTH seq 10 permit ' + transport['healthIP'] + '/32',
                 'bgp extcommunity-list standard TENANT permit rt ' + transport['routeTarget']]
        lines += [f'ip prefix-list LOCAL seq {(i+1)*10} permit {prefix}'
                  for i, prefix in enumerate(self.api.local_prefixes(self.config))]
        pools = sorted({pool for link in self.links for pool in link['delegatedPrefixes']})
        for index, pool in enumerate(pools):
            lines.append(f'ip prefix-list SIBLING seq {(index + 1) * 10} permit {pool} le 32')
        for index, link in enumerate(self.links):
            lines.append(f'ip prefix-list HEALTH{index} seq 10 permit {link["remoteHealthIP"]}/32')
            for number, pool in enumerate(link['delegatedPrefixes']):
                lines.append(f'ip prefix-list REMOTE{index} seq {(number + 1) * 10} permit {pool} le 32')
        lines += ['route-map LOCAL_EXPORT permit 10', ' match ip address prefix-list LOCAL', 'exit',
                  'route-map LOCAL_EXPORT permit 20', ' match ip address prefix-list LOCAL_HEALTH', 'exit',
                  'route-map EVPN_EXPORT permit 10', ' match evpn route-type prefix',
                  ' match ip address prefix-list LOCAL', ' match extcommunity TENANT', 'exit',
                  'route-map EVPN_EXPORT permit 20', ' match evpn route-type prefix',
                  ' match ip address prefix-list LOCAL_HEALTH', ' match extcommunity TENANT', 'exit']
        for index, _ in enumerate(self.links):
            lines += [f'route-map DATA_READY{index} deny 10', 'exit',
                      f'route-map EVPN_IMPORT{index} permit 10', ' match evpn route-type prefix',
                      f' match ip address prefix-list HEALTH{index}', ' match extcommunity TENANT', 'exit',
                      f'route-map EVPN_IMPORT{index} permit 20', ' match evpn route-type prefix',
                      f' match ip address prefix-list REMOTE{index}', ' match extcommunity TENANT',
                      f' call DATA_READY{index}', 'exit']
        lines += ['bfd', ' profile GVPC', f'  receive-interval {bfd["minRX"]}',
                  f'  transmit-interval {bfd["minTX"]}', f'  detect-multiplier {bfd["multiplier"]}', ' exit',
                  f' peer {bfd["sourceIP"]} local-address {gateway["gatewayIP"]} interface eth0 vrf {self.vrf}',
                  '  profile GVPC', '  no shutdown', ' exit']
        for link in self.links:
            lines += [f' peer {link["tunnelIP"]} multihop local-address {link["localTunnelIP"]}',
                      '  profile GVPC', '  no shutdown', ' exit',
                      f' peer {link["remoteHealthIP"]} multihop local-address {transport["healthIP"]} vrf {self.vrf}',
                      '  profile GVPC', '  no shutdown', ' exit']
        for sibling in siblings:
            lines += [f' peer {sibling["ip"]} local-address {gateway["gatewayIP"]} interface eth0 vrf {self.vrf}',
                      '  profile GVPC', '  no shutdown', ' exit']
        lines += ['exit', f'router bgp {transport["localASN"]}', f' bgp router-id {transport["routerID"]}',
                  ' no bgp default ipv4-unicast', ' bgp ebgp-requires-policy']
        for link in self.links:
            peer = link['tunnelIP']
            lines += [f' neighbor {peer} remote-as {link["asn"]}',
                      f' neighbor {peer} update-source {link["localTunnelIP"]}',
                      f' neighbor {peer} ebgp-multihop 2', f' neighbor {peer} timers 3 9',
                      f' neighbor {peer} bfd profile GVPC', f' neighbor {peer} graceful-restart-disable']
        lines += [' address-family l2vpn evpn', '  advertise-all-vni']
        for index, link in enumerate(self.links):
            peer = link['tunnelIP']
            # FRR 10.2.1 bgpd.c peer defaults already preserve EVPN next hops.
            # Make that contract explicit: session control addresses are not
            # VTEPs and must never replace the native Type-5 next hop.
            # Do not enable inbound soft-reconfiguration: the pinned version
            # has an EVPN overlay lifetime bug in its Adj-RIB-In cache (FRR
            # #20078). Native BGP Route Refresh reapplies the import policy.
            lines += [f'  neighbor {peer} activate', f'  neighbor {peer} route-map EVPN_IMPORT{index} in',
                      f'  neighbor {peer} route-map EVPN_EXPORT out', f'  neighbor {peer} maximum-prefix 256',
                      f'  neighbor {peer} attribute-unchanged next-hop']
        lines += [' exit-address-family', 'exit',
                  f'router bgp {transport["localASN"]} vrf {self.vrf}',
                  f' bgp router-id {transport["routerID"]}', ' no bgp default ipv4-unicast']
        for sibling in siblings:
            peer = sibling['ip']
            lines += [f' neighbor {peer} remote-as {transport["localASN"]}',
                      f' neighbor {peer} update-source {gateway["gatewayIP"]}',
                      f' neighbor {peer} timers 3 9', f' neighbor {peer} bfd profile GVPC',
                      f' neighbor {peer} graceful-restart-disable']
        lines += [' address-family ipv4 unicast', f'  maximum-paths {max(1, min(128, len(self.links)))}',
                  f'  network {transport["healthIP"]}/32']
        for sibling in siblings:
            peer = sibling['ip']
            lines += [f'  neighbor {peer} activate', f'  neighbor {peer} next-hop-self',
                      f'  neighbor {peer} prefix-list SIBLING in', f'  neighbor {peer} prefix-list SIBLING out',
                      f'  neighbor {peer} maximum-prefix 1024']
        lines += [' exit-address-family', ' address-family l2vpn evpn',
                  '  rd ' + transport['routeDistinguisher'],
                  '  route-target both ' + transport['routeTarget'],
                  '  advertise ipv4 unicast route-map LOCAL_EXPORT', ' exit-address-family', 'exit', '']
        return '\n'.join(lines)

    def local_bfd_up(self):
        gateway = self.gateway
        command = (f'show bfd vrf {self.vrf} peer {gateway["bfd"]["sourceIP"]} '
                   f'local-address {gateway["gatewayIP"]} interface eth0 json')
        result = self.api.run('vtysh', '-d', 'bfdd', '-c', command, timeout=2)
        if len(result.stdout) > 65536:
            raise RuntimeError('local BFD response too large')
        peer = json.loads(result.stdout)
        if (not isinstance(peer, dict) or peer.get('peer') != gateway['bfd']['sourceIP'] or
                peer.get('local') != gateway['gatewayIP'] or peer.get('interface') != 'eth0' or
                peer.get('multihop') is not False or peer.get('vrf') != self.vrf or
                peer.get('status') not in ('up', 'down', 'init', 'shutdown')):
            raise RuntimeError('local BFD response identity or status changed')
        return peer['status'] == 'up'

    def set_local_advertisement(self, active):
        commands = []
        for prefix in self.api.local_prefixes(self.config):
            commands += ['-c', ('network ' if active else 'no network ') + prefix]
        result = self.api.run('vtysh', '-d', 'bgpd', '-c', 'configure terminal',
            '-c', f'router bgp {self.transport["localASN"]} vrf {self.vrf}',
            '-c', 'address-family ipv4 unicast', *commands, '-c', 'end', timeout=2)
        if result.stdout.strip() or result.stderr.strip():
            raise RuntimeError('local route advertisement operation was not clean')

    def remote_bfd_up(self, link):
        command = (f'show bfd vrf {self.vrf} peer {link["remoteHealthIP"]} '
                   f'multihop local-address {self.transport["healthIP"]} json')
        result = self.api.run('vtysh', '-d', 'bfdd', '-c', command, timeout=2)
        if len(result.stdout) > 65536:
            raise RuntimeError('remote BFD response too large')
        peer = json.loads(result.stdout)
        if (not isinstance(peer, dict) or peer.get('peer') != link['remoteHealthIP'] or
                peer.get('local') != self.transport['healthIP'] or
                peer.get('multihop') is not True or peer.get('vrf') != self.vrf or
                peer.get('status') not in ('up', 'down', 'init', 'shutdown')):
            raise RuntimeError('remote BFD response identity or status changed')
        return peer['status'] == 'up'

    def refresh_remote_health(self):
        # Read all peers before changing any gate. An ambiguous/error response
        # is fatal to the caller's routing supervisor, never treated as Up.
        observed = [self.remote_bfd_up(link) for link in self.links]
        for index, (link, active) in enumerate(zip(self.links, observed)):
            if active == self.remote_health[link['id']]:
                continue
            action = 'permit' if active else 'deny'
            # Switching a route-map action replaces that clause in FRR. The
            # changed map is deliberately just a gate; peer/prefix/RT matches
            # remain installed in its caller throughout the transition.
            # Without an inbound cache, FRR's soft-in requests the peer's
            # negotiated Route Refresh capability. Unsupported peers produce
            # a command error and cannot be recorded as an applied gate.
            result = self.api.run('vtysh', '-d', 'bgpd', '-c', 'configure terminal',
                '-c', f'route-map DATA_READY{index} {action} 10', '-c', 'end',
                '-c', f'clear bgp l2vpn evpn {link["tunnelIP"]} soft in', timeout=2)
            if result.stdout.strip() or result.stderr.strip():
                raise RuntimeError('EVPN data health operation was not clean')
            self.remote_health[link['id']] = active
        return dict(self.remote_health)

    def stats(self):
        def read(*args):
            result = self.api.run(*args, check=False)
            return json.loads(result.stdout) if result.returncode == 0 and result.stdout.strip() else None
        return {'profile': 'vxlan-evpn', 'vrf': self.vrf,
                'dataHealthMode': 'native-evpn-health-prefix-bfd',
                'routes': read('ip', '-j', 'route', 'show', 'vrf', self.vrf),
                'evpnRoutes': read('vtysh', '-c', 'show bgp l2vpn evpn route json'),
                'evpnVNI': read('vtysh', '-c', 'show evpn vni json'),
                'tenantBGP': read('vtysh', '-c', f'show bgp vrf {self.vrf} ipv4 unicast json'),
                'bfd': read('vtysh', '-c', 'show bfd peers json'),
                'links': [{'id': link.get('id', 'data'), 'interface': link['interface'],
                           'interfaceStats': read('ip', '-j', '-s', 'link', 'show', 'dev', link['interface'])}
                          for link in self.links + [self.data]]}
