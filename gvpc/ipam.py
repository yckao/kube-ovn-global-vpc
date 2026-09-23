"""Versioned executable IPAM provider contract, independent of network rendering.

Providers receive one JSON request on stdin and return one JSON response on stdout.
Credential configuration belongs to the provider process, never to an allocation.
"""
import json
import ipaddress
import subprocess


class IPAMError(RuntimeError):
    pass


class ExecProvider:
    def __init__(self, command, timeout=30):
        if not command or not all(isinstance(s, str) for s in command):
            raise ValueError('Provider command must be a nonempty argument array')
        self.command, self.timeout = command, timeout

    def call(self, operation, **parameters):
        request = {'apiVersion': 'ipam.globalvpc.io/v1alpha1', 'operation': operation, **parameters}
        try:
            p = subprocess.run(self.command, input=json.dumps(request), text=True,
                               capture_output=True, timeout=self.timeout)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise IPAMError('Provider unavailable; allocation state is unknown') from error
        if p.returncode:
            # Provider stderr may contain authentication details.
            raise IPAMError('Provider failed; preserve allocations and retry discovery')
        try:
            response = json.loads(p.stdout)
        except ValueError as error:
            raise IPAMError('Invalid provider response') from error
        if response.get('apiVersion') != request['apiVersion']:
            raise IPAMError('Unsupported provider response version')
        if not response.get('ok'):
            raise IPAMError(response.get('error', 'Provider rejected request'))
        result = response['result']
        if operation in ('EnsurePrefix', 'GetPrefix'):
            for key in ('claimUID', 'vpcUID', 'scopeRef'):
                if result.get(key) != parameters.get(key):
                    raise IPAMError('Provider returned an allocation for the wrong identity')
            try:
                matches = ipaddress.ip_network(result['cidr'], strict=True) == ipaddress.ip_network(parameters['cidr'], strict=True)
            except (KeyError, ValueError) as error:
                raise IPAMError('Provider returned an invalid allocation') from error
            if not matches or not result.get('allocationID') or result.get('releasePolicy') != 'Retain':
                raise IPAMError('Provider allocation does not match the requested retained claim')
        return result
