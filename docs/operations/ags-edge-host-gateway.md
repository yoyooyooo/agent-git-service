# AGS host gateway: routing, prewarming and diagnosis

## Contract

A client keeps the primary AGS Git URL. Its per-user Git configuration can select a nearer Edge connection without changing repository remotes, system DNS, browser routing or SSH. This is connection selection, not a second write authority.

`AGS_EDGE_UNBOUND_READS=primary` enables host-wide forwarding for repositories not enrolled in snapshot acceleration. Enrolled reads use original-user authorization, an exact verified local view and use-time revalidation. Unenrolled reads and all writes retain primary behavior. A denial, corrupt cache or failed synchronization of an enrolled repository does not become a silent read from another version.

The default `reject` mode is appropriate before host-wide acceptance. Do not install a host-wide client rule when unenrolled repositories would unexpectedly stop working. Enrollment remains explicit; a global routing rule does not grant a node or user access to every repository.

## Prerequisites

The execution environment must have an authorized network path to both the primary and the Edge. Network node sharing does not automatically grant access to other nodes. The Edge must serve the same destination port selected by the canonical URL, or a front proxy must provide that port. An HTTPS entry needs a valid certificate for the canonical hostname and every supported alias; do not disable certificate verification.

User authentication stays with the existing primary credential helper. Do not copy personal tokens into Edge configuration. A short hostname and its fully qualified alias must be explicitly supported by both the credential route and the Edge's canonical-origin configuration.

The examples below use reserved documentation addresses. Replace them with operator-approved endpoints after validating connectivity; they are not a working deployment. HTTP examples assume a separately secured network. Prefer verified HTTPS where traffic is not protected independently.

## Client configuration

First inspect the proposed values without changing Git configuration or creating a state receipt:

```bash
python3 scripts/edge-client-route.py plan --name primary \
  --origin http://primary.example.test:6666 \
  --origin http://primary-alias.example.test:6666 \
  --edge-ip 192.0.2.20 --primary-ip 192.0.2.10
```

After the server and access checks pass, use the same arguments with `install`. Inspect or remove the exact named rule with:

```bash
python3 scripts/edge-client-route.py status --name primary
python3 scripts/edge-client-route.py remove --name primary
```

These are explicit operator commands, not automatic workload bootstrap. The helper owns only URL-scoped `http.<origin>/.curloptResolve` keys. Its owner-only receipt saves the previous values of those keys, not a copy of the complete Git configuration. It rejects unowned keys, malformed receipts and configuration drift before restoration. `status` and `plan` do not provision missing state directories. Always pass the existing receipt name when migrating an older installation; the generic default is `primary`.

The rule affects the current OS user's Git HTTP client. Other users, containers and machines need their own explicit setup. Submodules, redirects, proxy settings and alternate protocol URLs must be tested in the actual execution environment; do not assume they use the same path.

The first connection address is Edge, the second is primary. This permits connection-level fallback supported by the Git/libcurl build. It is **not** a health-aware load balancer or HTTP retry engine: a 403 is not bypassed, a push is not replayed after an ambiguous response, and an in-progress transfer does not restart transparently elsewhere. A blackholed address can incur connection timeout before another address is tried.

## Server setup

Register repositories and node certificates on the primary using the [enrollment workflow](ags-edge-enrollment.md). Attach its mTLS replica endpoint to the owning primary process using the [primary listener guide](ags-edge-primary-listener.md). Configure `ags-edge` independently with `.env.edge.example` and `docs/examples/edge-read.example.json`.

Keep the physical primary endpoint independent of client-side canonical routing. The Edge's own outbound requests must not inherit a rule that directs it back into itself. Canonical aliases are exact origins for one primary, not wildcard host routing or multi-primary authority selection.

Set `AGS_EDGE_UNBOUND_READS=primary` only after verifying an enrolled repository, an unenrolled private repository, an unknown repository, permission denial and write forwarding. Initial synchronization may need substantially more time than a warm read; configure bounded request, peer and synchronization timeouts for the expected repository size.

## Prewarming and capacity

An explicit `prewarm_interval`, such as the example's 30 seconds, lets the enrolled node inspect and synchronize repositories ahead of foreground requests. It uses node authority, not personal credentials. Failures back off and remain observable. This is polling; durable change subscriptions, cursors and resumable downloads are not yet implied.

Minimum free space is checked during operation as well as startup. Published snapshot count, logical byte budget, protection window and idle expiry are separate controls. Keep the protected-view window at least as long as Git negotiation may need older views. Active readers and synchronization bases are pinned; inability to reclaim safe space rejects new work rather than evicting a live view. Allocate extra margin for temporary objects and indexing because these checks are not a disk quota or reservation.

## Diagnosis

`AGS_EDGE_DIAGNOSTICS_ADDR=127.0.0.1:16667` enables a separate loopback-only operator endpoint. From the Edge host or an authorized local tunnel:

```bash
curl --noproxy '*' -fsS http://127.0.0.1:16667/status
curl --noproxy '*' -fsS http://127.0.0.1:16667/metrics
```

The public `/livez` checks process liveness. `/readyz` uses fresh primary/peer/capacity checks and known warming degradation; it does not promise that every repository is cached or every user is authorized. Stale observations cannot keep readiness green indefinitely.

| Signal | Interpretation |
| --- | --- |
| `local_snapshot` | Local-read route was selected; inspect result and synchronization stage before calling it a successful cache hit. |
| `primary_unbound` | Unenrolled request forwarded to the primary; no implicit replica grant. |
| `primary_write` | Write request forwarded; the primary Git protocol result remains authoritative. |
| `read_queue`, `synchronize` | Queue pressure, cold copy or incremental synchronization. |
| `prepare_*`, `revalidate` | Primary authority and exact-view checks; denial cannot be bypassed with cached files. |
| `local_backend` | Native Git serving the selected local view. |
| `primary_proxy` | Primary forwarding; uncertain mutation responses require exact readback before another attempt. |

Responses carry `X-AGS-Edge-Request-ID` and `X-AGS-Edge-Route`. Correlate them with the Edge's structured logs and primary request ID. Events intentionally exclude credentials, user identity, request bodies and raw URL queries.

The recent-request endpoint is a bounded in-memory window. Preserve service-manager logs with an operator-defined retention period for later dogfood analysis. A failed connection that never reached Edge leaves no Edge request event; client fallback evidence requires a separate credential-safe observation. Configure certificate renewal before expiry; expiry visibility is not automatic renewal.

## Acceptance checklist

Exercise fresh clones as well as existing working copies, both canonical names, noninteractive credentials, enrolled/unenrolled paths, push followed by immediate read, force-update during negotiation, revoked credentials against warm snapshots, missing objects, synchronization interruption, capacity failure and restart. Validate a failed first connection without disrupting the production Edge when possible.

Source tests support only the exercised paths. Keep real topology, certificates, account details, throughput measurements and deployment rollback receipts in the operator's private evidence store. Public documentation must not embed a maintainer's machine inventory.
