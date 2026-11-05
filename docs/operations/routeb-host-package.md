# Route B host package contract

Current Home for the host-package source contract. This page does not authorize
provision, a production window, a tag, or a deployment.

The single user entry remains `scripts/ags-maintenance-transaction`.
`--mode host` dispatches to `scripts/routeb_host_transaction.py`. The default
command stays the isolated rehearsal and still refuses a real `~/.ags`.
A host plan without a private package writes nothing (`package_missing`).
Host mode does not accept `--manager-dir` or `--fault-after`.

Host mode loads a private package and its fixed sidecars through
`routeb_host_package.py`. `plan` reads the real registered host, manager,
running generation, lifecycle authority, recovery group and native capabilities.
`status` reads the existing transaction journal. `run --execute` uses
`routeb_host_execution.py`, `ColdWindow` and fixed native manager commands; it
does not execute the old injected fixture chain. An admitted isolated chain
seals a backup, runs independent restore verification, and starts once. An
unknown start is not replayed. Stop and start each re-read the window approval.
An expired approval stops before the manager call.

The current executable slice admits the same application revision and tree on
both sides of the transaction. The tool bundle has a separate revision, tree
and byte manifest. This permits validating host tooling without introducing
the mainline database migration or losing an already deployed CI hotfix.
An exact-source candidate is not a Release and supplies no release pin.

Native roots are fixed under the runtime account's real home at
`.local/state/ags-routeb-native/<transaction>/ags`. Their identity and tool
bundle are independently registered under root-owned
`/usr/local/libexec/ags-routeb-native/<transaction>`. No environment variable or
CLI path can select another trust profile. The user-owned launcher verifies
that bundle before loading its runtime core. The manager must already be a
one-shot definition: launchd `KeepAlive=false`, or user systemd `Restart=no`.
Registration and lifecycle activation are separate explicit provisioning
steps; the run command neither invents an approval nor expands sudo privileges.

Production plans still report `native_full_closure_required`. The current
source does not claim a complete native run, M6 acceptance, or an installed
production host package. Missing registration, an old automatic-restart
definition, unknown process/holder state, foreign metadata without an explicit
authority record, or a non-active lifecycle epoch prevents a stop.

The complete executing bundle and its fixed identity must be present in the
recovery inventory. Root-owned immutable files use the versioned
[authority archive contract](routeb-immutable-authority.md); user-owned
launcher, manager, config and application data retain ordinary full-copy
metadata requirements. The entry never performs an implicit root restore.

`scripts/routeb_observation.py` reuses `recovery_inventory` and `recovery_gate`
from [maintenance windows](maintenance-window.md). Elapsed time starts at
validation. 599 seconds is short. A lease longer than the elapsed window is
uncovered. A 200 `/readyz` with an unknown provider result is refused.
`readyz` stays `diagnostic_only` and is not a webhook proof.

`scripts/routeb_provision.py` is a read-only plan. A missing import, the wrong
interpreter, the wrong owner, or missing native evidence closes the plan.
The older `routeb_native_evidence.py` and `/usr/bin/true` harness only validate
source-level input shapes. Their caller-supplied booleans and duration do not
prove a native lifecycle. Real post-start samples come from
[native capture](routeb-native-capture.md), which binds original artifacts,
OS identity, the start log offsets and monotonic intervals. A capture still
does not replace full-restore, old-start, business or cleanup evidence.

Ordinary source CI does not run the launchd experiment. Set
`AGS_ROUTEB_NATIVE=1` only for an explicitly authorized disposable label.
That run is not a source-CI pass and does not authorize deployment.

Related pages: [runbook](routeb-runbook.md), [mini](routeb-runbook-mini.md),
[backup and recovery](backup-recovery.md), [local upgrade](local-upgrade.md),
[provisioning](upgrade-provisioning.md), [external dependencies](routeb-external-dependencies.md),
[native capture](routeb-native-capture.md), [immutable authorities](routeb-immutable-authority.md).
