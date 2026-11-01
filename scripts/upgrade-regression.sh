#!/usr/bin/env bash
# One test entry for local regression and hosted CI; only synthetic data.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
sandbox="$(mktemp -d "$HOME/ags-upgrade-regression-XXXXXXXX")"
chmod 700 "$sandbox"
# Deliberately retained for diagnosis, never removed by this runner.
printf 'Upgrade regression sandbox: %s\n' "$sandbox"
entries=(
  'scripts/test_upgrade_identity.py'
  'scripts/test_upgrade_host_evidence.py' 'scripts/test_upgrade_bootstrap.py'
  'scripts/test_upgrade_adapter_launchd.py' 'scripts/test_upgrade_adapter_systemd.py'
  'scripts/test_upgrade_metadata.py'
  'scripts/test_upgrade.py' 'scripts/test_upgrade_safety.py'
  'scripts/test_upgrade_inventory.py' 'scripts/test_upgrade_backup.py BackupTests'
  'scripts/test_upgrade_restore.py RestoreTests' 'scripts/test_upgrade_backup_regressions.py'
  'scripts/test_upgrade_quiescence.py' 'scripts/test_upgrade_recovery.py'
  'scripts/test_upgrade_manual_approval.py' 'scripts/test_upgrade_wireup.py'
  'scripts/test_install_v2.py' 'scripts/test_single_root.py' 'scripts/test_release_consumer.py'
  'fork/scripts/test_release.py' 'fork/scripts/test_release_binding.py'
)
for index in "${!entries[@]}"; do
  work="$sandbox/$index"
  mkdir -m 700 "$work" "$work/home" "$work/tmp" "$work/cwd"
  read -r -a args <<< "${entries[$index]}"
  args[0]="$root/${args[0]}"
  printf '\n==> %s\n' "${entries[$index]}"
  (cd "$work/cwd" && env -u PYTHONDONTWRITEBYTECODE -u PYTHONPYCACHEPREFIX -u PYTHONPATH -u BASH_ENV \
    HOME="$work/home" TMPDIR="$work/tmp" "$root/scripts/ags-python.sh" "${args[@]}" -v)
done
printf '\nUpgrade regression passed (%s entries). Sandbox retained: %s\n' "${#entries[@]}" "$sandbox"
