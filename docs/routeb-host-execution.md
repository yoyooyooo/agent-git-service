# Route B native host execution

Native bootstrap activation, host execution and managed startup require a
live registered native supervisor. Direct Python entry, an executable hash,
or a file-only catalog check still refuses with
`interpreter_full_closure_required`. Isolated Linux can satisfy this boundary
only through the fixed root systemd supervisor and its complete interpreter
authority. There is no runtime override; see the
[host package contract](operations/routeb-host-package.md#interpreter-execution-remains-closed).

The component flow described below connects an exact-source package to the
durable cold window, independent restore verification, one manager start and
continuous observation. It is source-tested and is not an admitted native
workflow. A successful observation remains evidence: `accepted` and
`installable` stay false. Production also retains `native_full_closure_required`
until the complete native acceptance contract is satisfied.

## Entry and admission

The isolated Linux v2 manager starts the fixed root `ags-routeb-stage0`
image. Its registration fixes the operation, child credentials, canonical
loader and interpreter, and frozen root-owned bundle. After complete native
verification it executes the selected Python entry with `-I -B -S`. The
entry independently binds the live kernel-authenticated parent and registered
bundle. A checkout, direct interpreter invocation, or caller-supplied context
does not satisfy this boundary. The platform contract and remaining native
acceptance requirements are specified in the
[interpreter authority design](design/routeb-interpreter-authority.md).

The host command accepts these operations:

```text
plan --host HOST --package ABSOLUTE_PACKAGE --approval ABSOLUTE_APPROVAL
run --host HOST --package ABSOLUTE_PACKAGE --approval ABSOLUTE_APPROVAL --execute
continue-installed --host HOST --package ABSOLUTE_PACKAGE --approval ABSOLUTE_APPROVAL --execute
status --transaction-id TRANSACTION --journal ABSOLUTE_HOST_JOURNAL
```

`plan` and `status` are read-only. `run` and `continue-installed` require the
explicit execution flag and their own registered native invocation.
The package fixes the runtime root, native manager, candidate, recovery group,
interpreter, tool bundle, profile and approvals. No manager-directory adapter,
fault hook, alternate HOME or environment import path is accepted as native
evidence. The older maintenance wrapper still supports isolated component
tests; its fixtures do not satisfy this host entry.

Before creating a claim or stopping a process, execution checks the registered
host, current package and dependency bytes, candidate version, actual old
installation, native process ownership, backup capacity and recovery metadata.
The durable lifecycle must already have active authority at epoch 1, with the
same host and no unresolved effects. This version refuses later epochs rather
than manufacturing an epoch transition. Provisioning a new isolated native
root requires its own explicit authority record; a package cannot activate
itself or substitute a fabricated S4 completion.

## Transaction and recovery boundary

The private `state/ags-autodeploy/local-v2/pending` directory is an exclusive
claim. `host-execution.json` records intent before each external effect and
keeps the admission, package and approval hashes. An existing claim is never
resumed by a new `run` call.

Execution follows the durable cold-window sequence:

1. Acquire the transaction owner lock and recheck admission and lifecycle.
2. Record stop intent, stop the fixed manager and prove native writers closed.
3. Seal the complete old backup, retaining original installation bytes.
4. Restore into the separate verification root and validate its complete data
   and metadata before moving it through the native Trash facility.
5. Under both cold locks, recheck approval, backup and candidate bytes, then
   atomically publish the binary and installation receipt with an intervening
   installation marker.
6. Record start intent and issue the one authorized native manager request.
7. Observe the actual new runtime, native parent and child, boot, journal,
   consumed handoff, installed hashes and readiness before freezing evidence.
8. Immediately begin the continuous native observation for at least 600 real
   seconds and at least the profile's lease duration.

Every mutating boundary rechecks the original approval and candidate. A failed
publication, expired approval or unknown start result retains the claim and
recovery fence. Execution neither refunds a consumed start nor automatically
retries a manager request. Recovery and old start use their separate approvals
and verified recovery contract.

The installation receipt identifies the actual exact source and tree, binary,
candidate manifest and tool bundle. It does not relabel that build as a release
or retain a stale release attestation.

## Installed state continuation

`ColdWindow.reopen_installed` lets a new coordinator object read the same
exact-source cold window after installation completed and before any start
intent. It requires the original owner and both cold locks, an explicitly
bound cold-journal hash, and the caller's continuation authorization check.
It verifies the original claim, sealed backup and independent restore report,
retained old installation bytes, unchanged recovery data, current writer
closure, and the installed candidate identity. The complete installation
receipt must match the same pure derivation used by the installer. Its raw
hash must also match the hash recorded at installation.

The rebind writes no files and retains the same generation. It does not repeat
stop, copy, restore or install, create a replacement claim, or change the
host journal's recorded failure. Only the existing `start_intent` and `start`
methods can subsequently publish and consume the original one-use handoff.
Any partial install, prior start intent, unknown writer, changed protected
data or changed evidence is refused. A change outside the recovery group does
not expand that group's data comparison.

The isolated Linux host entry connects this component to a separately
registered `continue-installed` invocation. `run` still refuses an existing
claim. The initial root registration must reserve exactly one continuation
slot, the original transaction slot and the existing initial/candidate serve
slots. A later approval cannot add an invocation to an old registration.

After the actual installed failure, the authorized root provisioner may write
one `continuation-registration.json` under the fixed native registry. This
typed record binds the original registration and transaction invocation, host,
cold binding, generation, original failed host and cold journal hashes, and
sealed snapshot, manifest and group. Its sole approval is fixed at
`<transaction>/continuation/approval.json`. The approval's purpose is
`continue-installed`; restore or old-start permission cannot substitute for it.
The command's package and approval arguments still select the original fixed
package/window files. They cannot select the late continuation authority.

Before claiming the new invocation, stage0 reads the original root-owned stop
intent/result, initial service Context/ExitRecord and normal manager completion.
It verifies the original failed controller's claim, manager-start anchor,
Context, bound exit and actual terminal state. Only the covered child exit 78
failure is admitted; a user journal, unknown exit or still living controller
cannot establish that the service is stopped. Any candidate ticket, claim,
manager-start intent or other candidate effect record refuses continuation,
even if no new process is visible.

Under the existing owner/install/runtime locks, the child reconstructs the
same stopped identity from that root proof. It reads the old installation from
the fixed pending preimage and sealed backup, then verifies the complete
derived installed receipt. It exclusively archives the original journal bytes
in `pending/continuation/` and writes its own host journal and evidence there.
The original host failure remains unchanged. The same cold generation then
uses the original `start_intent`, manager request, handoff consumption and
observation tail. No stop, copy, restore or install is repeated.

A failed continuation keeps its consumed controller slot. If the root proves
that no candidate effect was initiated and every claimed controller has the
covered terminal failure, the independently approved full-restore path may use
the original normal stop. This is a positive root readback, never a fallback
after a failed active stop. Once a candidate start is initiated or unknown,
the prestart restore route is refused. After a successful restore, old-start
uses the root-sealed historical stopped identity; a legitimately running old
service does not have to become inactive again to read that history.

Source tests cover the continuation and refusal paths using private fixtures
and explicit mock root peers. They do not establish a complete native
continuation, mount-alias, Docker, manager or production acceptance. That
requires the real isolated Linux lifecycle and its original evidence.

## Observation and running health

Startup evidence includes raw process and manager facts, verified lifecycle
journal, installed identity, readiness and log bytes from the pre-start
boundary. The execution receipt binds the fixed startup manifest by SHA256;
the collector verifies every original artifact and the exact range boundary
both before observation and before declaring it observed. Its log range starts
at the exact end of that archive. Rotation, truncation, changed generation, changed boot, drifted
bytes or an observation gap fail the collection; snapshots do not manufacture
elapsed time. The host journal points to `observation/capture.json`.

Periodic runtime inspection distinguishes the currently living generation
from a new spawn. A consumed cold-window handoff remains healthy only for its
own verified host, active epoch, generation, binary, installation receipt and
actual supervisor/child relationship. Startup still rejects a second consumer.
The health check does not consume, recreate or refund permission.

Component tests cover interrupted installation, changed or expired admission,
unknown-start replay refusal, running-health ownership and native entry trust.
They use explicit fixtures where needed and do not constitute a native pass.
Native observation alone also does not close M6: business operations, provider
projection, Git/PR/wiki behavior, full recovery, old one-time start and native
cleanup must each have their required evidence before production admission.
