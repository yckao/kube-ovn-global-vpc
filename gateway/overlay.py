"""Native host-underlay overlays with tenant-local routing and owned cleanup.

Linux VXLAN/Geneve retain the creation namespace for outer socket I/O. These
adapters create down devices in the host, mark them, and move them into this
gateway namespace. They never change host routes, sysctls, or firewall policy.
"""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time


def identity(config, label):
    raw = config['gateway']['ownerUID']+'\0'+config['gateway']['gatewayID']+'\0'+label
    digest = hashlib.sha256(raw.encode()).hexdigest()
    return digest[:11], 'globalvpc:'+digest


def require_tenant_namespace():
    if os.stat('/proc/self/ns/net').st_ino == os.stat('/proc/1/ns/net').st_ino:
        raise RuntimeError('overlay runtime must use a tenant network namespace')


def host_preflight(config, api):
    require_tenant_namespace()
    transport = config['transport']
    if transport.get('trustedUnderlay') is not True:
        raise RuntimeError('overlay requires trusted underlay registration')
    local = transport.get('underlayIP', transport.get('localVtepIP'))
    host = ['nsenter', '--target', '1', '--net', '--']
    interfaces = json.loads(api.run(*host, 'ip', '-j', 'address', 'show').stdout)
    assigned = {a['local'] for i in interfaces for a in i.get('addr_info', []) if a.get('family') == 'inet'}
    if local not in assigned:
        raise RuntimeError('registered overlay source is not a local host address')
    unavailable = []
    for link in transport['links']:
        remote = link['endpoint'].rsplit(':', 1)[0]
        args = ['ip', '-j', 'route', 'get', remote]
        if transport['profile'] == 'vxlan-evpn':
            args += ['from', local]
        result = api.run(*host, 'env', 'LC_ALL=C', *args, check=False)
        if result.returncode:
            # One remote route may disappear during a site outage. Native
            # tunnel creation does not need that route; BFD/BGP can recover
            # using the host FIB without blocking healthy peers or siblings.
            # Inspection failures are not evidence that a peer is unreachable.
            if (result.returncode == 2 and result.stdout.strip() in ('', '[]') and
                    result.stderr.strip() == 'RTNETLINK answers: Network is unreachable'):
                unavailable.append(link['id'])
                continue
            raise RuntimeError('overlay host route inspection failed')
        routes = json.loads(result.stdout)
        if (isinstance(routes, list) and len(routes) == 1 and isinstance(routes[0], dict) and
                routes[0].get('type') in ('blackhole', 'unreachable', 'prohibit')):
            unavailable.append(link['id'])
            continue
        if len(routes) != 1 or routes[0].get('type', 'unicast') not in ('unicast', 'local') or 'dev' not in routes[0]:
            raise RuntimeError('overlay endpoint has no usable host route')
        route = routes[0]
        if transport['profile'] == 'geneve-bgp' and route.get('prefsrc', route.get('src')) != local:
            # Fixed-mode Geneve cannot select a source address. Do not rewrite
            # the host route to make an invalid registration appear to work.
            raise RuntimeError('Geneve host route source differs from registration')
        devices = json.loads(api.run(*host, 'ip', '-j', 'link', 'show', 'dev', route['dev']).stdout)
        metrics = route.get('metrics', {})
        if isinstance(metrics, list):
            metrics = next((m for m in metrics if 'mtu' in m), {})
        mtu = min(devices[0]['mtu'], metrics.get('mtu', devices[0]['mtu']))
        if transport.get('mtu', 1380)+50 > mtu:
            raise RuntimeError('overlay MTU exceeds observed host path capacity')
    return unavailable


def create_tunnel_link(api, staged, alias, *kind_args):
    host = ['nsenter', '--target', '1', '--net', '--']
    api.run(*host, 'ip', 'link', 'add', 'name', staged, 'type', *kind_args)
    # If create or marking has an unknown outcome, preserve the unmarked link.
    # A later restart must not infer ownership from its generated name alone.
    api.run(*host, 'ip', 'link', 'set', 'dev', staged, 'alias', alias)
    api.require_owned_link(staged, alias, host=True)


def delete_owned_link(api, name, alias, host=False, strict=True):
    """Delete one explicitly owned link and verify absence after unknown writes."""
    prefix = ['nsenter', '--target', '1', '--net', '--'] if host else []

    def inspect():
        result = api.run(*prefix, 'ip', '-j', 'link', 'show', 'dev', name, check=False)
        if result.returncode:
            if result.returncode == 1 and 'does not exist' in result.stderr:
                return None
            raise RuntimeError('overlay interface inspection failed')
        if len(result.stdout) > 65536:
            raise RuntimeError('overlay interface response too large')
        records = json.loads(result.stdout)
        if (not isinstance(records, list) or len(records) != 1 or
                not isinstance(records[0], dict) or records[0].get('ifname') != name or
                type(records[0].get('ifindex')) is not int or records[0]['ifindex'] <= 0):
            raise RuntimeError('invalid overlay interface response')
        return records[0]

    current = inspect()
    if current is None:
        return
    if current.get('ifalias') != alias:
        if strict:
            raise RuntimeError('foreign gateway interface')
        return
    try:
        api.run(*prefix, 'ip', 'link', 'delete', 'dev', name, check=False)
    except (subprocess.TimeoutExpired, subprocess.CalledProcessError):
        # A response can be lost after the kernel commits the deletion.
        pass
    if inspect() is not None:
        raise RuntimeError('overlay interface deletion unconfirmed')


def policy_identity(config):
    suffix, alias = identity(config, 'source-policy')
    return 'gvpc_overlay_'+suffix, alias


def owned_policy(config, api):
    name, alias = policy_identity(config)
    result = api.run('nft', '-j', 'list', 'table', 'inet', name, check=False)
    if result.returncode:
        if result.returncode == 1 and 'No such file or directory' in result.stderr:
            return False
        raise RuntimeError('overlay source policy inspection failed')
    tables = [x['table'] for x in json.loads(result.stdout).get('nftables', []) if 'table' in x]
    if len(tables) != 1 or tables[0].get('name') != name or tables[0].get('comment') != alias:
        raise RuntimeError('foreign overlay source policy')
    return True


def install_source_policy(config, api, interfaces):
    """Bind decapsulated IPv4 sources to administrator-authorized delegations.

    This is not cryptographic authentication: transport endpoints must reside on
    the registered trusted underlay. Unregistered IPv4 and all IPv6 are dropped.
    """
    name, alias = policy_identity(config)
    exists = owned_policy(config, api)
    lines = [f'delete table inet {name}'] if exists else []
    lines += [f'table inet {name} {{', f' comment "{alias}"',
              ' chain source_guard { type filter hook prerouting priority -140; policy accept;']
    for interface, sources in interfaces:
        # Callers pass only generated interface names and validated IP networks.
        addresses = sorted({str(ipaddress.ip_network(p, strict=True)) for p in sources})
        if addresses:
            lines.append(' iifname "'+interface+'" ip saddr { '+', '.join(addresses)+' } counter accept')
        lines.append(' iifname "'+interface+'" counter drop')
    lines += [' }', '}']
    body = '\n'.join(lines)+'\n'
    api.run_input(['nft', '--check', '-f', '-'], body)
    api.run_input(['nft', '-f', '-'], body)


def cleanup_source_policy(config, api):
    if owned_policy(config, api):
        name, _ = policy_identity(config)
        api.run('nft', 'delete', 'table', 'inet', name)


class GeneveBackend:
    def __init__(self, config, api):
        self.config, self.api = config, api
        self.links = []
        for link in sorted(config['transport']['links'], key=lambda x: x['id']):
            suffix, alias = identity(config, 'geneve:'+link['id'])
            self.links.append(dict(link, interface='gn'+suffix, staged='gd'+suffix, alias=alias))
        self.interfaces = [link['interface'] for link in self.links]

    def configure(self):
        api, gateway, transport = self.api, self.config['gateway'], self.config['transport']
        for link in self.links:
            for name, host in ((link['interface'], False), (link['staged'], False), (link['staged'], True)):
                delete_owned_link(api, name, link['alias'], host=host, strict=True)
        for link in self.links:
            remote = link['endpoint'].rsplit(':', 1)[0]
            create_tunnel_link(api, link['staged'], link['alias'], 'geneve', 'id', str(link['vni']),
                               'remote', remote, 'dstport', str(link['listenPort']), 'ttl', '64', 'df', 'set')
            api.move_owned_link(link['staged'], link['interface'], link['alias'])
            api.run('ip', 'address', 'replace', link['localTunnelIP']+'/32', 'dev', link['interface'])
            api.run('ip', 'link', 'set', 'dev', link['interface'], 'mtu', str(transport.get('mtu', 1380)), 'up')
            api.run('ip', 'route', 'replace', link['tunnelIP']+'/32', 'dev', link['interface'])
        for prefix in api.local_prefixes(self.config):
            api.run('ip', 'route', 'replace', prefix, 'via', gateway['routerIP'], 'dev', 'eth0')
        api.run('ip', 'route', 'replace', gateway['bfd']['sourceIP']+'/32', 'via', gateway['routerIP'], 'dev', 'eth0')
        api.run('ip', 'route', 'del', 'default', check=False)
        api.run('ip', 'route', 'replace', 'unreachable', 'default', 'metric', '42760')

    def render_frr(self):
        return self.api.render_frr_ha(self.config)

    def local_bfd_up(self):
        return self.api.local_bfd_up(self.config)

    def set_local_advertisement(self, active):
        self.api.set_local_advertisement(self.config, active)

    def cleanup(self):
        errors = []
        for link in self.links:
            for name, host in ((link['interface'], False), (link['staged'], False), (link['staged'], True)):
                try:
                    delete_owned_link(self.api, name, link['alias'], host=host, strict=True)
                except Exception:
                    errors.append('interface')
        if errors:
            raise RuntimeError('overlay cleanup incomplete')

    def stats(self):
        api = self.api
        return {'links': [{'id': l['id'], 'interface': l['interface'],
                           'interfaceStats': json.loads(api.run('ip', '-j', '-s', 'link', 'show', 'dev', l['interface']).stdout)} for l in self.links],
                'routes': json.loads(api.run('ip', '-j', 'route', 'show', 'proto', 'bgp').stdout)}


def backend(config, api):
    if config['transport']['profile'] == 'geneve-bgp':
        return GeneveBackend(config, api)
    if config['transport']['profile'] == 'vxlan-evpn':
        if __package__:
            from .evpn import EvpnBackend
        else:
            from evpn import EvpnBackend
        return EvpnBackend(config, api)
    raise RuntimeError('unsupported overlay profile')


def main(config, api):
    # A rejected host-network execution must not run tenant cleanup there.
    require_tenant_namespace()
    driver = backend(config, api)
    gateway, transport = config['gateway'], config['transport']
    stop, processes = False, []

    def terminate(*_):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGTERM, terminate)
    signal.signal(signal.SIGINT, terminate)
    stage = 'overlay-preflight'
    try:
        Path('/run/gateway-ready').unlink(missing_ok=True)
        for link_id in host_preflight(config, api):
            print(json.dumps({'event': 'underlay-route-unavailable',
                              'gatewayID': gateway['gatewayID'], 'linkID': link_id}), flush=True)
        stage = 'overlay-source-policy'
        if transport['profile'] == 'geneve-bgp':
            filters = [(link['interface'], link['delegatedPrefixes']+[link['tunnelIP']+'/32'])
                       for link in driver.links]
        else:
            sources = sorted({p for link in transport['links'] for p in link['delegatedPrefixes']} |
                             {link['remoteHealthIP']+'/32' for link in transport['links']})
            filters = [(driver.bridge, sources)] + [(link['interface'], [link['tunnelIP']+'/32'])
                                                    for link in driver.links]
        # Interface names are known before creation. Install the complete policy
        # before any tunnel becomes Up, including same-Pod restart with forwarding
        # already enabled. Preserve it until every owned tunnel is absent.
        install_source_policy(config, api, filters)
        stage = 'overlay-configure'
        driver.configure()
        settings = [('net.ipv4.ip_forward', '1'), ('net.ipv4.fib_multipath_hash_policy', '1')]
        for interface in ['all', 'default', 'eth0']+driver.interfaces:
            settings += [('net.ipv4.conf.'+interface+'.rp_filter', '0'),
                         ('net.ipv4.conf.'+interface+'.send_redirects', '0'),
                         ('net.ipv4.conf.'+interface+'.accept_redirects', '0')]
        for key, value in settings:
            api.run('sysctl', '-w', key+'='+value)
        stage = 'overlay-local-bfd'
        api.install_bfd_compat(config)
        Path('/run/frr').mkdir(exist_ok=True)
        shutil.chown('/run/frr', user='frr', group='frr')
        Path('/run/frr/frr.conf').write_text(driver.render_frr())
        Path('/run/frr/base.conf').write_text('frr defaults traditional\nhostname global-vpc-gateway\nlog stdout informational\n')
        daemons = ('mgmtd', 'zebra', 'bfdd', 'bgpd') if transport['profile'] == 'vxlan-evpn' else ('zebra', 'bfdd', 'bgpd')
        for daemon in daemons:
            stage = 'overlay-start-'+daemon
            command = ['/usr/lib/frr/'+daemon, '-i', '/run/frr/'+daemon+'.pid', '-A', '127.0.0.1']
            # FRR 10.2 routes VRF/VNI configuration through mgmtd. Its CLI has
            # no -f flag; configuration is delivered by the integrated loader
            # after all local daemons are available.
            if daemon != 'mgmtd':
                command += ['-f', '/run/frr/base.conf', '-z', '/run/frr/zserv.api']
            process = subprocess.Popen(command)
            processes.append(process)
            time.sleep(1)
            if process.poll() is not None:
                raise RuntimeError('routing process failed')
        stage = 'overlay-routing-configuration'
        api.load_runtime_config()
        if transport['profile'] == 'vxlan-evpn':
            stage = 'overlay-native-l3vni'
            driver.verify_local_frr()
        Path('/run/gateway-ready').write_text(gateway['ownerUID']+'\n')
        print(json.dumps({'event': 'local-ready', 'gatewayID': gateway['gatewayID'], 'profile': transport['profile']}), flush=True)
        advertised = False
        while not stop:
            stage = 'overlay-routing-health'
            if any(p.poll() is not None for p in processes):
                raise RuntimeError('routing process exited')
            if hasattr(driver, 'refresh_remote_health'):
                driver.refresh_remote_health()
            active = driver.local_bfd_up()
            if active != advertised:
                driver.set_local_advertisement(active)
                advertised = active
                print(json.dumps({'event': 'local-advertisement', 'gatewayID': gateway['gatewayID'], 'active': active}), flush=True)
            time.sleep(max(0.1, min(1, gateway['bfd']['minRX']/1000)))
    except Exception as error:
        print(json.dumps({'event': 'gateway-stage-error', 'stage': stage, 'code': api.failure_code(error)}), file=sys.stderr, flush=True)
        raise
    finally:
        Path('/run/gateway-ready').unlink(missing_ok=True)
        for process in processes:
            if process.poll() is None:
                process.terminate()
        for process in processes:
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill(); process.wait(timeout=3)
        try:
            api.cleanup_bfd_compat(config)
        finally:
            driver.cleanup()
            cleanup_source_policy(config, api)


def stats(config, api):
    result = backend(config, api).stats()
    result.update(profile=config['transport']['profile'], gatewayID=config['gateway']['gatewayID'])
    result['bfd'] = json.loads(api.run('vtysh', '-c', 'show bfd peers json').stdout)
    name, _ = policy_identity(config)
    policy = api.run('nft', '-j', 'list', 'table', 'inet', name, check=False)
    result['sourcePolicy'] = json.loads(policy.stdout) if policy.returncode == 0 else None
    return result
