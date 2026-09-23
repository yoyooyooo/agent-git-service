# AGS Edge

AGS Edge is an optional, independently started Git read replica for an AGS primary. It is a product feature, not a requirement for hosting, building or contributing to this source repository. GitHub owns this fork's source workflow; an operator may independently deploy AGS and Edge for other repositories.

## Authority and routing

The primary owns users, permissions, repository lifecycle, Git writes, Issues, pull requests and provider effects. Edge owns only verified Git snapshots, synchronization state and bounded diagnostic data. It does not bootstrap the primary Service, connect to its business database, or execute provider workflows a second time.

```text
Git client, unchanged repository URL
              |
       connection selection
              |
          AGS Edge
        /          \
verified local     primary forwarding
Git upload-pack    receive-pack / unbound / other
        |                  |
 snapshot replication and original-user authorization
                           |
                       AGS primary
```

Restricted mode rejects unbound Git reads. Explicit host-gateway mode (`AGS_EDGE_UNBOUND_READS=primary`) chooses forwarding for unenrolled repositories at ingress, before protocol negotiation. It never converts a permission denial, corrupt snapshot or failed synchronization into a successful fallback. An unenrolled repository is not auto-created, copied or granted to a peer.

All Git writes remain primary-owned, including receive-pack discovery. HTTP success is not itself proof that a Git ref update succeeded. Forwarding preserves the original credential and protocol response; uncertain writes are not automatically replayed. LFS, archives and other APIs retain primary behavior and are not counted as snapshot acceleration.

## Two independent identities

Peer mTLS identifies an enrolled Edge and the exact repository storage identities it may copy. It is not the user's authorization. Each foreground read supplies the original user credential to the primary's Git authorization path, including Access Grant transport restrictions and revocation. No user credential is persisted in a snapshot, synchronization task or diagnostic event.

A read plan binds an authority, immutable storage identity, business repository ID, repository kind, permitted refs/HEAD, object format, export policy and lifetime. Repository rename preserves storage identity. Deletion and same-name recreation must not reuse it or inherit an old peer grant.

The primary revalidates after synchronization. Long synchronization cannot extend an expired plan locally: the original credential must acquire a fresh plan for the same retained view and pass use-time validation again. A missing user, revoked parent Grant, disabled repository or unavailable authority fails closed.

## Snapshot lifecycle

`EnsureSnapshot` returns only a complete verified view. Missing targets trigger coalesced synchronization; simultaneous readers can share one transfer. A disconnected reader does not cancel the operation required by other waiters. Node concurrency, pending work, pack size and synchronization time are bounded.

The primary prepares private staging before taking its mutation barrier. Under the barrier it revalidates access, observes refs/HEAD and hardlinks immutable objects. Packing, object verification, publication and transfer happen after the barrier is released. Source Git and snapshot staging therefore require a hardlink-capable shared filesystem. Enumeration and linking still consume a finite write pause; this is not zero-pause capture.

Unpublished staging is never served. Exact-root validation rejects incomplete or additional objects outside the export view. Published snapshots own their file entries and do not retain an alternates-path dependency on the primary or an older view. Compatible immutable pack files may share inodes; a shrinking object set is rebuilt exactly rather than retaining deleted-branch-only objects.

Cross-version transfer is bound to an exact compatible base and target. The primary sends missing objects; Edge reconstructs and verifies before publication. If the primary has legitimately evicted that base, it may send the same target in full. Authentication failure or corrupted transfer does not trigger a weaker retry path.

## Git protocol continuity

Git Smart HTTP is multi-request and stateless. Edge recognizes supported v0/v1/v2 discovery and object-fetch phases without requiring a cookie or mapping clients by IP. New discovery obtains a fresh primary view. A subsequent `want` may still refer to a recently advertised retained view after another client discovers a newer head.

A bounded recent-view registry selects a view satisfying the requested objects and current authorization. It must not silently replace an earlier requested commit with a newer one. Expired views, invalid requests and missing objects fail explicitly. Native Git continues to own object negotiation and pack generation; unsupported protocol capabilities are not advertised.

## Resource and failure boundaries

Published view count, logical byte budget, recent-view protection and idle expiry are independently bounded. Active reader/export/base leases prevent unsafe eviction. Capacity failure rejects new publication rather than deleting a live view. Hardlinked files may be conservatively counted more than once; logical retention bytes are not an exact measure of reclaimed physical blocks.

Minimum filesystem headroom is checked at startup, during import and before publication. It is not a reservation or operating-system quota. Temporary pack/index space and concurrent work need additional margin. Request-body spooling observes cancellation and releases private temporary files.

Foreground correctness does not depend on prewarming. Node-authorized periodic warming reduces cold waits and uses no personal token. The current implementation is polling, not a durable change-event subscription with a recovery cursor. Interrupted transfer resume, automatic certificate rotation and pack compaction remain separate work.

## Operating surface

The primary's optional mTLS endpoint is attached to the original owning process through `AGS_REPLICATION_CONFIG_FILE`. A second process reading the same Git directory does not share its in-process write barrier and is not an equivalent deployment. Registration uses the administrator interface and explicit stable repository identity; a generated peer plan is unapplied until the operator installs it.

`ags-edge` reads its own explicit configuration, never the primary runtime environment. It validates the canonical origin and any exact aliases; the physical primary endpoint must not resolve back into the Edge. Network topology, peer certificates and user credentials belong to operator configuration, not compiled defaults.

`/livez` is process liveness. `/readyz` reports current configured primary/peer/capacity observations and known prewarm degradation; stale health observations expire. Without full read/operational assembly it remains unavailable. Readiness is not a claim that all repositories are enrolled or every user may read them.

A separate loopback-only diagnostic listener exposes `/status` and `/metrics`. Structured events report request IDs, route, stage, duration and transfer/capacity outcomes without credentials or request bodies. The bounded recent-request list is not durable audit retention. A client's connection failure before reaching Edge cannot appear in Edge's request log.

## Source and verification map

| Surface | Owner |
| --- | --- |
| Native CGI, credential stripping and bounded spooling | `internal/gitbackend` |
| Read plans, manifests and transfer contracts | `internal/edgeprotocol` |
| Capture, exact objects, leases, publication and retention | `internal/snapshotstore` |
| Peer authorization and primary control/export transport | `internal/replication` |
| Original-user authorization and repository identity | `internal/service/replication_*` and `git_transport_access.go` |
| Mirror, read selection, gateway, warming and observation | `internal/edge` |
| Primary endpoint process lifecycle | `server/replication_*` |
| Operator registration / unapplied peer plans | `cmd/ags-replication`, `internal/replicationadmin` |
| Scoped client routing | `scripts/edge-client-route.py` |

Use real Git, real isolated databases, mTLS and process-lifecycle tests for these boundaries. Include force-update between discovery and fetch, user/node revocation, same-name recreation, corrupt packs, full disks, cancellation, restart and unknown push responses. A local test does not attest an operator's private write topology or cross-region throughput.

Operational entries: [host gateway](../operations/ags-edge-host-gateway.md), [primary listener](../operations/ags-edge-primary-listener.md), [enrollment](../operations/ags-edge-enrollment.md). Private deployment transcripts are not the public implementation contract. Fork source acceptance and deployed-runtime acceptance remain separate; see [fork governance](../../fork/README.md).
