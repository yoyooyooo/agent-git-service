# Official gh command coverage and remaining provider gaps

Unmodified official gh is the standard client. AGS owns GitHub-shaped API
compatibility; provider-specific operator capabilities belong in AGSX. Do not
add a gh shim or new management features to ags-cli.

The command table describes the current server contracts. Local protocol
fixtures and the official-gh acceptance job exercise writes without production
credentials or data. Real deployments must configure the selected CI backend,
API origin and, for older Forgejo logs, the typed provider-log bridge described in
[CI backends](../architecture/ci-backends.md).

| Command | Status / scope |
| --- | --- |
| `gh repo view`, `gh browse --no-browser` | Supported repository discovery and configured browser origin. |
| `gh pr list`, `view`, `status`, `diff` | Supported; state filters and merged SHA are covered separately. |
| `gh pr create`, `edit`, `comment`, `ready` | Supported through native PR owners; isolated official-gh tests include durable readback. |
| `gh pr close`, `reopen` | GraphQL state writes refresh provider projections on reopen, like REST PATCH. Invalid IDs return errors rather than fabricated success. |
| `gh pr checks` | Current-head evidence from the selected backend. |
| `gh issue list`, `gh release list`, `gh label list` | Native read surfaces; an empty repository collection is valid. |
| `gh run list`, `view`, `view --exit-status` | Supported mapped run IDs; older Forgejo collection-only APIs have a bounded detail lookup. `--exit-status` preserves a failed run's exit status. |
| `gh run view --log`, `--log-failed`, `--job ID --log` | Supported with the configured log bridge. Missing step metadata is shown as `UNKNOWN STEP`. |
| `gh run watch --exit-status` | Official-gh tests cover pending → success and pending → failure. Optional unprojected annotations do not hide the CI result. |
| `gh api actions/runs/ID[/jobs\|/logs]` | Supported; select the AGS hostname explicitly and use AGS `databaseId`, not a provider number. |
| `gh api actions/runs/ID/attempts/1` | The exact observed Forgejo run is attempt 1; unobserved attempts are not invented. |
| `gh workflow list`, `view` | Forgejo exposes observed workflow identities with state `unknown`, not a complete enabled-workflow catalogue. Workflow path/YAML is unavailable when the provider does not expose it. |
| `gh run rerun ID`, `--failed`, `gh run cancel ID` | GitHub Actions supports its public API. The targeted Forgejo API does not: an existing run returns 501, never false success or a missing-run 404. No provider writes or native fallback. |
| `gh run rerun --job ID`, `gh run delete`, `gh workflow run/enable/disable` | External adapter management remains explicitly unsupported. |
| `gh run download` | External adapters do not publish artifacts; an empty artifact collection is not a claim that provider artifacts were inspected. Run-scoped artifact reads first validate the repository/backend/run identity; unknown or retired runs return 404. |

GraphQL and REST share the native PR state lifecycle. Provider convergence remains
an independent boundary: projection failure is logged after the native state
write, as on REST PATCH. A returned native OPEN state is not proof that a remote
provider has converged. Source acceptance does not deploy or restart anything.

## AGSX follow-up backlog

These are follow-ups, not implemented capabilities or permission to operate a
provider. Do not put them in ags-cli.

1. **Forgejo exact-run management**: research a supported, authenticated adapter
   for rerun-all, failed-only rerun, cancel and single-job rerun where the public
   API lacks them. Preserve exact repository, provider run ID, head SHA and
   operation scope; require readback after an unknown write. Do not substitute
   workflow dispatch, a dummy commit, another backend or SQL writes for rerun.
   If a supported public API becomes available, prefer adding it to AGS so the
   existing gh commands work without AGSX.
2. **Provider workflow catalogue/YAML and dispatch controls**: expose explicit
   provider discovery in AGSX only when official gh cannot represent the observed
   scope truthfully. An observed filename must not become a fabricated enabled
   workflow, guessed source directory, or new workflow dispatch.
3. **Provider artifacts, retention and deletion**: add provider-specific operations
   only with validated public APIs and explicit identity/authorization contracts.
   Until then, preserve empty/unprojected semantics and unsupported mutations.
4. **Observation diagnostics**: a scoped AGSX diagnostic can explain provider
   outages, bounded inventory exhaustion, missing bridge configuration and host
   selection. It must not install an alternate gh executable or print credentials.

Use [official gh setup](official-gh.md) for routing and
[CI backends](../architecture/ci-backends.md) for identity, completeness and logs.
