#!/usr/bin/env python3
"""Local WireGuard/FRR gateway; it never calls a Kubernetes or peer-controller API.

The WireGuard UDP socket is born in the host network namespace, while its
interface and all cleartext tenant routes live only in this Pod's namespace.
"""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import time


def run(*args, check=True, timeout=15):
    return subprocess.run(args, check=check, text=True, capture_output=True, timeout=timeout)


def run_input(args, body, timeout=15):
    return subprocess.run(args, input=body, check=True, text=True, capture_output=True, timeout=timeout)


def create_tunnel_link(staged, alias, *kind_args):
    if __package__:
        from .overlay import create_tunnel_link as create
    else:
        from overlay import create_tunnel_link as create
    return create(sys.modules[__name__], staged, alias, *kind_args)


def local_prefixes(config):
    """Managed bindings may own several exact local subnets; legacy stays singular."""
    gateway = config['gateway']
    values = gateway.get('cidrs', [gateway['cidr']])
    if not isinstance(values, list) or not values:
        raise ValueError('at least one local prefix is required')
    prefixes = sorted({str(ipaddress.IPv4Network(value, strict=True)) for value in values})
    if len(prefixes) != len(values):
        raise ValueError('duplicate local prefixes')
    return prefixes


def render_frr(config):
    if config['gateway']['version']=='v2':
        return render_frr_ha(config)
    gateway, transport = config['gateway'], config['transport']
    lines = ['frr defaults traditional', 'hostname global-vpc-gateway',
             'log stdout informational',
             'ip prefix-list LOCAL seq 10 permit ' + gateway['cidr']]
    for i, peer in enumerate(transport['peers']):
        for j, prefix in enumerate(peer['delegatedPrefixes']):
            lines.append(f'ip prefix-list REMOTE{i} seq {(j+1)*10} permit {prefix} le 32')
    lines.extend([f'router bgp {transport["localASN"]}',
                  f' bgp router-id {transport["localTunnelIP"]}',
                  ' no bgp default ipv4-unicast', ' bgp ebgp-requires-policy'])
    for peer in transport['peers']:
        lines.extend([f' neighbor {peer["tunnelIP"]} remote-as {peer["asn"]}',
                      f' neighbor {peer["tunnelIP"]} update-source {transport["localTunnelIP"]}',
                      f' neighbor {peer["tunnelIP"]} ebgp-multihop 2',
                      f' neighbor {peer["tunnelIP"]} timers 3 9'])
    lines.extend([' address-family ipv4 unicast', '  network '+gateway['cidr']])
    for i, peer in enumerate(transport['peers']):
        lines.extend([f'  neighbor {peer["tunnelIP"]} activate',
                      f'  neighbor {peer["tunnelIP"]} prefix-list REMOTE{i} in',
                      f'  neighbor {peer["tunnelIP"]} prefix-list LOCAL out',
                      f'  neighbor {peer["tunnelIP"]} maximum-prefix 256'])
    lines.extend([' exit-address-family', 'exit', ''])
    return '\n'.join(lines)


def main_legacy(config):
    gateway, transport = config['gateway'], config['transport']
    owner = gateway['ownerUID']
    interface = 'gd' + hashlib.sha256(owner.encode()).hexdigest()[:10]
    alias = 'globalvpc:' + owner
    host = ['nsenter', '--target', '1', '--net', '--']
    processes = []
    stop = False

    def terminate(*_):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGTERM, terminate)
    signal.signal(signal.SIGINT, terminate)
    try:
        # A Pod restart can retain its network namespace. Rebuild only our own
        # interface; no unrelated host interface or route is modified.
        old = run('ip', '-j', 'link', 'show', 'dev', 'wg0', check=False)
        if old.returncode == 0:
            if json.loads(old.stdout)[0].get('ifalias') != alias:
                raise RuntimeError('foreign tenant WireGuard interface')
            run('ip', 'link', 'delete', 'wg0')
        local_staged = run('ip', '-j', 'link', 'show', 'dev', interface, check=False)
        if local_staged.returncode == 0:
            if json.loads(local_staged.stdout)[0].get('ifalias') != alias:
                raise RuntimeError('foreign local staging interface')
            run('ip', 'link', 'delete', interface)
        staged = run(*host, 'ip', '-j', 'link', 'show', 'dev', interface, check=False)
        if staged.returncode == 0:
            if json.loads(staged.stdout)[0].get('ifalias') != alias:
                raise RuntimeError('foreign host staging interface')
            run(*host, 'ip', 'link', 'delete', interface)
        create_owned_link(interface, alias)
        move_owned_link(interface, 'wg0', alias)
        args = ['wg', 'set', 'wg0', 'listen-port', str(transport['listenPort']),
                'private-key', '/keys/privateKey']
        for peer in transport['peers']:
            allowed = [peer['tunnelIP']+'/32'] + peer['delegatedPrefixes']
            args += ['peer', peer['publicKey'], 'endpoint', peer['endpoint'],
                     'allowed-ips', ','.join(allowed), 'persistent-keepalive', '5']
        run(*args)
        run('ip', 'address', 'replace', transport['localTunnelIP']+'/32', 'dev', 'wg0')
        run('ip', 'link', 'set', 'dev', 'wg0', 'mtu', str(transport.get('mtu', 1380)), 'up')
        for peer in transport['peers']:
            run('ip', 'route', 'replace', peer['tunnelIP']+'/32', 'dev', 'wg0')
        run('ip', 'route', 'replace', gateway['cidr'], 'via', gateway['routerIP'], 'dev', 'eth0')
        # Never send an unknown remote prefix back into the OVN default route.
        run('ip', 'route', 'del', 'default', check=False)
        run('ip', 'route', 'replace', 'unreachable', 'default', 'metric', '42760')
        for name, value in [('net.ipv4.ip_forward','1'), ('net.ipv4.conf.all.rp_filter','0'),
                            ('net.ipv4.conf.default.rp_filter','0'), ('net.ipv4.conf.eth0.rp_filter','0'),
                            ('net.ipv4.conf.wg0.rp_filter','0')]:
            run('sysctl', '-w', name+'='+value)
        Path('/run/frr').mkdir(exist_ok=True)
        shutil.chown('/run/frr', user='frr', group='frr')
        Path('/run/frr/frr.conf').write_text(render_frr(config))
        for daemon in ('zebra', 'bgpd'):
            args = ['/usr/lib/frr/'+daemon, '-f', '/run/frr/frr.conf', '-i', '/run/frr/'+daemon+'.pid',
                    '-z', '/run/frr/zserv.api', '-A', '127.0.0.1']
            processes.append(subprocess.Popen(args))
            time.sleep(1)
            if processes[-1].poll() is not None:
                raise RuntimeError('routing process failed')
        Path('/run/gateway-ready').write_text(owner+'\n')
        print(json.dumps({'event':'local-ready','ownerUID':owner,'globalVpcID':gateway['globalVpcID']}), flush=True)
        while not stop:
            if any(process.poll() is not None for process in processes):
                raise RuntimeError('routing process exited')
            time.sleep(1)
    finally:
        Path('/run/gateway-ready').unlink(missing_ok=True)
        for process in processes:
            if process.poll() is None:
                process.terminate()
        for process in processes:
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
        delete_owned_link(interface, alias, host=True)
        current = run('ip', '-j', 'link', 'show', 'dev', 'wg0', check=False)
        if current.returncode == 0 and json.loads(current.stdout)[0].get('ifalias') == alias:
            run('ip', 'link', 'delete', 'wg0', check=False)
        staged = run('ip', '-j', 'link', 'show', 'dev', interface, check=False)
        if staged.returncode == 0 and json.loads(staged.stdout)[0].get('ifalias') == alias:
            run('ip', 'link', 'delete', interface, check=False)


def wireguard_links(config):
    """Each gateway-pair link has a distinct cryptokey routing table."""
    gateway = config['gateway']
    result=[]
    for link in sorted(config['transport']['links'],key=lambda link:link['id']):
        identity=gateway['ownerUID']+'\x00'+gateway['gatewayID']+'\x00'+link['id']
        suffix=hashlib.sha256(identity.encode()).hexdigest()[:11]
        result.append(dict(link, interface='wg'+suffix, staged='gd'+suffix,
                           alias='globalvpc:'+hashlib.sha256(identity.encode()).hexdigest()))
    return result


def render_frr_ha(config):
    gateway, transport = config['gateway'],config['transport']
    links, siblings = wireguard_links(config),transport['siblings']
    bfd=gateway['bfd']
    lines=['frr defaults traditional','hostname global-vpc-gateway','log stdout informational']
    lines += [f'ip prefix-list LOCAL seq {(i+1)*10} permit {prefix}'
              for i, prefix in enumerate(local_prefixes(config))]
    remote_pools=sorted({pool for link in links for pool in link['delegatedPrefixes']})
    for i,pool in enumerate(remote_pools):
        lines.append(f'ip prefix-list SIBLING seq {(i+1)*10} permit {pool} le 32')
    for i,link in enumerate(links):
        for j,pool in enumerate(link['delegatedPrefixes']):
            lines.append(f'ip prefix-list REMOTE{i} seq {(j+1)*10} permit {pool} le 32')
    lines.extend(['bfd',' profile GVPC',f'  receive-interval {bfd["minRX"]}',
                  f'  transmit-interval {bfd["minTX"]}',f'  detect-multiplier {bfd["multiplier"]}',' exit'])
    # OVN's UDP 3784 local session is single-hop even though its explicitly
    # assigned source address is outside the tenant transit subnet.
    lines.extend([f' peer {bfd["sourceIP"]} local-address {gateway["gatewayIP"]} interface eth0',
                  '  profile GVPC','  no shutdown',' exit'])
    for link in links:
        lines.extend([f' peer {link["tunnelIP"]} multihop local-address {link["localTunnelIP"]}',
                      '  profile GVPC','  no shutdown',' exit'])
    for sibling in siblings:
        lines.extend([f' peer {sibling["ip"]} local-address {gateway["gatewayIP"]} interface eth0',
                      '  profile GVPC','  no shutdown',' exit'])
    lines.extend(['exit',f'router bgp {transport["localASN"]}',f' bgp router-id {transport["routerID"]}',
                  ' no bgp default ipv4-unicast',' bgp ebgp-requires-policy'])
    for link in links:
        peer=link['tunnelIP']
        lines.extend([f' neighbor {peer} remote-as {link["asn"]}',
                      f' neighbor {peer} update-source {link["localTunnelIP"]}',
                      f' neighbor {peer} ebgp-multihop 2',f' neighbor {peer} timers 3 9',
                      f' neighbor {peer} bfd profile GVPC',f' neighbor {peer} graceful-restart-disable'])
    for sibling in siblings:
        peer=sibling['ip']
        lines.extend([f' neighbor {peer} remote-as {transport["localASN"]}',
                      f' neighbor {peer} update-source {gateway["gatewayIP"]}',
                      f' neighbor {peer} timers 3 9',
                      f' neighbor {peer} bfd profile GVPC',f' neighbor {peer} graceful-restart-disable'])
    # The local prefix is advertised only after the local OVN BFD session is
    # Up. Keeping its static route alone must not advertise a broken OVN path.
    lines.extend([' address-family ipv4 unicast',f'  maximum-paths {max(1,min(128,len(links)))}'])
    for i,link in enumerate(links):
        peer=link['tunnelIP']
        lines.extend([f'  neighbor {peer} activate',f'  neighbor {peer} prefix-list REMOTE{i} in',
                      f'  neighbor {peer} prefix-list LOCAL out',f'  neighbor {peer} maximum-prefix 256'])
    for sibling in siblings:
        peer=sibling['ip']
        # Normal iBGP split horizon remains enabled. Direct eBGP is preferred;
        # sibling paths provide backup only and never leak remote routes to WAN.
        lines.extend([f'  neighbor {peer} activate',f'  neighbor {peer} next-hop-self',f'  neighbor {peer} prefix-list SIBLING in',
                      f'  neighbor {peer} prefix-list SIBLING out',f'  neighbor {peer} maximum-prefix 1024'])
    lines.extend([' exit-address-family','exit',''])
    return '\n'.join(lines)


def nft_identity(config):
    gateway=config['gateway']
    alias='globalvpc:'+hashlib.sha256((gateway['ownerUID']+'\x00'+gateway['gatewayID']).encode()).hexdigest()
    return 'gvpc_bfd_'+hashlib.sha256(alias.encode()).hexdigest()[:12],alias


def nft_table(config):
    name,alias=nft_identity(config)
    current=run('nft','-j','list','table','inet',name,check=False)
    if current.returncode:
        return None
    table=next((entry['table'] for entry in json.loads(current.stdout)['nftables'] if 'table' in entry),None)
    if not table or table.get('name')!=name or table.get('comment')!=alias:
        raise RuntimeError('foreign local BFD compatibility table')
    return table


def install_bfd_compat(config):
    """Normalize only OVN's known TTL-254 single-hop BFD packets in this Pod.

    Kube-OVN provides an OpenBFDD TTL-254 compatibility patch. This runtime
    normalizes the verified OVN single-hop path because FRR only permits changing
    the minimum TTL for multi-hop sessions, which use a different UDP port. This
    named, owner-checked rule preserves the single-hop port and all other traffic.
    It never enters the host network namespace.
    """
    name,alias=nft_identity(config)
    if nft_table(config):
        run('nft','delete','table','inet',name)
    run('nft','add','table','inet',name,'{ comment "'+alias+'"; }')
    run('nft','add','chain','inet',name,'prerouting','{ type filter hook prerouting priority mangle; policy accept; }')
    gateway=config['gateway']
    run('nft','add','rule','inet',name,'prerouting','iifname','eth0',
        'ip','saddr',gateway['bfd']['sourceIP'],'ip','daddr',gateway['gatewayIP'],
        'udp','dport','3784','ip','ttl','254','counter','ip','ttl','set','255',
        'comment','"ovn-bfd-ttl254"')


def cleanup_bfd_compat(config):
    name,_=nft_identity(config)
    try:
        owned=nft_table(config)
    except RuntimeError:
        return
    if owned:
        run('nft','delete','table','inet',name,check=False)


def delete_owned_link(name, alias, host=False, strict=False):
    prefix=['nsenter','--target','1','--net','--'] if host else []
    current=run(*prefix,'ip','-j','link','show','dev',name,check=False)
    if current.returncode:
        return
    if json.loads(current.stdout)[0].get('ifalias')!=alias:
        if strict:
            raise RuntimeError('foreign gateway interface')
        return
    run(*prefix,'ip','link','delete',name,check=False)


def require_owned_link(name, alias, host=False):
    prefix=['nsenter','--target','1','--net','--'] if host else []
    current=run(*prefix,'ip','-j','link','show','dev',name)
    records=json.loads(current.stdout)
    if (not isinstance(records,list) or len(records)!=1 or
            not isinstance(records[0],dict) or records[0].get('ifalias')!=alias):
        raise RuntimeError('foreign gateway interface')


def create_owned_link(name, alias):
    """Mark only a successfully created interface; never adopt an existing one."""
    host=['nsenter','--target','1','--net','--']
    run(*host,'ip','link','add','name',name,'alias',alias,'type','wireguard')
    # Create-time IFLA_IFALIAS is not reliably applied by the kernel. A lost
    # create response or crash before marking leaves an unowned remnant; the
    # normal strict guard must preserve it and stop, never adopt it by name.
    run(*host,'ip','link','set','dev',name,'alias',alias)
    require_owned_link(name,alias,host=True)


def move_owned_link(staged, interface, alias):
    """Keep ownership explicit across namespace move and rename.

    Linux applies IFLA_IFALIAS after the namespace move in the same request.
    Kernel-side partial failures are not rolled back: unmarked remnants stay
    untouched and prevent restart pending controlled operator cleanup.
    """
    run('nsenter','--target','1','--net','--','ip','link','set','dev',staged,
        'netns',str(os.getpid()),'alias',alias)
    for name in (staged,interface):
        require_owned_link(name,alias)
        if name==staged:
            run('ip','link','set','dev',staged,'name',interface,'alias',alias)


def local_bfd_up(config):
    """Read one exact local session; ambiguity or a daemon error is fatal."""
    gateway=config['gateway']
    command='show bfd peer '+gateway['bfd']['sourceIP']+' local-address '+gateway['gatewayIP']+' interface eth0 json'
    result=run('vtysh','-d','bfdd','-c',command,timeout=2)
    if len(result.stdout)>65536:
        raise RuntimeError('local BFD response too large')
    peer=json.loads(result.stdout)
    if (not isinstance(peer,dict) or peer.get('peer')!=gateway['bfd']['sourceIP'] or
            peer.get('local')!=gateway['gatewayIP'] or peer.get('interface')!='eth0' or
            peer.get('multihop') is not False or peer.get('vrf')!='default' or
            peer.get('status') not in ('up','down','init','shutdown')):
        raise RuntimeError('local BFD response identity or status changed')
    return peer['status']=='up'


def set_local_advertisement(config, active):
    gateway,transport=config['gateway'],config['transport']
    commands=[]
    for prefix in local_prefixes(config):
        commands += ['-c', ('network ' if active else 'no network ')+prefix]
    result=run('vtysh','-d','bgpd','-c','configure terminal',
               '-c','router bgp '+str(transport['localASN']),'-c','address-family ipv4 unicast',
               *commands,'-c','end',timeout=2)
    if result.stdout.strip() or result.stderr.strip():
        raise RuntimeError('local route advertisement operation was not clean')


def refresh_local_advertisement(config, advertised):
    active=local_bfd_up(config)
    if active!=advertised:
        set_local_advertisement(config,active)
        print(json.dumps({'event':'local-advertisement','gatewayID':config['gateway']['gatewayID'],
                          'active':active}),flush=True)
    return active


def load_runtime_config():
    # Daemons cannot reliably parse other daemons' integrated configuration
    # nodes. vtysh dispatches the complete file to the correct local daemons.
    # FRR 10's forked loader writes successful child-progress lines to both
    # stdout and stderr. Synchronous dispatch keeps success output empty and
    # lets every unexpected warning remain a fail-closed configuration error.
    result=run('vtysh','--no-fork','-f','/run/frr/frr.conf',timeout=10)
    if result.stdout.strip() or result.stderr.strip():
        raise RuntimeError('integrated routing configuration was not clean')


def failure_code(error):
    """Emit only fixed categories, never arbitrary exception text or arguments."""
    known={
        'foreign gateway interface':'interface-ownership-conflict',
        'foreign local BFD compatibility table':'bfd-compatibility-ownership-conflict',
        'integrated routing configuration was not clean':'frr-configuration-output',
        'local route advertisement operation was not clean':'frr-advertisement-output',
        'local BFD response too large':'bfd-response-size',
        'local BFD response identity or status changed':'bfd-response-identity',
        'routing process failed':'routing-process-startup',
        'routing process exited':'routing-process-exit',
    }
    if isinstance(error,subprocess.TimeoutExpired):
        return 'command-timeout'
    if isinstance(error,subprocess.CalledProcessError):
        return 'command-failed'
    if isinstance(error,json.JSONDecodeError):
        return 'invalid-json'
    return known.get(str(error),'runtime-failure')


def main_ha(config):
    gateway,transport=config['gateway'],config['transport']
    links=wireguard_links(config)
    processes=[]
    stop=False
    stage='startup'

    def terminate(*_):
        nonlocal stop
        stop=True

    signal.signal(signal.SIGTERM,terminate)
    signal.signal(signal.SIGINT,terminate)
    try:
        Path('/run/gateway-ready').unlink(missing_ok=True)
        for link in links:
            stage='wireguard-ownership'
            for name,is_host in ((link['interface'],False),(link['staged'],False),(link['staged'],True)):
                delete_owned_link(name,link['alias'],host=is_host,strict=True)
            stage='wireguard-create'
            create_owned_link(link['staged'],link['alias'])
            stage='wireguard-move'
            move_owned_link(link['staged'],link['interface'],link['alias'])
            stage='wireguard-configure'
            run('wg','set',link['interface'],'listen-port',str(link['listenPort']),'private-key','/keys/privateKey',
                'peer',link['publicKey'],'endpoint',link['endpoint'],
                'allowed-ips',','.join([link['tunnelIP']+'/32']+link['delegatedPrefixes']),'persistent-keepalive','5')
            run('ip','address','replace',link['localTunnelIP']+'/32','dev',link['interface'])
            run('ip','link','set','dev',link['interface'],'mtu',str(transport.get('mtu',1380)),'up')
            run('ip','route','replace',link['tunnelIP']+'/32','dev',link['interface'])
        stage='local-network-configure'
        for prefix in local_prefixes(config):
            run('ip','route','replace',prefix,'via',gateway['routerIP'],'dev','eth0')
        run('ip','route','replace',gateway['bfd']['sourceIP']+'/32','via',gateway['routerIP'],'dev','eth0')
        run('ip','route','del','default',check=False)
        run('ip','route','replace','unreachable','default','metric','42760')
        settings=[('net.ipv4.ip_forward','1'),('net.ipv4.fib_multipath_hash_policy','1')]
        for interface in ['all','default','eth0']+[link['interface'] for link in links]:
            settings.extend([('net.ipv4.conf.'+interface+'.rp_filter','0'),
                             ('net.ipv4.conf.'+interface+'.send_redirects','0'),
                             ('net.ipv4.conf.'+interface+'.accept_redirects','0')])
        for name,value in settings:
            run('sysctl','-w',name+'='+value)
        stage='bfd-compatibility-configure'
        install_bfd_compat(config)
        stage='routing-files'
        Path('/run/frr').mkdir(exist_ok=True)
        shutil.chown('/run/frr',user='frr',group='frr')
        Path('/run/frr/frr.conf').write_text(render_frr(config))
        Path('/run/frr/base.conf').write_text('frr defaults traditional\nhostname global-vpc-gateway\nlog stdout informational\n')
        for daemon in ('zebra','bfdd','bgpd'):
            stage='routing-start-'+daemon
            processes.append(subprocess.Popen(['/usr/lib/frr/'+daemon,'-f','/run/frr/base.conf',
                '-i','/run/frr/'+daemon+'.pid','-z','/run/frr/zserv.api','-A','127.0.0.1']))
            time.sleep(1)
            if processes[-1].poll() is not None:
                raise RuntimeError('routing process failed')
        stage='routing-configuration'
        load_runtime_config()
        # Readiness means the local processes/configuration are installed. The
        # controller intersects this member ID with OVN BFD Up for HA status.
        # It does not imply remote routes or end-to-end reachability.
        Path('/run/gateway-ready').write_text(gateway['ownerUID']+'\n')
        print(json.dumps({'event':'local-ready','ownerUID':gateway['ownerUID'],
                          'globalVpcID':gateway['globalVpcID'],'gatewayID':gateway['gatewayID'],
                          'links':[{'id':link['id'],'interface':link['interface']} for link in links]}),flush=True)
        advertised=False
        while not stop:
            stage='routing-health'
            if any(process.poll() is not None for process in processes):
                raise RuntimeError('routing process exited')
            # Any read/JSON/write failure unwinds the whole runtime. Terminating
            # bgpd and bfdd below prevents a stale local-prefix advertisement.
            advertised=refresh_local_advertisement(config,advertised)
            time.sleep(max(0.1,min(1,gateway['bfd']['minRX']/1000)))
    except Exception as error:
        print(json.dumps({'event':'gateway-stage-error','stage':stage,'code':failure_code(error)}),file=sys.stderr,flush=True)
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
                process.kill()
                process.wait(timeout=3)
        try:
            cleanup_bfd_compat(config)
        finally:
            for link in links:
                for name,is_host in ((link['interface'],False),(link['staged'],False),(link['staged'],True)):
                    delete_owned_link(name,link['alias'],host=is_host)


def counters(config):
    """Read-only runtime evidence; never use wg dump/showconf, which reveal keys."""
    def read_json(*args):
        result=run(*args,check=False)
        return json.loads(result.stdout) if result.returncode==0 and result.stdout.strip() else None
    result={'gatewayID':config['gateway'].get('gatewayID'),'links':[],
            'routes':read_json('ip','-j','route','show','proto','bgp'),
            'bfd':read_json('vtysh','-c','show bfd peers json')}
    links=wireguard_links(config) if config['gateway']['version']=='v2' else [{'id':'legacy','interface':'wg0'}]
    for link in links:
        transfer=run('wg','show',link['interface'],'transfer',check=False)
        peers=[]
        if transfer.returncode==0:
            for line in transfer.stdout.splitlines():
                fields=line.split()
                if len(fields)==3:
                    peers.append({'rxBytes':int(fields[1]),'txBytes':int(fields[2])})
        result['links'].append({'id':link['id'],'interface':link['interface'],
                               'interfaceStats':read_json('ip','-j','-s','link','show','dev',link['interface']),
                               'wireguardTransfers':peers})
    if config['gateway']['version']=='v2':
        name,_=nft_identity(config)
        result['bfdCompatibility']=read_json('nft','-j','list','table','inet',name)
    return result


def main():
    config=json.loads(Path('/config/gateway.json').read_text())
    if config['transport'].get('profile', 'wireguard-bgp') != 'wireguard-bgp':
        if __package__:
            from . import overlay
        else:
            import overlay
        if len(sys.argv)>1 and sys.argv[1]=='--stats':
            print(json.dumps(overlay.stats(config, sys.modules[__name__]), sort_keys=True))
        else:
            overlay.main(config, sys.modules[__name__])
    elif len(sys.argv)>1 and sys.argv[1]=='--stats':
        print(json.dumps(counters(config),sort_keys=True))
    elif config['gateway']['version']=='v2':
        main_ha(config)
    else:
        main_legacy(config)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        # Never print subprocess arguments, which can include peer material.
        print(json.dumps({'event':'gateway-error','type':type(exc).__name__}), file=sys.stderr, flush=True)
        sys.exit(1)
