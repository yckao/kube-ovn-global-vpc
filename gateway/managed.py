#!/usr/bin/env python3
"""Managed binding runtime adapter; only local native networking is mutated.

Public membership and allocations are supplied by the Kubernetes operator. This
process never reads a remote controller/API and never writes OVN databases.
"""
import ipaddress
import json
import os
from pathlib import Path
import re
import sys

try:
    from . import gateway as runtime
    from . import overlay
except ImportError:
    import gateway as runtime
    import overlay


def validate(config):
    if config.get('managedVersion') != 'v1':
        raise ValueError('unsupported managed runtime contract')
    g, t = config['gateway'], config['transport']
    for key in ('ownerUID', 'gatewayID', 'globalVpcID'):
        if not isinstance(g[key], str) or not re.fullmatch(r'[a-z0-9][a-z0-9.-]{0,252}', g[key]):
            raise ValueError('invalid gateway identity')
    prefixes = [ipaddress.IPv4Network(p, strict=True) for p in runtime.local_prefixes(config)]
    transit = ipaddress.IPv4Network(g['transitCIDR'], strict=True)
    local = ipaddress.IPv4Address(g['gatewayIP'])
    router = ipaddress.IPv4Address(g['routerIP'])
    source = ipaddress.IPv4Address(g['bfd']['sourceIP'])
    if local not in transit or router not in transit or local == router or source in transit:
        raise ValueError('invalid transit or local BFD address')
    if any(p.overlaps(transit) or source in p for p in prefixes):
        raise ValueError('local prefix overlaps infrastructure')
    for i, p in enumerate(prefixes):
        if any(p.overlaps(q) for q in prefixes[i+1:]):
            raise ValueError('overlapping local prefixes')
    if t['profile'] not in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
        raise ValueError('unknown managed transport')
    if not 4200000000 <= t['localASN'] <= 4294967294 or not 1280 <= t['mtu'] <= 1400:
        raise ValueError('invalid location ASN or MTU')
    controls, ids, endpoints = set(), set(), set()
    if t['profile'] != 'wireguard-bgp' and t.get('trustedUnderlay') is not True:
        raise ValueError('native transport requires a trusted underlay')
    for link in t['links']:
        if link['id'] in ids:
            raise ValueError('duplicate link identity')
        ids.add(link['id'])
        host, port = link['endpoint'].rsplit(':', 1)
        ipaddress.IPv4Address(host)
        if not port.isdigit() or not 1024 <= int(port) <= 65535 or not 1024 <= link['listenPort'] <= 65535:
            raise ValueError('invalid transport port')
        if t['profile'] != 'wireguard-bgp' and (link['listenPort'] != int(port) or not 1 <= link['vni'] <= 16777215):
            raise ValueError('invalid native tunnel allocation')
        for value in (link['localTunnelIP'], link['tunnelIP']):
            ip = ipaddress.IPv4Address(value)
            if value in controls or ip in transit or any(ip in p for p in prefixes):
                raise ValueError('control address collision')
            controls.add(value)
        owner = (link['remoteSiteID'], link['remoteAttachmentID'])
        for prefix in link['delegatedPrefixes']:
            remote = ipaddress.IPv4Network(prefix, strict=True)
            if remote.overlaps(transit) or any(remote.overlaps(p) for p in prefixes):
                raise ValueError('remote prefix overlaps local ownership')
            for old, old_owner in endpoints:
                if remote.overlaps(old) and old_owner != owner:
                    raise ValueError('remote prefix owner collision')
            endpoints.add((remote, owner))
    if any(ipaddress.IPv4Address(c) in p for c in controls for p, _ in endpoints):
        raise ValueError('control address overlaps remote prefix')
    if t['profile'] == 'vxlan-evpn':
        health = [ipaddress.IPv4Address(t['healthIP'])] + [ipaddress.IPv4Address(l['remoteHealthIP']) for l in t['links']]
        if len(set(health)) != len(health) or not 1 <= t['vni'] <= 16777215:
            raise ValueError('invalid EVPN health or data VNI allocation')
        if any(h in transit or str(h) in controls or any(h in p for p in prefixes) or any(h in p for p, _ in endpoints) for h in health):
            raise ValueError('EVPN health overlaps tenant or control addressing')
    return config


def check_native_inventory(config, interfaces, sockets):
    """Allow shared fixed-mode sockets only with disjoint VNIs; never adopt links."""
    t = config['transport']
    driver = overlay.backend(config, runtime)
    wanted = {(int(l['listenPort']), int(l['vni'])): l['alias'] for l in driver.links}
    kind = 'geneve' if t['profile'] == 'geneve-bgp' else 'vxlan'
    if t['profile'] == 'vxlan-evpn':
        wanted[(t['vxlanPort'], t['vni'])] = driver.data['alias']
    ports = {port for port, _ in wanted}
    kernel_ports = set()
    for interface in interfaces:
        info = interface.get('linkinfo', {})
        data = info.get('info_data', {})
        port = data.get('port')
        if port not in ports:
            continue
        if info.get('info_kind') != kind or data.get('external') is True:
            raise RuntimeError('native port is occupied by another encapsulation owner')
        if not isinstance(data.get('id'), int):
            raise RuntimeError('unknown native socket ownership')
        kernel_ports.add(port)
        expected = wanted.get((port, data['id']))
        if expected is not None and interface.get('ifalias') != expected:
            raise RuntimeError('native VNI is occupied by another owner')
    for line in sockets.splitlines():
        fields = line.split()
        if len(fields) < 5:
            raise RuntimeError('invalid UDP socket inventory')
        try:
            port = int(fields[3].rsplit(':', 1)[1])
        except (ValueError, IndexError):
            raise RuntimeError('invalid UDP socket address')
        if port in ports and (port not in kernel_ports or 'users:(' in line):
            raise RuntimeError('native UDP socket ownership is unknown')


def native_preflight(config):
    # Interfaces move away from their UDP birth namespace. Inspect each current
    # host process namespace, holding descriptors so exit cannot swap identities.
    # This is read-only; kernel create still arbitrates a concurrent bind/VNI race.
    namespaces = {}
    interfaces = []
    try:
        for process in Path('/proc').iterdir():
            if not process.name.isdigit():
                continue
            try:
                descriptor = os.open(process/'ns/net', os.O_RDONLY)
            except FileNotFoundError:
                continue
            inode = os.fstat(descriptor).st_ino
            if inode in namespaces:
                os.close(descriptor)
            else:
                namespaces[inode] = descriptor
        if not namespaces:
            raise RuntimeError('host network namespace inventory is empty')
        for descriptor in namespaces.values():
            path = '/proc/'+str(os.getpid())+'/fd/'+str(descriptor)
            records = json.loads(runtime.run('nsenter', '--net='+path, '--', 'ip', '-j', '-d', 'link', timeout=3).stdout)
            if not isinstance(records, list):
                raise RuntimeError('invalid native interface inventory')
            interfaces.extend(records)
        sockets = runtime.run('nsenter', '--target', '1', '--net', '--', 'ss', '-H', '-lunp', timeout=3).stdout
        check_native_inventory(config, interfaces, sockets)
    finally:
        for descriptor in namespaces.values():
            os.close(descriptor)


def local_ready(config):
    sentinel = Path('/run/gateway-ready')
    if not sentinel.exists() or sentinel.read_text().strip() != config['gateway']['ownerUID']:
        return False
    if config['transport']['profile'] == 'wireguard-bgp':
        return runtime.local_bfd_up(config)
    return overlay.backend(config, runtime).local_bfd_up()


def rollout_ready(config):
    """Require direct peer health, not a route recursively using the retiring sibling."""
    if not local_ready(config):
        return False
    t = config['transport']
    driver = overlay.backend(config, runtime) if t['profile'] != 'wireguard-bgp' else None
    routes_command = ['ip', '-j', 'route', 'show']
    if t['profile'] == 'vxlan-evpn':
        routes_command += ['vrf', driver.vrf]
    routes = json.loads(runtime.run(*routes_command, timeout=2).stdout)
    for link in t['links']:
        command = 'show bgp neighbors ' + link['tunnelIP'] + ' json'
        peers = json.loads(runtime.run('vtysh', '-d', 'bgpd', '-c', command, timeout=2).stdout)
        if peers.get(link['tunnelIP'], {}).get('bgpState') != 'Established':
            return False
        if t['profile'] == 'vxlan-evpn':
            if not driver.remote_bfd_up(link):
                return False
            expected_hop = link['endpoint'].rsplit(':', 1)[0]
        else:
            command = 'show bfd peer '+link['tunnelIP']+' multihop local-address '+link['localTunnelIP']+' json'
            peer = json.loads(runtime.run('vtysh', '-d', 'bfdd', '-c', command, timeout=2).stdout)
            if (peer.get('peer') != link['tunnelIP'] or peer.get('local') != link['localTunnelIP'] or
                    peer.get('multihop') is not True or peer.get('status') != 'up'):
                return False
            expected_hop = link['tunnelIP']
        for prefix in link['delegatedPrefixes']:
            matches = [r for r in routes if r.get('dst') == prefix and r.get('type', 'unicast') == 'unicast']
            if len(matches) != 1:
                return False
            route = matches[0]
            # iproute2 resolves nhid groups into nexthops in normal route JSON.
            # Unknown shapes remain a blocked rollout, not an assumed proof.
            hops = [route.get('gateway')] + [hop.get('gateway') for hop in route.get('nexthops', [])]
            if expected_hop not in hops:
                return False
    return True


def main():
    config = validate(json.loads(Path('/config/gateway.json').read_text()))
    if len(sys.argv) > 1:
        if sys.argv[1] == '--ready':
            return 0 if local_ready(config) else 1
        if sys.argv[1] == '--rollout-ready':
            return 0 if rollout_ready(config) else 1
        if sys.argv[1] == '--stats':
            value = runtime.counters(config) if config['transport']['profile'] == 'wireguard-bgp' else overlay.stats(config, runtime)
            print(json.dumps(value))
            return 0
        raise ValueError('unknown managed runtime command')
    overlay.require_tenant_namespace()
    if config['transport']['profile'] == 'wireguard-bgp':
        runtime.main_ha(config)
    else:
        native_preflight(config)
        overlay.main(config, runtime)
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as error:
        # Do not expose full subprocess arguments or private runtime material.
        print(json.dumps({'event': 'managed-runtime-error', 'type': type(error).__name__}), file=sys.stderr)
        sys.exit(1)
