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

The Git root uses `<GitRepoDir>/<full_name>.git` with optional sibling
`<full_name>.wiki.git`, plus historical Wiki CAS under `.wikiblobs` and durable
auxiliary state. `GitStorageID` does not name its directory. Current installation
state is not a recovery archive. [Backup and recovery](backup-recovery.md) defines
the S1 complete recovery group and per-machine five-verified-backup retention
contract. S2 can copy a synthetic cold snapshot and check an offline restore
copy. It does not enable upgrade apply. A caller must inject a verified system
Trash function; there is no default implementation. Only successful restore
copies and sealed, restore-verified backups beyond the latest five per host are
moved to Trash. Failed partial backups and failed restore copies remain in place
for manual review, do not count toward five and are not automatically removed.
Seal time, host identity and validated seal/report binding determine eligibility,
not directory modification time. Insufficient space refuses before copying.

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

## Cold-window startup fence

The S3 [local upgrade component](local-upgrade.md#s3-private-cold-window-protocol)
adds `state/upgrade-cold.json`, independently of `installing.json`. Before spawn,
the launcher refuses pending upgrades with missing marker/claim/journal, unknown
phase, changed binary/receipt or mismatched generation. A fixed-manager handoff
is permitted only after durable `start_attempted` and manager-request intent.
The launcher consumes that token under runtime EX and install SH, fsyncs it
before spawn, and never refunds it. Repeated manager starts therefore refuse;
uncertain startup needs read-only diagnosis, not another start. The marker has
no TTL and S3 never removes it or automatically restores restart policy.
Direct installers also refuse a pending upgrade even after OS locks were lost.

S6.01 cold-window admission also uses the
[fixed host identity](local-upgrade.md#s601-fixed-host-identity). A missing or
changed profile blocks token consumption and spawn before the recovery check.
Startup dependency refusal remains exit 78. This does not provision a profile
or grant a normal restart.

Ordinary installations without pending upgrades retain the standalone launcher
path. A launcher provisioned for the new cold-window protocol needs adjacent
`upgrade_recovery.py` and its complete pinned dependency set; missing imports
with a pending upgrade fail closed. Provisioning those files or changing a
manager on a host is not part of this source slice. Runtime receipts now include
a unique generation, requested-stop/forced-kill flags, child exit status and
process-group-empty evidence. A fixed manager must separately prove supervisor
exit and restart suppression; a stop command returning zero does not prove it.
See [pre-start recovery](backup-recovery.md#s3-pre-start-recovery).

S6.02 adds shared stop and writer evidence in `upgrade_host_evidence.py`.
A manager command that exits zero while a writer pid is still alive is
`normal_stop_unproven`. Forced kill, a surviving descendant, boot or process
identity mismatch, and missing observation permission stay open. The S3 field
check is unchanged. `upgrade_bootstrap.py` refuses a legacy pid/time receipt;
a same-binary launcher transition can admit a real generation receipt and does
not upgrade the business binary. A crash at any bootstrap phase stays fenced.

## Runtime lifecycle

The launcher reads `config/runtime.env`, preserves ordinary authentication, starts
one owning primary with `<root>` as its working directory, forwards termination
to its process group, and waits for the child. It never starts a second primary
against a live Git directory. The service manager owns restart policy. The program
identity query completes before spawning the primary; failures after spawn always
reap the child. Requested shutdown permits an 80-second drain before terminating
the remaining process group, so the service manager needs a longer exit time.

Before launch, and once per minute while running, it checks the installed primary
hash, interrupted-install marker, environment, durable Git/database paths and
referenced replication files/certificate lifetime and snapshot filesystem free
space against the configured `min_free_bytes`. It writes the latest
`state/startup-health.json`. A later missing file changes this record and emits a
bounded diagnostic; it does **not** kill a still-working primary. This is startup
dependency visibility, not a substitute for primary/Edge readiness, full protocol
acceptance, online revocation checking or an external alert delivery service.

Startup health keeps the `ags.startup-health.v1` string-valued `checks` map for
compatibility. The additive `check_details` map supplies a machine-readable
`reason` for every failing check. `doctor` prints the same report that startup
and periodic checks persist to `state/startup-health.json` and emit to the bounded
runtime log. Reasons and numeric observations are allowlisted; configuration
values, environment names/values, exception messages and OpenSSL output are not
included. Environment syntax errors include a line number; missing or malformed
replication fields include only the known field name. Certificate probe failures
are separate from malformed replication configuration.

For example, insufficient space is an operational failure, not evidence that
replication configuration is corrupt:

```json
{
  "checks": {"replication_config": "ok", "replication_disk_free": "invalid"},
  "check_details": {
    "replication_disk_free": {
      "reason": "disk_free_below_threshold",
      "available_bytes": 1073741824,
      "minimum_bytes": 2147483648
    }
  }
}
```

Available space uses filesystem blocks available to the runtime user, not total
free blocks. Equality with the configured threshold passes. A failed filesystem
probe reports `disk_free_probe_failed` without pretending to measure zero bytes.
Periodic diagnostics also track reason/value changes when a status stays invalid,
and report recovery without restarting the running primary. This is still an
observation, not a quota or reservation; the native replication admission and
stream checks remain authoritative.

Malformed, unsafe or out-of-range retention configuration fails startup rather
than silently falling back to normal operation. A bounded diagnostic still records
the failed check. Program-identity and process-lifecycle failures update the fixed
`state/runtime-failure.json` with stage and error type only, never exception text,
subprocess output, credentials or user request content. A failed state write after
spawn cannot leave an unowned primary running.

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

## Release wiring boundary

The tag publication wiring described in [release operations](releases.md) does
not install or start these programs. V2 installation now has a complete-checkout consumer with a distinct verification-only
mode; the legacy production-pin gate remains disabled. The [local upgrade entry](local-upgrade.md)
is source preparation. Empty policy and explicit unimplemented-contract refusals block apply before effects. Each machine upgrade requires separate operator
approval, compatible verified installation, and its normal stop/start checks.
Do not use successful publication as a signal to restart a managed runtime.

### S5 admitted upgrade lifecycle

S5 [private-package admission](local-upgrade.md#s5-two-layer-admission) connects
S3's existing one-shot handoff, not a second runtime or installer. Root-owned
adapter provisioning must route its fixed manager through this launcher. The
writer probe and runtime stop evidence remain mandatory. Completed confirmation
does not clear the cold marker or authorize a manager restart; missing/consumed
handoff continues to refuse spawn. Restoring normal restarts and starting old
AGS after pre-start recovery remain separately scoped. Source and synthetic
manager tests are not a native launchd/systemd acceptance or deployment approval.
