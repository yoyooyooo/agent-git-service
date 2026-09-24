# Public release gate

Changing repository visibility is the last step, not a way to clean a repository. This preparation does not approve public release.

## Source and provenance

Build the new generation from the frozen official upstream base plus reviewed capability commits. Keep the previous private generation and its full recovery bundle in a private evidence store. Never push `--mirror`, `--all`, or all local tags to a public repository. Do not put private archival refs into the new public commit ancestry.

Before accepting a generation, record the disposition of every old capability and the relationship to its replacement tests in a private ledger. A smaller commit count is not evidence that bugs were fixed or behavior was preserved. Keep upstream copyright and license notices unchanged; use a maintainer-approved public Git identity for new commits.

## Content review

Inspect source, tests, fixtures, examples, generated files, commit subjects/bodies, author and committer identities, and release metadata. Tests are not exempt: a deterministic fixture can still contain a real workspace, repository, operator hostname or personal path. Anonymize related inputs and derived digests together, and rerun the consumer contract tests.

Reusable examples use `primary.example.test`, `edge.example.test`, documentation IP ranges, `/srv/ags`, and synthetic account/repository names. Actual network topology, account paths, certificates, credentials, task transcripts and deployment receipts remain private. Externalize operator scripts rather than replacing a few strings while leaving their production side effects enabled.

Run both the read-only history-object check and an independent secret scanner. The operator-specific deny policy stays outside the source tree; reports contain rule/location metadata but no matched values. A scanner pass is bounded evidence, not a guarantee of absence. Known false positives require narrow, testable exceptions; never disable an entire rule, directory or commit merely to turn CI green.

## GitHub and former publisher review

The Git tree is only one publication surface. Inspect every remote branch and tag, PR refs and discussion, Issues, releases, Actions logs/artifacts/caches and any other attached evidence before changing visibility. Changing a private repository to public can expose its Actions history too.

Fence the previous automatic publisher before using the old backup repository as source authority. Changing a local `origin` alone does not disable an AGS-to-GitHub mirror, queued provider writes, release automation or other workstation clones. A repository rename or HTTP redirect must not let an old publisher accidentally write private history into a new public destination.

If the existing repository contains extensive private history, preserve it as a private archive and publish only a separately reviewed clean generation. For this project, the maintained public destination is also attached to the official GitHub fork network so the parent relationship is explicit in the UI. Repository-network membership does not relax disclosure gates: only the public upstream history plus reviewed downstream generation may enter that fork; private historical refs remain outside it.

## Required evidence

- Full capability disposition, migration review and exact-source tests.
- Complete upstream ancestry and tag inventory, with no invented version.
- Current-tree and reachable-history privacy/secret checks, plus manual review.
- Repository-specific former-publisher fencing and external-surface inventory.
- GitHub-hosted CI with read-only permissions, no private runners, no production secrets and no untrusted privileged trigger.
- A clean source identity and independently accepted build/release artifacts.

Public source acceptance does not authorize upgrades, database migrations, credential rotation or changes to any running AGS/Edge service. Those actions retain their own backup, compatibility and rollback gates.
