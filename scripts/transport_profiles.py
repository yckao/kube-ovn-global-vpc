"""Administrator-only overlay grants; never discovers or writes a remote site."""
import ipaddress
import re


def number(value, low, high):
    return type(value) is int and low <= value <= high


def ip(value):
    parsed = ipaddress.IPv4Address(value)
    if (type(value) is not str or str(parsed) != value or parsed.is_multicast or
            parsed.is_unspecified or parsed.is_loopback or parsed.is_link_local or parsed.is_reserved):
        raise ValueError('overlay address must be canonical unicast IPv4')
    return parsed


def network(value):
    result = ipaddress.IPv4Network(value, strict=True)
    if type(value) is not str or str(result) != value or not 8 <= result.prefixlen <= 30:
        raise ValueError('overlay delegation must be a canonical IPv4 prefix')
    ip(str(result.network_address))
    return result


def name(value):
    return isinstance(value, str) and len(value) <= 63 and re.fullmatch(r'[a-z0-9](?:[-a-z0-9]*[a-z0-9])?', value)


def node_name(value):
    return isinstance(value, str) and len(value) <= 253 and all(name(label) for label in value.split('.'))


def target(value):
    if not isinstance(value, str) or not re.fullmatch(r'[1-9][0-9]*:(?:0|[1-9][0-9]*)', value):
        raise ValueError('EVPN RD and RT require ASN16:number32 form')
    left, right = map(int, value.split(':'))
    if not 1 <= left <= 65535 or not 0 <= right <= 4294967295:
        raise ValueError('EVPN RD or RT is outside ASN16:number32 range')


def endpoint(value):
    host, port = value.rsplit(':', 1)
    ip(host)
    if not port.isascii() or not port.isdigit() or str(int(port)) != port or not 1024 <= int(port) <= 65535:
        raise ValueError('invalid overlay endpoint port')
    return host, int(port)


def validate_overlay(gateway, grant, transit, delegated):
    profile = grant['profile']
    if set(grant) != {'globalVpcID', 'profile', 'trustedUnderlay', 'gateways'} or grant['trustedUnderlay'] is not True:
        raise ValueError('unencrypted overlays require an explicit trusted underlay grant')
    members, configured = gateway.get('gateways'), grant['gateways']
    if not isinstance(members, list) or not 2 <= len(members) <= 8 or not isinstance(configured, list):
        raise ValueError('overlay HA requires two through eight gateways')
    ids, addresses = set(), {str(ip(gateway['routerIP']))}
    for member in members:
        if set(member) != {'id', 'ip'} or not name(member['id']) or member['id'] in ids:
            raise ValueError('invalid overlay member identity')
        value = ip(member['ip'])
        if value not in transit or value in (transit.network_address, transit.broadcast_address) or str(value) in addresses:
            raise ValueError('invalid overlay transit address')
        ids.add(member['id']); addresses.add(str(value))
    if len(configured) != len(members) or {m['id'] for m in configured} != ids:
        raise ValueError('overlay members do not match gateway grant')
    bfd = gateway.get('bfd', {})
    if not isinstance(bfd, dict) or set(bfd) - {'sourceIP', 'minRX', 'minTX', 'multiplier', 'nodeSelector'}:
        raise ValueError('invalid overlay BFD fields')
    if 'nodeSelector' in bfd and not isinstance(bfd['nodeSelector'], dict):
        raise ValueError('invalid overlay BFD node selector')
    source = ip(bfd['sourceIP'])
    if str(source) in addresses or not all(number(bfd[f], 100, 60000) for f in ('minRX', 'minTX')) or not number(bfd['multiplier'], 2, 255):
        raise ValueError('invalid overlay BFD parameters')
    nodes, asns, routers, underlays, link_ids, controls, rds, vnis, rts = (set() for _ in range(9))
    owners, claims_by_member, health_owners, local_health = {}, [], {}, set()
    remote_underlays, data_ports = set(), set()
    for member in configured:
        allowed = {'id', 'nodeName', 'localASN', 'routerID', 'mtu', 'links'}
        allowed |= ({'underlayIP'} if profile == 'geneve-bgp' else
                    {'localVtepIP', 'healthIP', 'vni', 'routeDistinguisher', 'routeTarget', 'vxlanPort'})
        if set(member) - allowed or not node_name(member['nodeName']) or member['nodeName'] in nodes:
            raise ValueError('overlay gateways require distinct registered nodes and supported fields')
        nodes.add(member['nodeName'])
        if not number(member['localASN'], 1, 4294967294) or not number(member.get('mtu', 1380), 1280, 9000):
            raise ValueError('invalid overlay ASN or MTU')
        asns.add(member['localASN'])
        router = str(ip(member['routerID']))
        outer = str(ip(member['underlayIP'] if profile == 'geneve-bgp' else member['localVtepIP']))
        if router in routers or outer in underlays:
            raise ValueError('overlay gateway identities must be distinct')
        routers.add(router); underlays.add(outer)
        if profile == 'vxlan-evpn':
            if not number(member['vni'], 1, 16777215) or not number(member.get('vxlanPort', 4789), 1024, 65535):
                raise ValueError('invalid EVPN VNI or UDP port')
            target(member['routeDistinguisher']); target(member['routeTarget'])
            if member['routeDistinguisher'] in rds:
                raise ValueError('EVPN gateway route distinguishers must be unique')
            rds.add(member['routeDistinguisher']); rts.add(member['routeTarget']); vnis.add(member['vni'])
            data_ports.add(member.get('vxlanPort', 4789))
            health = str(ip(member['healthIP']))
            if health in health_owners and health_owners[health] != outer or health in local_health:
                raise ValueError('EVPN health addresses require distinct gateway ownership')
            health_owners[health] = outer; local_health.add(health)
        if not isinstance(member['links'], list) or not member['links']:
            raise ValueError('overlay gateways require direct routing links')
        claims, endpoints = {}, set()
        for link in member['links']:
            fields = {'id', 'listenPort', 'localTunnelIP', 'tunnelIP', 'endpoint', 'asn', 'remoteSiteID', 'remoteAttachmentID', 'delegatedPrefixes', 'vni'}
            if profile == 'vxlan-evpn':
                fields.add('remoteHealthIP')
            if set(link) != fields:
                raise ValueError('unsupported overlay link fields')
            if not name(link['id']) or link['id'] in link_ids or not name(link['remoteSiteID']) or not name(link['remoteAttachmentID']):
                raise ValueError('invalid overlay peer identity')
            link_ids.add(link['id'])
            host, port = endpoint(link['endpoint'])
            if not number(link['listenPort'], 1024, 65535) or port != link['listenPort'] or not number(link['vni'], 1, 16777215):
                raise ValueError('native overlay peers require matching UDP ports and valid VNI')
            if host == outer or host in endpoints:
                raise ValueError('duplicate or local overlay peer endpoint')
            endpoints.add(host)
            remote_underlays.add(host)
            if profile == 'vxlan-evpn':
                health = str(ip(link['remoteHealthIP']))
                if health in health_owners and health_owners[health] != host or health in local_health:
                    raise ValueError('EVPN remote health identity conflicts')
                if any(h != health and v == host for h, v in health_owners.items()):
                    raise ValueError('EVPN VTEP has inconsistent health identities')
                health_owners[health] = host
            if not number(link['asn'], 1, 4294967294) or link['asn'] == member['localASN']:
                raise ValueError('overlay peers require distinct remote ASNs')
            for field in ('localTunnelIP', 'tunnelIP'):
                control = str(ip(link[field]))
                if control in controls or control in addresses or control == str(source):
                    raise ValueError('overlay links require distinct control addresses')
                controls.add(control)
            if not isinstance(link['delegatedPrefixes'], list) or not link['delegatedPrefixes']:
                raise ValueError('overlay peer delegation is required')
            pools = tuple(sorted(str(network(p)) for p in link['delegatedPrefixes']))
            if len(set(pools)) != len(pools):
                raise ValueError('duplicate overlay prefix delegation')
            owner = (link['remoteSiteID'], link['remoteAttachmentID'])
            claim = (link['asn'], pools)
            if owner == (gateway['siteID'], gateway['attachmentID']) or owner in owners and owners[owner] != claim:
                raise ValueError('overlay prefix or ASN ownership changed')
            owners[owner] = claim; claims[owner] = claim
        claims_by_member.append(claims)
    if len(asns) != 1 or any(c != claims_by_member[0] for c in claims_by_member):
        raise ValueError('overlay siblings must share local ASN and remote delegated owners')
    if profile == 'vxlan-evpn' and (len(vnis) != 1 or len(rts) != 1 or len(data_ports) != 1):
        raise ValueError('EVPN siblings must share the tenant VNI, route target and data port')
    seen = [transit]
    for pool in delegated + [network(p) for _, pools in owners.values() for p in pools]:
        if any(pool.overlaps(previous) for previous in seen):
            raise ValueError('overlapping overlay prefix ownership')
        seen.append(pool)
    if controls & underlays or remote_underlays & (underlays | controls | addresses | {str(source)}):
        raise ValueError('overlay underlay overlaps a local or control identity')
    if set(health_owners) & (controls | underlays | remote_underlays | addresses | {str(source)}):
        raise ValueError('EVPN health address overlaps control identity')
    for value in controls | underlays | remote_underlays | set(health_owners) | {str(source)}:
        if any(ip(value) in pool for pool in seen):
            raise ValueError('overlay control or underlay address overlaps tenant allocation')
    for member in configured:
        for link in member['links']:
            if endpoint(link['endpoint'])[0] in underlays or endpoint(link['endpoint'])[0] in controls:
                raise ValueError('remote overlay VTEP conflicts with a local or control address')


def validate_socket_allocations(config):
    """Catch shared-host transport collisions before any local resource mutation.

    This covers the complete administrator registration. Independent registrations
    still require a shared platform allocation policy; live kernel conflicts fail
    closed and are never resolved by removing another owner's device.
    """
    ports, identifiers = {}, set()
    for grant in config['grants']:
        profile = grant.get('profile', 'wireguard-bgp')
        if profile not in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
            raise ValueError('unsupported registered transport profile')
        for member in grant.get('gateways', [grant]):
            node = member['nodeName']
            slots = [(link['listenPort'], None if profile == 'wireguard-bgp' else link['vni']) for link in member.get('links', [])]
            if 'listenPort' in member:
                slots.append((member['listenPort'], None))
            if profile == 'vxlan-evpn':
                slots.append((member.get('vxlanPort', 4789), member['vni']))
            for port, vni in slots:
                key = (node, port)
                if key in ports and (ports[key] != profile or profile == 'wireguard-bgp'):
                    raise ValueError('registered transports collide on a host UDP socket')
                ports[key] = profile
                identity = (node, port, vni)
                if identity in identifiers:
                    raise ValueError('registered overlay VNI or listener collision')
                identifiers.add(identity)
