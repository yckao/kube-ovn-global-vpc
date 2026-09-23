# Security policy

## Supported scope

Version 0.1.0 is experimental. Security fixes target the latest `main`; there is
no long-term-support branch, security SLA or production certification.

## Report privately

Use [GitHub private vulnerability reporting](https://github.com/yckao/kube-ovn-global-vpc/security/advisories/new)
when available. Do not put vulnerabilities, credentials, exploit payloads or
private infrastructure details in public issues. If private reporting is
unavailable, open a minimal issue requesting a private contact without disclosing
the issue or affected deployment details.

Include affected version, impact, prerequisites, a sanitized minimal reproducer
and any mitigation. Maintainers coordinate disclosure on a best-effort basis;
no response deadline is promised.

## Deployment trust boundaries

- Tenants should edit only public VPC/Subnet resources. NetworkBindings, native
  network resources, status and operator namespaces are platform-controlled.
- Gateways require privileged host access. Use appropriately isolated,
  administrator-controlled nodes. Gateway Pods do not mount an API token by default.
- Use immutable image digests and source-matched native extension binaries.
  Applying a CRD schema alone does not establish compatibility.
- WireGuard encrypts inter-gateway transport. Geneve and VXLAN/EVPN require a
  trusted underlay and do not provide transport encryption.
- Reserve disjoint infrastructure pools; verify underlay and MTU. Manage location
  credential renewal in the platform. The development helper is not an identity service.
- Preserve ownership/UID fences, allocation receipts and finalizers. Bypassing
  them can reassign identity or withdraw resources incorrectly.

Read [validation and maturity](docs/managed-validation.md) before deployment.
