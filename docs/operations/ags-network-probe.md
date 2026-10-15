# Hosted-runner deployment network probe

The optional `AGS network probe` workflow answers one question: can a GitHub-hosted
runner reach the configured private deployment webhook with verified TLS? It sends
an unauthenticated GET and accepts only HTTP 405 from the POST-only webhook route.
It never sends a deployment POST, uses a deployment webhook token, or requests a
deployment. Success does not prove webhook authentication, CI acceptance, or a
healthy deployed service.

## Boundaries

The workflow lives in `.github/workflows/ags-network-probe.yml`, separate from
`Fork CI`. Only `workflow_dispatch` triggers it. The job requires a branch ref
whose name equals the repository's actual default branch and the maintained
repository identity, before acquiring its environment. Checkout uses the exact
dispatch SHA. There is no caller-supplied checkout ref, target URL or token input.

Its `ags-network-probe-` concurrency namespace cannot cancel `fork-ci-` regression
runs or the production handoff. Repeated probes on the same ref may cancel only
other probes. A green probe is not a green regression suite. `Fork CI` has no
probe-only input or early-success shortcut.

The production workflow listens only to `Fork CI` and separately requires a
successful default-branch push. It does not consume `AGS network probe` results;
its existing environment protections and deployment credentials are unchanged.

## Operator environment setup

Use the separate `ags-autodeploy-probe` environment. Its selected-branch policy
must allow only the exact default branch, not all branches, tags, pull-request
refs or the temporary branch previously used for rehearsal:

```yaml
deployment_branch_policy:
  protected_branches: false
  custom_branch_policies: true
deployment_branch_policies:
  - name: <exact default branch from the repository API>
    type: branch
```

Keep shared OAuth `TS_OAUTH_CLIENT_ID` and `TS_OAUTH_SECRET` environment secrets and the
`AGS_DEPLOY_WEBHOOK_URL` variable. The network identity must remain limited by
the tag:ci network policy to the intended Dagu service on TCP 443. Never add the production
webhook token to this environment or expose its credentials to ordinary CI.
Retain any additional environment protection rules already required by the owner.

The URL must be HTTPS on a `.ts.net` host with the exact path
`/api/v1/webhooks/ags-deploy`, without userinfo, query parameters or fragments.
Redirects and all statuses other than 405 fail. No real endpoint or credential
belongs in this source tree.

Environment branch rules are GitHub state, not changed by a source merge. When
the repository default branch changes, update the one allowed environment branch
explicitly. A missing or mismatched rule must block the probe, never widen access.

## Run and verify

After the workflow is merged into the default branch, use the official CLI from
this repository, or select **AGS network probe** in the Actions UI:

```bash
repo=yoyooyooo/agent-git-service
default_branch="$(gh repo view "$repo" --json defaultBranchRef --jq .defaultBranchRef.name)"
gh workflow run ags-network-probe.yml --ref "$default_branch" -R "$repo"
gh run list --workflow ags-network-probe.yml --event workflow_dispatch --limit 5 -R "$repo"
gh run view <run-id> --log -R "$repo"
```

Check the selected run's head SHA, event, workflow name and successful `probe` job.
The output must include `method: GET`, `http_status: 405`, `tls_verified: true`,
`deployment_token_used: false`, and `deploy_requested: false`. A skipped job is not
reachability proof. Confirm the environment branch policy still names only the
actual default branch; the workflow guard is defense in depth, not a replacement
for that policy.

Source checks require no private network or environment secret:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_ags_deploy_probe.py
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_ags_deploy_handoff.py
```

These checks cover GET/no credentials, URL/status rejection, separate workflow
and concurrency, default-branch admission, and rejection of manual probe events
by production handoff. Only a successful hosted run establishes live reachability.

The separate manual `Tailscale OAuth verify` workflow checks the injected generation, single-tag CI identity, Dagu health and denied TCP destinations without a deployment POST. Default-branch environment restrictions remain. Retire the old auth-key secret only after the environment canary passes.

负向 TCP 探针使用经 Tailscale Services API 读回的 AGS VIP（100.101.222.132、100.66.212.180）；被拒绝的服务名称可能不出现在 runner 的 MagicDNS 中。DNS 不可见不当作 TCP 拒绝证据。服务重建导致 VIP 改变时，先读回并同步更新三个仓库的 canary。
