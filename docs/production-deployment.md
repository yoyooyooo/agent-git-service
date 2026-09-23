# Production Deployment

This guide describes a production-oriented `agent-git-service` deployment. It
assumes a persistent TiDB Cloud Starter database, durable Git repository
storage, explicit production configuration, and operational probes.

For local evaluation, use [Quick Start](quickstart.md). TiDB Zero is useful for
trying the service quickly and can be claimed from its claim URL for longer
experiments, but production deployments should use a persistent TiDB Cloud
Starter instance.

## Deployment Shape

The production baseline is:

- one or more `gh-server` processes or containers
- TLS terminated by a load balancer or reverse proxy
- `LISTEN_MODE=production`, which exposes one HTTP listener on `PORT`
- TiDB Cloud Starter for relational metadata
- a durable POSIX filesystem mounted at `GIT_REPO_DIR`
- `/readyz` for readiness and `/metrics` for Prometheus scraping

Single-node deployments may use a local persistent disk for `GIT_REPO_DIR`.
Horizontally scaled deployments need shared POSIX-compatible storage for
`GIT_REPO_DIR` so every instance can read and write the same bare repositories.

## Prerequisites

- Go 1.25+ if building from source
- Git available in the runtime image or host
- A TiDB Cloud Starter instance
- A persistent filesystem for bare Git repositories
- A reverse proxy or load balancer for TLS
- A secrets manager for production environment variables

## Prepare TiDB Cloud Starter

Create or open a TiDB Cloud Starter instance, then create a dedicated metadata
database:

```sql
CREATE DATABASE IF NOT EXISTS `gh-server`;
```

Open the TiDB Cloud **Connect** dialog, choose the public MySQL connection type,
and build a Go MySQL DSN. Keep `tls=true`:

```env
DB_DSN=<prefix>.root:<password>@tcp(<tidb-cloud-host>:4000)/gh-server?parseTime=true&timeout=10s&tls=true
```

Use one application metadata database per deployed AGS environment. The same
database stores users, tokens, repository metadata, issues, pull requests,
workflow state, and other product records.

The upstream control-plane/tenant router has been retired. A source locator is
not a database selector; Access Grant rollout requires `CONTROL_PLANE_DSN` to be
unset. Verify the exact runtime authority path, not just `/readyz` DB health or
an endpoint's existence. Never silently reinterpret an old tenant configuration
as access to the root database.

## Configure Runtime

Set production values through environment variables rather than committing
`.env` files with real credentials.

```env
ENVIRONMENT=production
LISTEN_MODE=production
PORT=8080
BASE_URL=https://gh.example.com

DB_DSN=<prefix>.root:<password>@tcp(<tidb-cloud-host>:4000)/gh-server?parseTime=true&timeout=10s&tls=true
GIT_REPO_DIR=/data/repos

SECRET_ENCRYPTION_KEY=<base64-encoded-32-byte-key>

LOG_FORMAT=json
LOG_LEVEL=info
```

Important production notes:

- `ENVIRONMENT=production` skips local seed data. `ADMIN_LOGIN` and
  `ADMIN_TOKEN` are local-development seed settings and do not bootstrap users
  in production.
- `BASE_URL` must be the external URL clients use. REST responses and Git clone
  URLs are built from it.
- `SECRET_ENCRYPTION_KEY` should be set for any production deployment that
  stores Actions, Dependabot, or Codespaces secrets. If it is unset, the server
  generates an ephemeral key at startup and previously encrypted secrets may not
  decrypt after a restart or on another instance.
- `GIT_REPO_DIR` must survive restarts. Do not point it at an ephemeral
  container filesystem.

## Bootstrap Access

Production does not seed `octocat` or `local-dev-token`. Use one of these
supported access paths:

- Configure OIDC for human login with `OIDC_PROVIDER`, `OIDC_ISSUER`,
  `OIDC_CLIENT_ID`, and optional `OIDC_AUDIENCE`.
  Auth0 migrations should keep `OIDC_PROVIDER=auth0` (or rely on the default
  inferred from an `*.auth0.com` issuer) so existing `UserIdentity` rows keep
  matching the same human accounts.
- Configure connected login with `CONNECTED_LOGIN_ORIGIN`,
  `CONNECTED_LOGIN_API_ORIGIN`, `CONNECTED_LOGIN_CLIENT_ID`, and
  `CONNECTED_LOGIN_CLIENT_SECRET` when a non-OIDC provider should mint local AGS
  sessions. Use `CONNECTED_LOGIN_PROVIDER`, `CONNECTED_LOGIN_LOGIN_PATH`, and the
  `CONNECTED_LOGIN_*_CLAIM` settings to describe provider-specific paths and
  claims in configuration. The callback URL is derived from `BASE_URL` as
  `/auth/connected/callback`; do not configure a separate `APP_ORIGIN`.
- Register agent accounts through `POST /api/ext/v1/agents`, which returns an agent
  login, token, and default repository.

Connected login is for providers that do not expose standard OIDC
discovery and signed ID tokens, but do support browser login, authorization-code
exchange, and bearer-token userinfo. The request flow is:

1. `GET /auth/connected/login` redirects the browser to
   `{CONNECTED_LOGIN_ORIGIN}{CONNECTED_LOGIN_LOGIN_PATH}` with `client_id`,
   callback URL, and CSRF `state`.
2. The provider returns to `{BASE_URL}/auth/connected/callback` with `code` and
   `state`.
3. AGS exchanges the code at
   `{CONNECTED_LOGIN_API_ORIGIN}{CONNECTED_LOGIN_TOKEN_PATH}` using
   `CONNECTED_LOGIN_CLIENT_ID` and `CONNECTED_LOGIN_CLIENT_SECRET`.
4. AGS calls `{CONNECTED_LOGIN_API_ORIGIN}{CONNECTED_LOGIN_USERINFO_PATH}` with
   the returned bearer token.
5. AGS maps configured userinfo claims into the same local identity/session
   path as OIDC.

Slock-compatible example:

```bash
CONNECTED_LOGIN_PROVIDER=slock
CONNECTED_LOGIN_ORIGIN=https://app.slock.ai
CONNECTED_LOGIN_API_ORIGIN=https://api.slock.ai
CONNECTED_LOGIN_CLIENT_ID=...
CONNECTED_LOGIN_CLIENT_SECRET=...
CONNECTED_LOGIN_LOGIN_PATH=/login-with-slock/setup
CONNECTED_LOGIN_SUBJECT_NAMESPACE_CLAIM=server_id
CONNECTED_LOGIN_SUBJECT_NAMESPACE_SLUG_CLAIM=server_slug
```

For Slock, `CONNECTED_LOGIN_PROVIDER=slock` preserves the local
`UserIdentity.provider` key. `CONNECTED_LOGIN_SUBJECT_NAMESPACE_CLAIM=server_id`
preserves the subject shape `<server_id>:<sub>` and avoids cross-server subject
collisions; when this claim is configured, Slock must return `server_id`.
Callback JSON and console redirect metadata include both `subject_namespace`
and the configured alias `server_id`, preserving the old Slock response shape
without Slock-specific code.
`CONNECTED_LOGIN_SUBJECT_NAMESPACE_SLUG_CLAIM=server_slug` only improves
generated login candidates.

These values keep their defaults unless the provider differs:

```bash
CONNECTED_LOGIN_TOKEN_PATH=/api/oauth/token
CONNECTED_LOGIN_USERINFO_PATH=/api/oauth/userinfo
CONNECTED_LOGIN_RETURN_TO_PARAM=return_to
CONNECTED_LOGIN_SUBJECT_CLAIM=sub
CONNECTED_LOGIN_ACTOR_TYPE_CLAIM=type
CONNECTED_LOGIN_HUMAN_TYPE_VALUE=human
CONNECTED_LOGIN_AGENT_TYPE_VALUE=agent
CONNECTED_LOGIN_CLIENT_ID_CLAIM=client_id
CONNECTED_LOGIN_PREFERRED_USERNAME_CLAIM=preferred_username
CONNECTED_LOGIN_NAME_CLAIM=name
CONNECTED_LOGIN_PICTURE_CLAIM=picture
CONNECTED_LOGIN_AVATAR_URL_CLAIM=avatar_url
CONNECTED_LOGIN_DESCRIPTION_CLAIM=description
CONNECTED_LOGIN_SCOPE_CLAIM=scope
```

The Slock sample is based on the historical adapter contract. Confirm the
provider's userinfo response still returns the configured claims, especially the
actor type claim, before enabling it in production.

Open agent self-registration is intentional in the current product model. If
that is not appropriate for your environment, restrict access at the reverse
proxy or network layer until an admission flow is in place.

## Optional Capabilities

### Forgejo workflow actions

Production Forgejo workflow actions require file-backed integration config with: an enabled Feishu `outbound.events.projection_drift` target; explicit repository mappings; `forgejo.authority_policy.enabled: true`; the public AGS webhook URL; the non-secret Forgejo integration-bot login; and explicit `forgejo.authority_policy.action_principal_bindings` for every human actor allowed to request label-first actions. Each binding value is an immutable numeric AGS principal ID; same-login or provider-role fallback is forbidden. Keep the Forgejo projection token, separate authority-policy operator token, webhook secret, and Feishu webhook URL in secret files. The operator token is used only for live branch-protection/policy reads and read-only inspection of a bound actor's effective repository permission; projection, label, status, and comment writes retain the least-privileged integration-bot token.

Before enabling action traffic, run `go run ./cmd/forgejo-authority -config <path> -mode plan`, then `apply`, then `verify`. Confirm AGS projection status reports equal AGS/Forgejo base SHA and policy-managed head SHA values. `/readyz` returns `not_ready` when alerting initialization or live authority verification fails. Give the production readiness request at least a 30-second caller deadline; a shorter deadline can cancel a healthy multi-repository authority inspection. Explicit mappings with `enabled: false` are excluded from authority verification and periodic projection scans. `AGS_ALLOW_MISSING_PROJECTION_ALERTING=true` and `AGS_ALLOW_MISSING_FORGEJO_AUTHORITY_POLICY=true` are independent development/test-only opt-outs and remain visibly `degraded`.

Upgrade in this order: configure and test the projection-drift target; converge Forgejo authority policy; verify actor-to-principal IDs and exact ref convergence; deploy; then enable action labels. A signed label request is admitted only after live Forgejo write permission and the shared AGS `pr.rebase` evaluator agree; the durable intent records a deterministic binding revision and revalidates it at later effect seams. Denial removes the action label and sets `ags/status-blocked`. To roll back, stop action traffic first. Do not use native Forgejo rebase/update, weaker base protection, unconditional force, or direct projection pushes as rollback procedures. See [Forgejo Integration](forgejo-integration.md#production-readiness-and-repository-onboarding).

For a partial rebase incident, only the same nonterminal bound generation may resume its saved desired SHA and exact accepted-old-SHA lease after current authorization and ref checks. A fresh signed label or new action intent cannot adopt a pre-job, operator-inspected or newer head as recovery. Unknown source/provider drift requires explicit reconciliation; never repair it with an inferred lease, unconditional force or a manual provider push. See the [provider effect and recovery contract](architecture/provider-effects.md). Keep incident-specific deployment evidence outside public source.

### Browser Console And CORS

Set the console URL and allowed browser origins:

```env
CONSOLE_BASE_URL=https://console.example.com
OAUTH_DEVICE_VERIFICATION_URL=https://console.example.com/device-login
CORS_ALLOWED_ORIGINS=https://console.example.com
```

When `CORS_ALLOWED_ORIGINS` is empty, the server derives local development
origins from `CONSOLE_BASE_URL`.
When `OAUTH_DEVICE_VERIFICATION_URL` is set, device-flow clients show that
external console URL instead of AGS's built-in `/login/device` fallback page.

Allowed browser origins can also read the `X-Request-Id` response header via
`Access-Control-Expose-Headers` so frontend and API clients can correlate
server responses with request-scoped logs.

### Workflow Execution

Workflow execution is fail-closed by default. Enable it only when the container
runtime and sandbox limits are ready:

```env
ENABLE_WORKFLOW_EXEC=true
WORKFLOW_EXEC_IMAGE=bash:5.2
WORKFLOW_EXEC_TIMEOUT=2m
WORKFLOW_EXEC_CPUS=1.0
WORKFLOW_EXEC_MEMORY=256m
WORKFLOW_EXEC_PIDS_LIMIT=128
WORKFLOW_EXEC_NOFILE=1024
WORKFLOW_EXEC_TMPFS_SIZE=64m
```

### Semantic Search

Lexical search works without an embedding provider. Set these variables only
when semantic/vector-backed issue and pull request search should be enabled:

```env
EMBEDDING_API_KEY=<provider-api-key>
EMBEDDING_BASE_URL=https://api.openai.com
EMBEDDING_MODEL=text-embedding-3-small
EMBEDDING_DIMENSIONS=
EMBEDDING_CONCURRENCY=50
```

### Git HTTP Push Limits

The normal API body limit is not used for Git pushes. Tune Git push spooling
when large repositories or memory-constrained hosts need it:

```env
GITHTTP_MAX_PUSH_BYTES=
GITHTTP_SPOOL_DIR=/data/spool
```

An empty `GITHTTP_MAX_PUSH_BYTES` uses the default 2 GiB push limit. An empty
`GITHTTP_SPOOL_DIR` uses the system temp directory.

### Protected-branch source authority

An AGS `BranchProtection` row is also a Git HTTP authority boundary. Before
`git-http-backend` runs, AGS sends the exact protected refs to its managed
pre-receive hook. Direct `receive-pack` updates to those refs are rejected for
all Git HTTP credentials, including durable maintainers. Unprotected work
branches remain writable according to normal repository permission. The
service-owned PR merge and Forgejo merged-base synchronization paths operate
inside the Git store rather than through Git HTTP, so they remain the approved
way to advance a protected base.

For a repository whose source contract is PR-only, create and verify protection
for its base before declaring the contract enforced. A failed direct-push probe
must leave the base SHA unchanged, and a later PR merge must still advance it.
Do not treat Forgejo-only branch protection as proof that AGS origin rejects a
direct push.

If a protected base has already advanced without a source PR, do not reset,
force-push, or restore an older runtime snapshot. Freeze further direct pushes,
back up the runtime and relational/Git state, identify the exact authenticated
writer and ref updates from non-secret request logs, audit the full linear
range, and submit an additive adoption/remediation PR. That PR must state that
the original range lacked PR provenance; required CI and its authorized merge
establish a new reviewed authority tip without rewriting history. This section
applies to repositories hosted by the AGS product, not this fork's GitHub source
workflow. See [fork governance](../fork/README.md) and the
[provider effect contract](architecture/provider-effects.md).

## Run From Source

Build and run the binary from an exact accepted Git archive with the production
environment loaded by your process manager. `make build` rejects dirty or
hidden-index checkout state, rejects symlink/gitlink/non-regular tree leaves
and malformed raw tree modes, rejects standard/custom replace refs, disables
replace objects for every Git read, pins the accepted 40-hex `HEAD`, and
materializes that immutable commit into a
retained audit candidate, builds there, and injects the same revision into the
receipt source boundary. `make check` remains the dirty-worktree development
compile/vet gate and does not produce an attributable artifact. An `unknown` or
mismatched revision identifies an unsupported build: it fails closed and cannot
produce Boundary Receipts. Same-owner
adversarial mutation of Git object storage during the build is not claimed.

```bash
make build
./gh-server
```

For a simple systemd-style deployment, configure the environment in a protected
environment file and run the binary as an unprivileged user with write access to
`GIT_REPO_DIR`.

## Run As A Container

The Dockerfile builds a static `gh-server` binary and includes Git in the
runtime image:

Use the supported Make target so Docker receives the same verified archive
context plus exact commit/tree marker files. A direct `docker build .` from the
mutable checkout is not a supported attributable build. The Dockerfile rejects
missing or mismatched commit/tree markers and build arguments.

```bash
make docker-build
docker run --rm \
  -p 8080:8080 \
  -e ENVIRONMENT=production \
  -e LISTEN_MODE=production \
  -e PORT=8080 \
  -e BASE_URL=https://gh.example.com \
  -e DB_DSN="$DB_DSN" \
  -e SECRET_ENCRYPTION_KEY="$SECRET_ENCRYPTION_KEY" \
  -v /srv/agent-git-service/repos:/data/repos \
  gh-server:local
```

Terminate TLS at the load balancer or reverse proxy and forward HTTP to the
container port.

## Verify The Deployment

Wait for readiness. The first start can take a few minutes while schema
migrations and TiDB full-text indexes are created:

```bash
curl -fsS https://gh.example.com/readyz | jq .
```

For a supported deployment, `version` must equal the exact accepted 40-hex SHA
used by `make build` or `make docker-build`. `unknown` denotes an unsupported,
unattributed build and cannot support Boundary Receipt claims.

Expected ready response (replace the example SHA with the accepted SHA):

```json
{
  "status": "ready",
  "version": "78852533feba3ce8251358ec69eb1a59999ff89c",
  "checks": {
    "main_db": {
      "status": "ok"
    }
  }
}
```

Probe the GitHub-compatible discovery endpoints:

```bash
curl -fsS https://gh.example.com/api/v3/ | jq .
curl -fsS https://gh.example.com/api/v3/meta | jq .
curl -fsS https://gh.example.com/api/v3/rate_limit | jq .
```

Scrape Prometheus metrics from:

```text
https://gh.example.com/metrics
```

The Grafana dashboard JSON lives in
[monitoring/grafana/gh-server-operations-dashboard.json](monitoring/grafana/gh-server-operations-dashboard.json).

## Backups And Upgrades

Back up both persistence layers:

- TiDB Cloud Starter database backups for metadata
- `GIT_REPO_DIR` snapshots for bare Git repositories

Keep these backups consistent enough that repository rows and bare repositories
can be restored together. Before upgrading:

1. Back up TiDB metadata and Git repository storage.
2. Deploy the new binary or image to one instance first.
3. Wait for `/readyz` to return `ready`.
4. Run API and Git smoke tests.
5. Roll the remaining instances.

Schema migrations run at startup. During the first deployment of a new version,
allow enough startup time for migrations and full-text index changes.

## Production Checklist

- `ENVIRONMENT=production`
- `LISTEN_MODE=production`
- `BASE_URL` set to the public HTTPS URL
- `DB_DSN` points to TiDB Cloud Starter with `tls=true`
- `GIT_REPO_DIR` is durable and shared across instances when scaled out
- `SECRET_ENCRYPTION_KEY` stored in a secrets manager
- TLS terminated before traffic reaches `gh-server`
- `/readyz` wired as the load balancer readiness check
- `/metrics` scraped by Prometheus
- database and Git storage backups configured
- OIDC, connected login, or agent registration controls chosen for account
  bootstrap
