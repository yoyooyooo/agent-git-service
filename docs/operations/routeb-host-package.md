# Route B host package contract

Current Home for the host-package source contract. This page does not authorize
provision, a production window, a tag, or a deployment.

The single user entry remains `scripts/ags-maintenance-transaction`.
`--mode host` dispatches to `scripts/routeb_host_transaction.py`. The default
command stays the isolated rehearsal and still refuses a real `~/.ags`.
A host plan without a private package writes nothing (`package_missing`).
Host mode does not accept `--manager-dir` or `--fault-after`.

An admitted isolated chain seals a backup, runs independent restore
verification, and starts once. An unknown start is not replayed. Stop and
start each re-read the window approval. An expired approval stops before the
manager call.

`scripts/routeb_observation.py` reuses `recovery_inventory` and `recovery_gate`
from [maintenance windows](maintenance-window.md). Elapsed time starts at
validation. 599 seconds is short. A lease longer than the elapsed window is
uncovered. A 200 `/readyz` with an unknown provider result is refused.
`readyz` stays `diagnostic_only` and is not a webhook proof.

`scripts/routeb_provision.py` is a read-only plan. A missing import, the wrong
interpreter, the wrong owner, or missing native evidence closes the plan.
`scripts/routeb_native_evidence.py` accepts only
`ags.routeb-native-evidence.v1` with `kind=native`, a 40-character head, a
cleaned disposable label, and at least 600 seconds. A skipped file or a
fixture labeled native is not a pass. A valid file is still not installable.

Ordinary source CI does not run the launchd experiment. Set
`AGS_ROUTEB_NATIVE=1` only for an explicitly authorized disposable label.
That run is not a source-CI pass and does not authorize deployment.

Related pages: [runbook](routeb-runbook.md), [mini](routeb-runbook-mini.md),
[backup and recovery](backup-recovery.md), [local upgrade](local-upgrade.md),
[provisioning](upgrade-provisioning.md), [external dependencies](routeb-external-dependencies.md).
