# Client-side Edge route ownership

Use [the host-gateway guide](ags-edge-host-gateway.md) for server/network prerequisites and example commands. This document defines the narrower local configuration contract of `scripts/edge-client-route.py`; it is not a machine-specific deployment receipt.

## Scope

The helper changes only explicit URL-scoped Git `curloptResolve` keys in the current user's global configuration. It does not rewrite repository remotes, install credentials, edit system DNS or configure other users and containers. The operator supplies canonical origins and distinct Edge/primary connection addresses.

Use an explicit `--name` for an installed route. The generic default is `primary`; it does not discover or rename older route receipts. Hostnames and aliases must already be supported by the primary credential route and Edge ingress policy.

## Read-only operations

`plan` validates the proposed origins/addresses and returns `applied=false`. It does not create a receipt, a state directory or Git configuration. `status` validates an existing receipt and compares the exact owned values; a missing receipt fails without provisioning state.

The tool refuses workload credentials and external Git configuration overrides. It must run as the intended ordinary OS user, not inside an implicit task-scoped credential environment.

## Install and rollback

Installation saves an owner-only durable intent containing the exact desired values and their previous values before applying changes. A repeat is a no-op only when both the receipt and current values match. An interrupted operation or later manual drift is an explicit reconciliation condition, not permission to overwrite newer changes.

Before readback or restoration, receipt validation checks its closed schema, phase, matching key sets and exact URL-scoped key ownership. A malformed receipt cannot introduce `user.name`, credential helpers or unrelated configuration keys. Rollback restores the recorded values of the owned keys without removing remotes or credentials.

This protocol is not a general-purpose concurrent Git configuration transaction. An operator should serialize configuration changes and inspect interrupted receipts; simultaneous independent processes editing the same keys remain a review area. A private receipt is not a credential and must never be treated as arbitrary configuration-write authority.

## Regression and diagnosis

```bash
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_edge_client_route.py
```

Tests use disposable home directories, synthetic origins and reserved documentation addresses. They exercise idempotence, exact prior-value restoration, unrelated-key preservation, manual drift, invalid credential-bearing origins, workload rejection, read-only planning and malformed receipts.

A configured route does not prove network reachability, repository enrollment or user permission. A failed initial connection may try the primary address, but this helper does not replay HTTP denials, uncertain writes or partial transfers. Correlate actual request IDs and route headers with Edge diagnostics; operations that never reached Edge need separate client-side evidence.
