# AGS Edge primary listener: opt-in and acceptance

## Implemented boundary

`AGS_REPLICATION_CONFIG_FILE` is now consumed by the normal `gh-server` startup
and by `server.New(config.Config{ReplicationConfigFile: ...})`. It attaches a
separate TLS 1.3 mutual-authentication listener to the **same owning primary**
Service/GitStore. It does not start a second AGS process, change `BASE_URL`, add
peer routes to the public API, or redirect any client.

Without this variable/config field, the primary listener set is unchanged.
The Edge's existing `AGS_EDGE_READ_CONFIG_FILE` remains a separate configuration.
Readiness stays operationally gated: this change is not approval for team cutover.

## Required operator inputs

Use [primary-replication.example.json](../examples/primary-replication.example.json)
as a shape example, not runnable credentials. Replace its sample authority,
repository IDs, store incarnations, public-key fingerprint, certificates and paths.

- Existing repositories must be explicitly provisioned by a native repo-admin.
  Use `ags-replication status/register` against the owning primary's
  opt-in registration API; see [enrollment](ags-edge-enrollment.md). It does not
  open a second DB writer, auto-enroll replacements or grant peers access.
- Peer grants bind the exact authority/store/repository/kind and certificate SPKI
  SHA-256. A common name, hostname or repository name is not a node grant.
- The same trust bundle verifies client certificates and the declared server
  certificate chain/name. Supply a dedicated peer trust domain, a server-auth
  certificate and client-auth node certificates. Private keys/config are regular
  owner-only files, with no symlink substitution. Relative paths resolve from
  the configuration file, not the current working directory.
- Bind an explicit loopback/private/shared-address IP and port; wildcard and
  public-address listener configurations are rejected. Network access policy
  remains deployment-owned. Loopback in the example is intentionally not a
  remotely accessible production configuration.
- `managed_writes_only: true` records an operator topology acknowledgement, not
  an automated proof. Audit receive-pack, service writes, provider callbacks,
  maintenance/GC and any other process with access to the primary Git directory.
  External filesystem edits or a second writer bypass the in-process barrier.

## Startup checks and failure semantics

File structure, duplicate/unknown fields, listener shape, certificate/key pairing,
server name, chain validity and certificate lifetime are checked before ordinary
DB bootstrap. The full owning runtime then validates each registered repository
against the persisted incarnation and checks actual storage paths.

For the configured Git root and every registered source repository, preflight
verifies that source and snapshot roots do not overlap, creates/removes private
empty probe files to test actual hardlinks, and observes available filesystem
blocks against `min_free_bytes`. A mount beneath an otherwise same-filesystem Git
root cannot silently evade the per-source check. No source objects or source
configuration are altered by the probes.

**Free space is an observation, not a reservation or OS quota.** The host-gateway
extension adds import-stream checks every 4 MiB and before publication using the
configured minimum. Concurrent indexing, capture pins, compaction and unrelated
services still require extra headroom. Published logical-byte retention is a
separate limit. Actual OS-level ENOSPC fault acceptance is not claimed.

All public and peer ports are bound before any starts serving. A peer port
conflict releases earlier bound sockets and fails the command-line process.
Unexpected listener failure is returned through the command-line runtime, not
merely logged while other listeners keep an apparently healthy process alive.
Construction/startup failures cancel primary workers and close retained ownership
when safe; they do not quietly disable the configured replica and continue.

## Shutdown and key changes

Shutdown stops peer admission, drains the HTTP servers, cancels overdue work,
and waits for active peer handlers before releasing retained storage. If a
handler has not exited by the deadline, closure reports failure and retains the
process lock. It never announces successful cleanup while a producer can still
access the store. Normal process exit/restart releases ownership through the OS.

Certificates and exact peer grants are loaded once per process. Use a controlled
primary/Edge restart for rotation or revocation; there is no file-watch/hot-reload
claim. Session tickets are disabled on the peer server. Each control/export RPC
also checks the current validity window of its verified client certificate chain,
so an existing busy keep-alive connection cannot extend an expired certificate.
This does not terminate a previously admitted streaming response instantly at its
certificate expiry, and is not an online CA revocation protocol.

The one-shot `wiki-reindex` command does not open a replication listener/store.
Its ordinary maintenance behavior is not an exception to the topology audit.

## Isolated acceptance evidence

`server/replication_binary_test.go` builds the real `gh-server` executable into a
temporary directory and seeds an isolated DB/Git fixture. It starts the executable
with an explicit config, connects via actual mTLS, and uses the normal Edge runtime
and native Git v2 through an unchanged canonical URL plus `curloptResolve`.
Clone, push and immediate clone of the new branch succeed; SIGTERM drains the
process and allows the retained root to be opened again. A second run with an
occupied peer port must exit unsuccessfully and leave the public port and retained
root available. These are isolated process tests, not production deployment acceptance.

Other tests cover missing user/client credentials, absent public peer routes,
stale identities, impossible headroom, overlapping paths, unsafe config/key files,
wrong certificate name, listener bind rollback and incomplete-handler drain.

```sh
go test ./server ./internal/replication ./internal/snapshotstore ./config -count=1
go test -race ./server ./internal/replication ./internal/snapshotstore ./config -count=1
go test ./... -count=1 -timeout=240s
go build ./...
go vet ./...
```

Before a live rollout, complete the reviewed enrollment/PKI workflow, review the
real writer/maintenance topology, use a patched Go/Git release build with exact
source attribution, and perform a restricted two-site canary without changing
team remotes. A writable mount into another Git service or an external maintenance
process is a rollout blocker until its writes are coordinated or removed through
a reviewed migration. Registered-node periodic prewarming is available; durable
event-driven discovery remains separate. See [host gateway](ags-edge-host-gateway.md).

Related: [AGS Edge architecture](../architecture/ags-edge.md).
