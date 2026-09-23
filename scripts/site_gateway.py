#!/usr/bin/env python3
"""Exec-contract adapter for local Kubernetes WireGuard/FRR gateways.

Administrator configuration contains one local kubeconfig and static transport
grants. It contains no remote Kubernetes/controller/API endpoint. Private keys
are referenced by an existing local Secret, never passed through this protocol.
"""
import argparse
import base64
import copy
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import subprocess
import sys

try:
    from .transport_profiles import validate_overlay, validate_socket_allocations
except ImportError:
    from transport_profiles import validate_overlay, validate_socket_allocations

OWNER = 'networking.globalvpc.io/owner-uid'
ROLE = 'networking.globalvpc.io/gateway'
HASH = 'networking.globalvpc.io/gateway-config-hash'
MEMBER = 'networking.globalvpc.io/gateway-id'


def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON key')
        result[key] = value
    return result


def decode(text):
    return json.loads(text, object_pairs_hook=strict_object)


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def dns(value):
    return isinstance(value, str) and len(value) <= 253 and all(
        len(label)<=63 and re.fullmatch(r'[a-z0-9](?:[-a-z0-9]*[a-z0-9])?', label) for label in value.split('.'))


def address(value):
    parsed=ipaddress.IPv4Address(value)
    if type(value) is not str or str(parsed) != value:
        raise ValueError('address must be a canonical IPv4 string')
    return parsed


def prefix(value):
    parsed=ipaddress.IPv4Network(value,strict=True)
    if type(value) is not str or str(parsed) != value:
        raise ValueError('prefix must be a canonical IPv4 string')
    return parsed


def integer(value, minimum, maximum):
    return type(value) is int and minimum <= value <= maximum


def managed_equal(actual, desired):
    """Allow API defaulted map fields, but never extra/reordered managed lists."""
    if isinstance(desired, dict):
        return isinstance(actual, dict) and all(
            (key in actual or value is False) and managed_equal(actual.get(key,False),value)
            for key,value in desired.items())
    if isinstance(desired, list):
        return isinstance(actual, list) and len(actual)==len(desired) and all(managed_equal(a,b) for a,b in zip(actual,desired))
    return type(actual) is type(desired) and actual == desired


def validate(config, request):
    if set(request) - {'version', 'operation', 'gateway', 'expectedResources'}:
        raise ValueError('unknown request field')
    if request.get('version') != 'v1' or request.get('operation') not in ('Ensure','Get','Delete','EndpointsEmpty'):
        raise ValueError('unsupported protocol operation')
    gateway = request['gateway']
    if gateway['version'] not in ('v1', 'v2') or gateway['gatewayRevision'] != config['revision']:
        raise ValueError('gateway registration revision changed')
    for key in ('ownerUID','globalVpcID','siteID','attachmentID','vpcName','subnetName','transitName'):
        if not dns(gateway[key]):
            raise ValueError('invalid gateway identity')
    if gateway['siteID'] != config['siteID'] or gateway['attachmentID'] != config['attachmentID']:
        raise ValueError('foreign local attachment')
    transport = next((item for item in config['grants'] if item['globalVpcID'] == gateway['globalVpcID']), None)
    if transport is None:
        raise ValueError('no transport grant')
    profile = transport.get('profile', 'wireguard-bgp')
    if profile not in ('wireguard-bgp', 'geneve-bgp', 'vxlan-evpn'):
        raise ValueError('unsupported transport profile')
    if gateway['version'] == 'v1' and profile != 'wireguard-bgp':
        raise ValueError('overlay profiles require HA gateway registration')
    local = prefix(gateway['cidr'])
    transit = prefix(gateway['transitCIDR'])
    delegated = [prefix(p) for p in gateway['delegatedPrefixes']]
    if not any(local.subnet_of(parent) for parent in delegated):
        raise ValueError('local CIDR outside delegation')
    if local.overlaps(transit):
        raise ValueError('local and transit CIDRs overlap')
    for key in ('routerIP','gatewayIP') if gateway['version']=='v1' else ('routerIP',):
        ip = address(gateway[key])
        if ip not in transit or ip in (transit.network_address, transit.broadcast_address):
            raise ValueError('invalid local transit address')
    if gateway['version']=='v1' and gateway['routerIP'] == gateway['gatewayIP']:
        raise ValueError('transit addresses collide')
    for key in ('namespace',):
        if not dns(config[key]):
            raise ValueError('invalid namespace')
    if gateway['version']=='v2':
        validate_ha(gateway, transport, transit, delegated)
        return gateway, transport
    if not dns(transport['nodeName']) or not dns(transport['privateKeySecretName']):
        raise ValueError('invalid transport resource')
    if not integer(transport['listenPort'],1024,65535) or not integer(transport['localASN'],1,4294967294):
        raise ValueError('invalid transport numeric value')
    if not integer(transport.get('mtu',1380),1280,1400):
        raise ValueError('unsupported gateway MTU')
    tunnel = address(transport['localTunnelIP'])
    seen = [transit] + delegated
    peers = set()
    keys = set()
    if not transport['peers']:
        raise ValueError('at least one routing peer grant required')
    for peer in transport['peers']:
        remote = address(peer['tunnelIP'])
        if remote == tunnel or str(remote) in peers:
            raise ValueError('duplicate routing peer')
        peers.add(str(remote))
        host, port = peer['endpoint'].rsplit(':', 1)
        address(host)
        if not port.isascii() or not port.isdigit() or str(int(port)) != port or not 1024 <= int(port) <= 65535 or not integer(peer['asn'],1,4294967294) or peer['asn'] == transport['localASN']:
            raise ValueError('invalid peer transport')
        decoded_key=base64.b64decode(peer['publicKey'], validate=True)
        if len(decoded_key) != 32 or base64.b64encode(decoded_key).decode() != peer['publicKey'] or peer['publicKey'] in keys:
            raise ValueError('invalid peer key')
        keys.add(peer['publicKey'])
        if not peer['delegatedPrefixes']:
            raise ValueError('remote delegated prefixes required')
        for raw in peer['delegatedPrefixes']:
            pool = prefix(raw)
            if any(pool.overlaps(previous) for previous in seen):
                raise ValueError('overlapping prefix ownership within one tenant')
            seen.append(pool)
    for ip in [tunnel] + [address(p['tunnelIP']) for p in transport['peers']]:
        if any(ip in pool for pool in seen):
            raise ValueError('routing control address overlaps tenant pool')
    return gateway, transport


def validate_ha(gateway, transport, transit, delegated):
    """Independent links may share a pool only for the same delegated owner."""
    if transport.get('profile', 'wireguard-bgp') != 'wireguard-bgp':
        validate_overlay(gateway, transport, transit, delegated)
        return
    if set(transport) - {'globalVpcID', 'profile', 'gateways'}:
        raise ValueError('unsupported WireGuard grant fields')
    members = gateway.get('gateways')
    configured = transport.get('gateways')
    if not isinstance(members, list) or not 2 <= len(members) <= 8 or not isinstance(configured, list):
        raise ValueError('HA requires two to eight registered gateways')
    ids, ips = set(), {gateway['routerIP']}
    for member in members:
        if set(member) != {'id', 'ip'} or not dns(member['id']) or len(member['id'])>63 or '.' in member['id'] or member['id'] in ids:
            raise ValueError('gateway member identities must be unique')
        ip = address(member['ip'])
        if ip not in transit or ip in (transit.network_address,transit.broadcast_address) or member['ip'] in ips:
            raise ValueError('gateway member address must be a unique transit host')
        ids.add(member['id'])
        ips.add(member['ip'])
    if len(configured) != len(members) or {m['id'] for m in configured} != ids:
        raise ValueError('transport members do not match the local gateway grant')
    bfd = gateway.get('bfd', {})
    if not isinstance(bfd, dict) or set(bfd)-{'sourceIP','minRX','minTX','multiplier','nodeSelector'}:
        raise ValueError('invalid local BFD configuration')
    source = address(bfd['sourceIP'])
    if bfd['sourceIP'] in ips or not integer(bfd['minRX'],100,60000) or not integer(bfd['minTX'],100,60000) or not integer(bfd['multiplier'],2,255):
        raise ValueError('invalid BFD source or timers')
    if 'nodeSelector' in bfd and not isinstance(bfd['nodeSelector'], dict):
        raise ValueError('invalid BFD node selector')
    # All gateways are independently hosted; multiple Pods on one node do not
    # constitute host-failure redundancy. Each endpoint has its own UDP socket.
    nodes, secrets, router_ids, member_ids, ports, endpoints, controls, link_ids = set(),set(),set(),set(),set(),set(),set(),set()
    local_asns = set()
    ownership = {}
    member_claims = []
    for member in configured:
        if set(member) - {'id', 'nodeName', 'privateKeySecretName', 'localASN', 'routerID', 'mtu', 'links'}:
            raise ValueError('unsupported WireGuard member fields')
        if 'siblings' in member:
            raise ValueError('siblings must be derived from the local gateway grant')
        if member['id'] in member_ids or not dns(member['nodeName']) or member['nodeName'] in nodes or not dns(member['privateKeySecretName']) or member['privateKeySecretName'] in secrets:
            raise ValueError('HA gateway identities, nodes and Secret references must be valid and distinct')
        member_ids.add(member['id'])
        nodes.add(member['nodeName'])
        secrets.add(member['privateKeySecretName'])
        router_id = address(member['routerID'])
        if str(router_id) in router_ids or not integer(member['localASN'],1,4294967294) or not integer(member.get('mtu',1380),1280,1400):
            raise ValueError('invalid HA router ID, ASN or MTU')
        router_ids.add(str(router_id))
        local_asns.add(member['localASN'])
        if not isinstance(member.get('links'),list) or not member['links']:
            raise ValueError('each gateway requires routing links')
        claims = {}
        for link in member['links']:
            if set(link) != {'id', 'listenPort', 'localTunnelIP', 'tunnelIP', 'endpoint', 'publicKey',
                             'asn', 'remoteSiteID', 'remoteAttachmentID', 'delegatedPrefixes'}:
                raise ValueError('unsupported WireGuard link fields')
            if not dns(link['id']) or len(link['id'])>63 or '.' in link['id'] or link['id'] in link_ids or not dns(link['remoteSiteID']) or not dns(link['remoteAttachmentID']):
                raise ValueError('invalid or duplicate routing link identity')
            link_ids.add(link['id'])
            if not integer(link['listenPort'],1024,65535) or (member['nodeName'],link['listenPort']) in ports:
                raise ValueError('duplicate or invalid gateway listen port')
            ports.add((member['nodeName'],link['listenPort']))
            host, port = link['endpoint'].rsplit(':',1)
            address(host)
            if not port.isascii() or not port.isdigit() or str(int(port))!=port or not 1024<=int(port)<=65535 or link['endpoint'] in endpoints:
                raise ValueError('invalid or duplicate remote endpoint')
            endpoints.add(link['endpoint'])
            if not integer(link['asn'],1,4294967294) or link['asn']==member['localASN']:
                raise ValueError('remote links require a distinct eBGP ASN')
            public = base64.b64decode(link['publicKey'],validate=True)
            if len(public)!=32 or base64.b64encode(public).decode()!=link['publicKey']:
                raise ValueError('invalid link public key')
            for field in ('localTunnelIP','tunnelIP'):
                ip = address(link[field])
                if str(ip) in controls or str(ip) in ips or ip==source:
                    raise ValueError('each point-to-point link requires unique control addresses')
                controls.add(str(ip))
            if not isinstance(link['delegatedPrefixes'],list) or not link['delegatedPrefixes']:
                raise ValueError('remote delegated prefixes required')
            pools = tuple(sorted(str(prefix(p)) for p in link['delegatedPrefixes']))
            if len(set(pools))!=len(pools):
                raise ValueError('duplicate remote delegated prefix')
            owner = (link['remoteSiteID'],link['remoteAttachmentID'])
            if owner==(gateway['siteID'],gateway['attachmentID']):
                raise ValueError('remote link cannot impersonate the local attachment')
            claim = (link['asn'],pools)
            if owner in ownership and ownership[owner]!=claim:
                raise ValueError('links disagree on remote delegation or ASN ownership')
            ownership[owner] = claim
            claims[owner] = claim
        member_claims.append(claims)
    if len(local_asns)!=1:
        raise ValueError('sibling gateways must share one local ASN')
    if any(claims != member_claims[0] for claims in member_claims):
        raise ValueError('sibling gateways must cover the same remote delegated owners')
    seen = [transit]
    for pool in delegated + [prefix(p) for _,pools in ownership.values() for p in pools]:
        if any(pool.overlaps(previous) for previous in seen):
            raise ValueError('overlapping prefix ownership within one tenant')
        seen.append(pool)
    for ip in [source]+[address(p) for p in controls]:
        if any(ip in pool for pool in seen):
            raise ValueError('routing control address overlaps tenant or transit pool')


class Backend:
    def __init__(self, config, request):
        self.config, self.request = config, request
        self.gateway, self.transport = validate(config, request)
        validate_socket_allocations(config)
        self.namespace = config['namespace']
        self.owner = self.gateway['ownerUID']
        self.name = 'sg-' + digest({'owner':self.owner})[:16]
        self.members = []
        if self.gateway['version']=='v1':
            self.members.append({'name':self.name,'gateway':self.gateway,'transport':self.transport})
        else:
            for member in sorted(self.transport['gateways'],key=lambda m:m['id']):
                desired = next(g for g in self.gateway['gateways'] if g['id']==member['id'])
                local_gateway = copy.deepcopy(self.gateway)
                local_gateway.update(gatewayID=member['id'],gatewayIP=desired['ip'])
                local_transport = copy.deepcopy(member)
                if self.transport.get('profile', 'wireguard-bgp') != 'wireguard-bgp':
                    local_transport.update(profile=self.transport['profile'], trustedUnderlay=self.transport['trustedUnderlay'])
                local_transport['siblings'] = sorted([copy.deepcopy(g) for g in self.gateway['gateways'] if g['id']!=member['id']],key=lambda g:g['id'])
                self.members.append({'name':'sg-'+digest({'owner':self.owner,'member':member['id']})[:16],
                                     'gateway':local_gateway,'transport':local_transport})
        names={m['name'] for m in self.members}
        for record in request.get('expectedResources',[]):
            if set(record)!={'apiVersion','kind','namespace','name','uid'} or record['apiVersion']!='v1' or record['kind'] not in ('Pod','ConfigMap') or record['namespace']!=self.namespace or record['name'] not in names or not record['uid']:
                raise ValueError('gateway ledger contains an unexpected resource')
        self.expected = {(r['kind'],r['namespace'],r['name']):r['uid'] for r in request.get('expectedResources',[])}
        if len(self.expected) != len(request.get('expectedResources',[])):
            raise ValueError('duplicate resource ledger entry')

    def kube(self, *args, data=None):
        command = ['kubectl','--kubeconfig',self.config['kubeconfig'],'--request-timeout=15s',*args]
        output = subprocess.run(command, input=None if data is None else json.dumps(data), text=True,
                                capture_output=True, timeout=25)
        if output.returncode:
            raise RuntimeError('local Kubernetes operation failed')
        return output.stdout

    def get(self, kind, name, namespace=None):
        args = ['get',kind,name,'--ignore-not-found','-o','json']
        if namespace:
            args += ['-n',namespace]
        text = self.kube(*args)
        return decode(text) if text.strip() else None

    def guard(self, obj):
        if obj['metadata'].get('labels',{}).get(OWNER) != self.owner or obj['metadata'].get('labels',{}).get(ROLE) != 'true':
            raise ValueError('foreign gateway resource')
        key = (obj['kind'],obj['metadata'].get('namespace',''),obj['metadata']['name'])
        if key in self.expected and self.expected[key] != obj['metadata']['uid']:
            raise ValueError('gateway resource UID changed')

    def manifests(self):
        runtime = Path(self.config.get('runtimePath',str(Path(__file__).resolve().parents[1]/'gateway/gateway.py'))).read_text()
        return [manifest for member in self.members for manifest in self.member_manifests(member,runtime)]

    def member_manifests(self, member, runtime):
        name, gateway, transport = member['name'],member['gateway'],member['transport']
        content = {'gateway':gateway,'transport':transport}
        identity = {'config':content,'runtime':runtime,'image':self.config['image']}
        modules = {}
        profile = transport.get('profile', 'wireguard-bgp')
        if profile != 'wireguard-bgp':
            directory = Path(self.config.get('runtimePath', str(Path(__file__).resolve().parents[1]/'gateway/gateway.py'))).parent
            for module in ('overlay.py', 'evpn.py') if profile == 'vxlan-evpn' else ('overlay.py',):
                modules[module] = (directory/module).read_text()
            identity['modules'] = modules
        config_hash = digest(identity)
        metadata = {'name':name,'namespace':self.namespace,
                    'labels':{OWNER:self.owner,ROLE:'true'},'annotations':{HASH:config_hash}}
        if gateway['version']=='v2':
            metadata['labels'][MEMBER]=gateway['gatewayID']
        cm = {'apiVersion':'v1','kind':'ConfigMap','metadata':metadata,
              'data':{'gateway.json':json.dumps(content,sort_keys=True),'gateway.py':runtime,**modules}}
        pod_meta = json.loads(json.dumps(metadata))
        pod_meta['annotations'].update({'ovn.kubernetes.io/logical_switch':gateway['transitName'],
                                       'ovn.kubernetes.io/ip_address':gateway['gatewayIP'],
                                       'ovn.kubernetes.io/port_security':'false'})
        pod = {'apiVersion':'v1','kind':'Pod','metadata':pod_meta,'spec':{
            'nodeName':transport['nodeName'],'hostPID':True,'hostNetwork':False,'automountServiceAccountToken':False,
            'terminationGracePeriodSeconds':10,'restartPolicy':'Always',
            'containers':[{'name':'gateway','image':self.config['image'],'imagePullPolicy':'IfNotPresent',
                'command':['python3','/app/gateway.py'],'securityContext':{'privileged':True},
                'resources':{'requests':{'cpu':'25m','memory':'64Mi'},'limits':{'memory':'192Mi'}},
                'readinessProbe':{'exec':{'command':['test','-f','/run/gateway-ready']},'periodSeconds':2},
                'volumeMounts':[{'name':'config','mountPath':'/config','readOnly':True},
                    {'name':'config','mountPath':'/app','readOnly':True}]}],
            'volumes':[{'name':'config','configMap':{'name':name}}]}}
        if profile == 'wireguard-bgp':
            pod['spec']['containers'][0]['volumeMounts'].append({'name':'key','mountPath':'/keys','readOnly':True})
            pod['spec']['volumes'].append({'name':'key','secret':{'secretName':transport['privateKeySecretName'],'defaultMode':256}})
        return [cm,pod]

    def inventory(self):
        result = []
        for member in self.members:
            for kind in ('ConfigMap','Pod'):
                obj = self.get(kind,member['name'],self.namespace)
                if obj:
                    self.guard(obj)
                    result.append(obj)
        return result

    def endpoints_empty(self, inventory):
        gateway_pods = {o['metadata']['name']:o for o in inventory if o['kind']=='Pod'}
        gateway_uids = {o['metadata']['uid'] for o in gateway_pods.values()}
        gateway_ips = {m['name']:m['gateway']['gatewayIP'] for m in self.members}
        names = {self.gateway['subnetName'],self.gateway['transitName']}
        pods = decode(self.kube('get','pods','-A','-o','json'))['items']
        for pod in pods:
            annotations = pod['metadata'].get('annotations',{})
            attached = any(k.endswith('/logical_switch') and value in names for k,value in annotations.items())
            if not attached:
                continue
            if pod['metadata']['uid'] in gateway_uids:
                continue
            return False
        ips = decode(self.kube('get','ips.kubeovn.io','-o','json'))['items']
        for record in ips:
            spec = record['spec']
            attached = spec.get('subnet') in names or any(s in names for s in spec.get('attachSubnets',[]))
            if not attached:
                continue
            name=spec.get('podName')
            if name in gateway_pods and record['metadata']['name'] == name+'.'+self.namespace and spec.get('namespace') == self.namespace and spec.get('subnet') == self.gateway['transitName'] and spec.get('v4IpAddress') == gateway_ips[name] and not spec.get('attachSubnets'):
                continue
            return False
        return True

    def run(self):
        if self.get('namespace','kube-system')['metadata']['uid'] != self.config['clusterUID']:
            raise ValueError('local cluster identity changed')
        namespace = self.get('namespace',self.namespace)
        if not namespace or namespace['metadata']['uid'] != self.config['namespaceUID']:
            raise ValueError('gateway namespace identity changed')
        operation = self.request['operation']
        inventory = self.inventory()
        desired = self.manifests()
        if operation == 'Ensure':
            # Preflight every member before any mutation. An ownership or drift
            # conflict on a second member cannot partially rewrite the first.
            for manifest in desired:
                current = self.find(inventory,manifest)
                if current:
                    if current['metadata'].get('annotations',{}).get(HASH) != manifest['metadata']['annotations'][HASH]:
                        raise ValueError('accepted gateway configuration changed')
                    if manifest['kind']=='Pod' and (not managed_equal(current['spec'],manifest['spec']) or
                            not managed_equal(current['metadata'].get('annotations',{}),manifest['metadata']['annotations'])):
                        raise ValueError('owned gateway Pod managed specification drifted')
                    continue
                if (manifest['kind'],self.namespace,manifest['metadata']['name']) in self.expected:
                    raise ValueError('recorded gateway resource missing; explicit lifecycle recovery required')
            for manifest in desired:
                current = self.find(inventory,manifest)
                if current is None:
                    self.kube('create','-f','-',data=manifest)
                elif manifest['kind']=='ConfigMap' and current.get('data') != manifest['data']:
                    manifest['metadata']['resourceVersion'] = current['metadata']['resourceVersion']
                    self.kube('replace','-f','-',data=manifest)
            inventory = self.inventory()
        empty = self.endpoints_empty(inventory) if operation in ('EndpointsEmpty','Delete') else False
        if operation == 'Delete':
            if not empty:
                raise ValueError('local tenant endpoints still exist')
            # UID preconditions prevent deletion of same-name replacements.
            for obj in sorted(inventory,key=lambda o:o['kind']!='Pod'):
                self.kube('delete','--raw','/api/v1/namespaces/'+self.namespace+'/'+('pods' if obj['kind']=='Pod' else 'configmaps')+'/'+obj['metadata']['name'],
                          '-f','-',data={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':obj['metadata']['uid'],
                                     'resourceVersion':obj['metadata']['resourceVersion']}})
            inventory = self.inventory()
        ready_ids=[]
        ready_count=0
        for member in self.members:
            pair=[m for m in desired if m['metadata']['name']==member['name']]
            if all(self.configured(self.find(inventory,m),m) for m in pair):
                ready_count+=1
                if self.gateway['version']=='v2':
                    ready_ids.append(member['gateway']['gatewayID'])
        response={'version':'v1','ownerUID':self.owner,'globalVpcID':self.gateway['globalVpcID'],
                'gatewayRevision':self.gateway['gatewayRevision'],'ready':ready_count>0,'absent':not inventory,
                'endpointsEmpty':empty,'resources':[{'apiVersion':o['apiVersion'],'kind':o['kind'],
                 'namespace':self.namespace,'name':o['metadata']['name'],'uid':o['metadata']['uid']} for o in inventory]}
        if self.gateway['version']=='v2':
            response.update(readyGateways=ready_count,desiredGateways=len(self.members),readyGatewayIDs=sorted(ready_ids))
        return response

    @staticmethod
    def find(inventory, manifest):
        return next((o for o in inventory if o['kind']==manifest['kind'] and o['metadata']['name']==manifest['metadata']['name']),None)

    @staticmethod
    def configured(current, desired):
        if not current or current['metadata'].get('deletionTimestamp') or current['metadata'].get('annotations',{}).get(HASH)!=desired['metadata']['annotations'][HASH]:
            return False
        if desired['kind']=='ConfigMap':
            return current.get('data')==desired['data']
        return (managed_equal(current['spec'],desired['spec']) and
                managed_equal(current['metadata'].get('annotations',{}),desired['metadata']['annotations']) and
                any(c['type']=='Ready' and c['status']=='True' for c in current.get('status',{}).get('conditions',[])))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config',required=True)
    args = parser.parse_args()
    request_text = sys.stdin.read(1024*1024+1)
    if len(request_text)>1024*1024:
        raise ValueError('request too large')
    response = Backend(decode(Path(args.config).read_text()),decode(request_text)).run()
    print(json.dumps(response,separators=(',',':')))


if __name__=='__main__':
    try:
        main()
    except Exception as error:
        print('Local gateway operation failed: '+type(error).__name__,file=sys.stderr)
        sys.exit(1)
