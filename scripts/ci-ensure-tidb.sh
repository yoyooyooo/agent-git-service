#!/usr/bin/env bash
# Reuse the fork's prebuilt runner, while supporting upstream non-root Ubuntu.
set -euo pipefail
export PATH="${HOME}/.tiup/bin:${PATH}"
if ! command -v mysql >/dev/null 2>&1; then
  if [[ $(id -u) -eq 0 ]]; then
    apt-get update
    apt-get install -y default-mysql-client curl
  else
    sudo apt-get update
    sudo apt-get install -y default-mysql-client curl
  fi
fi
if ! command -v tiup >/dev/null 2>&1; then
  curl --proto '=https' --tlsv1.2 -sSf https://tiup-mirrors.pingcap.com/install.sh | sh
fi
if [[ -n ${GITHUB_PATH:-} ]]; then
  printf '%s\n' "${HOME}/.tiup/bin" >> "$GITHUB_PATH"
fi
mysql --version
tiup --version
