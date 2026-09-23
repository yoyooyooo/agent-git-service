# Maintaining this fork

This source repository uses GitHub for collaboration and CI. Building or contributing to it does **not** require an AGS, Forgejo, Multica, private network, or maintainer-managed runner. The AGS product can still integrate with those systems when an operator explicitly configures it.

## Remotes and generations

- `origin` is `git@github.com:yoyooyooo/agent-git-service-fork.git`, the maintained clean GitHub repository.
- `upstream` is the official `ngaut/agent-git-service` source. Never push to it.
- `fork/<upstream-version>-main.<date>` identifies an accepted generation. If the official repository has no tags, use `fork/main.<date>` and pin its full baseline SHA; do not invent a release version.
- `feat/*` and `fix/*` branch from the active generation. Merge accepted work linearly into that generation.
- `prepare/*` is private working material, not an accepted generation and not safe to publish automatically.
- Immutable release tags identify exact tested source. A documentation-only manifest tip and the runtime build SHA are recorded separately.

The selected source baseline is in `UPSTREAM_BASELINE`. `UPSTREAM_BASELINE_TAG` is `none` because the remote tag inventory was empty at selection. Refresh and inspect upstream refs before deriving the next generation's name; never overwrite a release tag to repair a naming mistake.

A new upstream generation starts at the frozen upstream tree. Inventory the previous fork by **behavior**, then choose `upstream`, `keep`, `reimplement`, `externalize`, `retire`, or `blocked` for each capability. Old generations are evidence, not a queue of commits that must be replayed. In particular, do not move 290 historical commits into a public branch merely to preserve their messages.

## Accepted source and release boundary

The selected generation is tracked by [its manifest](releases/main.20260924.md). The [capability decisions](CAPABILITIES.md) and [preservation matrix](FEATURE_PARITY.md) separate retained contracts, reviewed repairs and explicit upstream retirements. [Publication](PUBLICATION.md) owns disclosure gates. Private source checkpoints and deployment transcripts are kept outside the public generation.

The active generation has completed its source and disclosure gates; the manifest records the exact fully tested source. Earlier trial and private operational history remain in separate private archives, not on this repository's refs. Do not publish local archive branches, use `git push --all`, or mirror the private object inventory into this destination. Public visibility and runtime deployment are separate explicit decisions: accepting source never upgrades a running primary or Edge.

## Verification

```bash
python3 fork/scripts/audit.py inventory --details
python3 fork/scripts/audit.py verify
python3 fork/scripts/test_audit.py
python3 fork/scripts/test_generation.py
(cd fork/tools && go test ./... -count=1)
python3 fork/scripts/audit.py tree --private-policy /private/operator-policy.json
python3 fork/scripts/audit.py publication --private-policy /private/operator-policy.json
```

`fork/scripts/generation.py` constructs new capability-scoped Git objects from an already-reviewed source and frozen baseline. It never edits a checkout or pushes; an owner-only policy outside the tree names allowed narrative substitutions and private evidence to externalize. Existing production Go blobs cannot change. `fork/tools/coverage.go` independently verifies production bytes plus existing test declarations, imports and non-string structure. These checks do not replace final exact-head tests.

The inventory includes empty commits, all changed paths, and explicit pending review state. `tree` scans the selected **committed** fork tree, not dirty working files, and explicitly does not examine commit metadata. `publication` examines all new reachable blobs **and commit metadata**, including removed historical content, and never prints matched values. A clean `tree` result cannot stand in for the historical gate. Its output is a preflight, not proof that there are no secrets. Run an independent secret scanner and review GitHub branches, tags, PRs, Actions logs and artifacts separately.

Hosted CI uses read-only GitHub permissions and disposable test dependencies. No workflow may use a self-hosted runner, production credentials, deployment keys, or `pull_request_target` to execute contributor code. CI caches dependencies, not private deployment data; it never deploys automatically.

## Product boundaries

The public architecture and operational guide for the optional Edge are in [`docs/architecture/ags-edge.md`](../docs/architecture/ags-edge.md). Reusable examples use documentation-only hosts and addresses. Real hostnames, account paths, node certificates, operator tokens, and acceptance transcripts belong to the operator's private configuration and evidence store.

Do not transplant another project's storage rules blindly. This server's existing GORM migrations, deployed SQLite data, TiDB test semantics and Git invariants remain binding compatibility constraints. Source cleanup must not turn a migration into data loss or broaden authorization.
