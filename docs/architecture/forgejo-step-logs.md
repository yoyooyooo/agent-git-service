# Forgejo step logs for official gh

The AGS Forgejo adapter can return GitHub-shaped step log ZIPs without changing
Forgejo or the official `gh` client. The provider-host bridge must expose the
optional `step_logs` member of its existing, fully bound
`ags.internal-provider-log.v1` response. An older bridge continues to return the
original whole-job log; upgrading the client-facing AGS alone does not create
missing provider metadata.

## Exact boundaries

Forgejo 12 records `action_task_step.log_index` and `log_length` as logical line
ranges. `action_task.log_indexes` maps those lines to absolute byte offsets in
the decoded log. Its codec uses signed varints, as defined by
[Forgejo's LogIndexes implementation](https://code.forgejo.org/forgejo/forgejo/src/tag/v12.0.4/models/actions/utils.go).
The bridge decodes that index rather than treating a line number as a byte
offset or guessing from timestamps, runner messages or test results. The same
offsets apply after the existing bounded zstd decode. Raw job log endpoints
continue to return the complete text.

The optional `ags.internal-provider-step-logs.v1` object contains `byte_length`
and ordered `steps`. Each step carries the same number, name, status, conclusion
and nullable timestamps as the step metadata resource, plus half-open byte
`ranges` (`start` inclusive, `end` exclusive). Repository, current task, run,
job name, commit, ref and event checks remain mandatory before logs are read.
The adapter validates the layout again and requires every decoded byte to be
covered exactly once. Unknown, incomplete, overlapping, out-of-bounds, oversized
or expired metadata retains whole-job mode. A failed job without a recorded
failed step with output also retains whole-job mode.

## Output outside provider steps

Forgejo can store setup, teardown or other output outside its recorded steps.
Those bytes are preserved in an explicit AGS diagnostic segment named
`AGS: output outside recorded steps`. Its number follows the last provider
step; the metadata resource and ZIP derive it from the same task layout. This
segment is not a provider execution step. Its conclusion is copied from the
overall job so that `gh run view --log-failed` retains unassigned output from a
failed job; it does not claim that those particular lines failed. It has no
invented start or completion time and does not change the job conclusion or
required checks.

Within the diagnostic segment, unassigned ranges preserve their original order.
`gh --log` groups output by step, so that diagnostic segment appears after the
provider steps rather than interleaved in original job order. The raw job log
remains available when exact chronological order is needed.

## ZIP and client behavior

The archive uses `<job>/<step-number>_step.txt` and matches official gh's job-name
normalization: remove `/` and `:`, truncate to 90 UTF-16 code units, then trim
whitespace. Unsafe or colliding normalized names are omitted from the ZIP so
gh uses its existing job-ID log API fallback instead of selecting another job's
file. There are no guessed display-name disambiguators.

Official gh enters step mode when it finds any matching step file and silently
skips missing step files. AGS therefore emits a complete verified layout for a
job or its whole-job segment, never a partial set. Archive count and expanded
size limits still apply. These rules match
[official gh v2.101.0 log selection](https://github.com/cli/cli/blob/v2.101.0/pkg/cmd/run/view/logs.go).

`--log-failed` selects failed **steps**, not failed test cases. Successful tests
inside a failed test-command step remain visible. Whole-job fallback can still
display `UNKNOWN STEP` and includes successful steps as context; it preserves
evidence when exact attribution is unavailable.

Official gh caches run ZIPs using the run identity and start time. Acceptance
must use an isolated gh cache to observe a new provider response. AGS never
changes run IDs or timestamps to invalidate a client cache.
