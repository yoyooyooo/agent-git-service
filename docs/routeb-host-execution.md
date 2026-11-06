# Route B native host execution

Native bootstrap activation, host execution and managed startup currently
refuse with `interpreter_full_closure_required`. An executable hash does not
verify the interpreter's complete dependencies before it executes. There is
no runtime override for this gate; see the [host package contract](operations/routeb-host-package.md#interpreter-execution-remains-closed).

The component flow described below connects an exact-source package to the
durable cold window, independent restore verification, one manager start and
continuous observation. It is source-tested and is not an admitted native
workflow. A successful observation remains evidence: `accepted` and
`installable` stay false. Production also retains `native_full_closure_required`
until the complete native acceptance contract is satisfied.

## Entry and admission

Use the canonical interpreter recorded in the registered host profile with
`-I -B`, executing `routeb_host_transaction.py` from that profile's frozen,
root-owned bundle. The entry refuses an untrusted import directory before
loading the implementation. It also verifies that the executing bundle is the
bundle registered for the admitted host. A checkout is not a native entry.

The host command accepts these operations:

```text
plan --host HOST --package ABSOLUTE_PACKAGE --approval ABSOLUTE_APPROVAL
run --host HOST --package ABSOLUTE_PACKAGE --approval ABSOLUTE_APPROVAL --execute
status --transaction-id TRANSACTION --journal ABSOLUTE_HOST_JOURNAL
```

`plan` and `status` are read-only. `run` requires the explicit execution flag.
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
