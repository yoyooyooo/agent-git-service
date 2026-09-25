# Single-root user runtime

The user-operated runtime has one durable home, `~/.ags/` by default. Software
versions identify verified bytes; they are not retained local installation slots.
A canary is an explicit configuration and acceptance scope, not a permanent copy
of the application, database, Git store and previous deployments.

## Ownership

```text
~/.ags/
  bin/                  current programs and the installed operator launcher
  config/runtime.env    current primary environment (owner-only)
  config/replication.json
  config/pki/           current replication trust and private keys
  config/retention.json
  data/                 authoritative database and Git repositories
  cache/                bounded derived snapshots, managed by their owning process
  state/                current installation, runtime and startup-health records
  logs/                 byte-bounded rotating runtime output
```

An explicit custom root is supported. The operator launcher uses `--root` or
`AGS_HOME`; these select the launcher's root, not a new implicit behavior of raw
`gh-server`. Existing database schemas, repository/storage identities, credentials
and endpoints remain unchanged by relocation.

Never reference `deployments/<date>`, a download directory, build checkout or
rehearsal workspace from live configuration. Relative replication paths resolve
from the replication JSON's directory, not the service working directory. Use
explicit durable paths and check all referenced files before switching roots.

## Program installation

The verified Python installer places the current three native commands directly
in `<root>/bin`. Its default root is `~/.ags`. There is no `current` selector,
`releases/<version>` collection, `stage` command or activation phase. The installer
preserves `config`, `data`, `cache`, `logs` and unrelated operator tools.

Downloads and extraction candidates live only for the installation transaction.
Normal success and exceptions clean them up. A small `state/installing.json`
marker fences a partially replaced program set: the launcher rejects startup
while it exists. Rerunning the same exact verified release can finish that
interrupted installation; a different target is rejected until reconciliation.
The installer lock serializes replacement, and the managed runtime's lock rejects
replacement while that runtime is running. Startup validation/spawn also shares
the installer lock. This is not an OS-wide transaction: direct unmanaged processes
still require operator coordination, and an uncatchable
process/host failure can leave temporary material for explicit reconciliation.

`state/installation.json` records verified identities and hashes, not a complete
archive. It is different from `state/runtime.json`, which identifies the running
process. Installing programs does not restart a service or migrate a database.
Immutable remote releases remain available for selecting exact bytes without
keeping every version on the user's disk.

The Bash wrapper uses a trusted adjacent Python installer when invoked from a
checkout or an operator-managed installation. The standalone wrapper obtains the
installer from an immutable Latest stable release independently of the requested
binary version. It rejects an installer predating this single-root contract;
selecting an old binary must not revive its old multi-version installer.

## Process and diagnostic output

An operator can install `scripts/ags-runtime.py` as
`<root>/bin/ags-runtime.py` and point the existing service manager at:

```bash
python3 ~/.ags/bin/ags-runtime.py --root ~/.ags doctor
python3 ~/.ags/bin/ags-runtime.py --root ~/.ags serve
```

The launcher reads `config/runtime.env`, preserves ordinary authentication, starts
one owning primary with `<root>` as its working directory, forwards termination
to its process group, and waits for the child. It never starts a second primary
against a live Git directory. The service manager owns restart policy.

Before launch, and once per minute while running, it checks the installed primary
hash, interrupted-install marker, environment, durable Git/database paths and
referenced replication files/certificate lifetime. It writes the latest
`state/startup-health.json`. A later missing file changes this record and emits a
bounded diagnostic; it does **not** kill a still-working primary. This is startup
dependency visibility, not a substitute for primary/Edge readiness, full protocol
acceptance, online revocation checking or an external alert delivery service.

Default stdout/stderr retention is five segments of 10 MiB each, including the
current file. `config/retention.json` can set `log_segment_bytes` (64 KiB–64 MiB)
and `log_backups` (0–8). Writes are split at byte boundaries, so a single unbroken
output stream cannot defeat the limit. Direct the service manager's own output
away from a second unbounded copy. Do not copy private logs into public sources.

The launcher does not recursively sweep arbitrary files in `logs` or `state`.
Current records use fixed filenames. Additional explicit incident evidence needs
its own size/lifetime bound; do not retain application/database copies as logs.

## Data and cache are not software versions

Never prune authoritative database or Git data as installation cleanup. A real
schema-changing operation may require a consistent, explicitly scoped backup;
that is a separate data-recovery decision, not a reason for every routine
installation to retain another full environment.

Replication can require a bounded set of recent Git views for negotiation and
incremental bases. These are correctness-related cache states, not software
version slots. Preserve reader/base leases and let the snapshot owner enforce
capacity, protection and expiry. Do not delete live snapshots from a generic
housekeeping process.

## One-time migration

Read the actual service entry, environment, database path, Git root, certificate
references, integration credential paths and client helpers before touching data.
Validate the replacement startup material first. Stop and drain the owning
primary; record database integrity and Git ref facts, move (not clone) live data
on the same filesystem, then start the same verified program from the durable
root. Revalidate primary/Edge identity, ordinary access, real Git objects,
canary push/readback and denial against warm snapshots. Reboot/restart from the
new entry to prove disk dependencies are complete.

Only after these checks may obsolete installation/download/build/rehearsal
material be retired under the operator's deletion policy. Check for remaining
active references, including credential helpers and service units. Moving files
to Trash/quarantine is not reclaimed space and must not be reported as such.
Do not retain the old runtime as a permanent compatibility copy or silently
change global deletion policy.

Regression: `python3 scripts/test_single_root.py` and
`python3 fork/scripts/test_install_sh.py` use isolated files and processes.
Private migration receipts are not evidence that all deployments were migrated.
