# AGS Edge operator enrollment

## Status

The remote operator client uses the native-admin registration API on the owning
primary; it does not open a live database through an ad-hoc program. It does
**not** deploy, restart, issue certificates, change node permissions
automatically, or approve production cutover.

The operator CLI has no transitive dependency on the primary bootstrap, business
Service/DB, GitStore or Edge runtime. All live metadata writes go through the
already-running authoritative primary. Use a patched exact-source release for
real deployment; `go build` below is a development build, not release provenance.

```sh
go build -o /tmp/ags-replication ./cmd/ags-replication
/tmp/ags-replication help
```

## 1. Enable registration on the owning primary

Configure a stable logical authority identifier on the existing primary:

```sh
AGS_REPLICATION_AUTHORITY_ID=primary-authority
```

This permits registration before a peer listener exists and does not start an
extra listener. With neither this setting nor a peer configuration, the operator
API returns 404. When AGS_REPLICATION_CONFIG_FILE is supplied, its authority_id
also enables registration. If both are supplied, they must match, otherwise
bootstrap fails before DB initialization.

Single-DB native authentication is required. Control-plane/embedded identity
modes and AllowAnyToken are not admitted. A native repository administrator can
observe/register that repository; ordinary read/write collaborators and delegated
transport Sessions cannot. An Access Grant bearer is not a native admin token.

The authority identifier is operator-owned configuration, not a hostname, and
must remain stable across controlled restarts. Changing it is a planned migration,
not a supported casual rename. Existing BASE_URL and team remotes remain unchanged.
No identity is derived from X-Forwarded-* or from an API request body.

## 2. Observe, then register the exact repository

Use an existing administrator credential in a regular owner-only file. Do not
pass a token on the command line, copy it into a config/receipt, or enable shell
tracing. The CLI does not auto-load .env, credential helpers or AGS databases.

```sh
umask 077
/tmp/ags-replication status \
  --primary https://primary.example.test:6666 \
  --repo owner/repository \
  --token-file "$HOME/.config/ags/admin-token" > observed.json

/tmp/ags-replication register \
  --primary https://primary.example.test:6666 \
  --expected observed.json \
  --token-file "$HOME/.config/ags/admin-token" > registered.json
```

`--allow-private-http` is explicit acknowledgement of a separately encrypted,
access-controlled path such as the existing private network. Without it, only
HTTPS or loopback HTTP is accepted. HTTPS keeps certificate validation; an
optional --ca-file supplies an explicit CA. Environment proxy settings and
redirects are not used. The tool does not forward credentials to a new origin.

`status` is read-only: it returns authority, canonical repository path, numeric
ID, creation timestamp, and optional already-provisioned identity. It never
initializes Git storage, allocates an identity or grants a node access.

`register` sends one POST bound to the previously observed authority, repository
ID and creation timestamp. Allocation happens inside the owning primary's
capture barrier after fresh native admin checks. An exact repeat is idempotent;
rename preserves identity, but a same-name replacement requires a new explicit
observation. Creation time also distinguishes recycled numeric IDs. No request
can choose a store ID or add a peer.

After an uncertain POST result, GET status and compare it with the original
observation. Do not replace the observation automatically and retry POST, because
that could enroll a same-name replacement. The CLI does not retry mutations or
follow redirects. Failed responses never echo the raw token or upstream body.

### HTTP contract

```text
GET  /api/v3/repos/{owner}/{repo}/replication/identity
POST /api/v3/repos/{owner}/{repo}/replication/identity
```

POST is a bounded closed JSON object:

```json
{
  "version": "ags.replication.register.v1",
  "expected_authority_id": "primary-authority",
  "expected_repository_id": 101,
  "expected_created_at": "2026-01-01T00:00:00Z"
}
```

The example timestamp/ID are placeholders, not values to substitute for the
actual GET response. Unknown/duplicate fields, malformed/trailing JSON, queries,
compressed bodies and ambiguous authorization headers are rejected. Responses
are no-store. Stale expectations return 409. This route is documented in the
REST extension OpenAPI; it is not a node-authenticated peer endpoint.

## 3. Build matching primary/Edge bindings from a node certificate

Obtain a client-auth node certificate from the deployment's dedicated peer PKI.
The operator command consumes public certificates only; it neither creates nor
exports any CA or node private key. Private keys stay in their controlled node
locations. CA issuance and revocation policy remain deployment-owned.

```sh
/tmp/ags-replication peer-plan \
  --edge-id edge-1 \
  --certificate certificates/edge.pem \
  --ca-file certificates/peer-ca.pem \
  --registration registered.json > edge-plan.json
```

Repeat --registration for additional registered repositories. The plan contains:

* `peer`: the exact edge_id, SPKI SHA-256 and store allowlist for primary peers[].
* `bindings`: matching canonical repository/identity entries for the Edge config.
* Certificate validity dates, a version, and `applied: false`.

The node leaf must explicitly allow client authentication and chain to the
supplied CA at the time of planning. Expired/future, server-only, CA-as-node,
malformed or untrusted certificates are rejected. The certificate's public key,
not its Common Name, determines the fingerprint. A different key with the same
name generates a different node grant. Private-key possession is verified later
by mTLS; the offline plan is not proof of possession or live user authority.

Registration files are inputs for an operator-reviewed plan, not signed bearer
authorizations. The primary still independently checks the persisted identity at
startup and original-user authority on every read. Duplicate repository names,
unregistered receipts and mixed primary authorities fail plan generation.

Review the plan before copying peer into the primary config and bindings into
[the Edge config](../examples/edge-read.example.json). Use the existing
[primary listener guide](ags-edge-primary-listener.md) for controlled configuration
and restart. No part of this command modifies either file automatically.

## Rotation and removal

Configurations are loaded at startup; there is no hot-reload claim. For planned
rotation, generate a plan from the new certificate, replace the reviewed grant
at the primary, update the Edge's certificate/key, and use a coordinated restart.
The CLI never merges new keys into old allowlists automatically. Removing a
node means removing its primary grant and restarting the owning primary; changing
its display name is not revocation. Already-admitted response streams have the
same documented lifetime behavior as the peer listener.

## Topology gate

Audit the actual writer and maintenance topology before setting
`managed_writes_only`. A writable mount into another Git service or an external
maintenance process is a separate writer that does not share the primary's
in-process capture barrier. Do not remove such a mount without a reviewed
migration. Begin acceptance with a dedicated source without out-of-process
writers. Root or Docker-socket access also requires operational review; a
directory inventory alone cannot prove that arbitrary processes cannot write.

## Verification

Tests use real primary HTTP routes/native auth and the CLI library, plus a built
operator executable. They verify no mutation on GET, exact/idempotent POST,
rename/recreate handling, stale creation facts, non-admin/delegated denial,
closed OpenAPI/JSON, missing opt-in, mismatch-before-bootstrap, TLS certificate
usage/trust, no retry/redirect/credential disclosure and database-free dependencies.
These tests use isolated fixtures; they do not mutate production configuration
or perform a live rollout.

```sh
go test ./internal/replicationadmin ./cmd/ags-replication ./config -count=1
go test ./server ./internal/service ./internal/rest -run 'ReplicationRegistration|RegistrationAuthority' -count=1
```

Certificate verification reference: https://pkg.go.dev/crypto/x509#Certificate.Verify
