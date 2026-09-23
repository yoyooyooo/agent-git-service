#!/usr/bin/env bash
# Disposable GitHub-hosted test database. Never operates a developer service.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${CI:-} == true ]] || { echo 'GitHub Actions only' >&2; exit 2; }
[[ ${GITHUB_RUN_ID:-} =~ ^[0-9]+$ && ${GITHUB_RUN_ATTEMPT:-} =~ ^[0-9]+$ && ${GITHUB_JOB:-} =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid job identity' >&2; exit 2; }
name="ags-test-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-${GITHUB_JOB}"
owner="${GITHUB_RUN_ID}:${GITHUB_RUN_ATTEMPT}:${GITHUB_JOB}"
case ${1:-} in
  start)
    if docker container inspect "$name" >/dev/null 2>&1; then
      echo 'Refusing to reuse an existing database container' >&2
      exit 1
    fi
    if ! command -v mysql >/dev/null 2>&1; then
      sudo apt-get update -qq
      sudo apt-get install -y default-mysql-client
    fi
    docker run --detach --name "$name" --label "ags.ci.owner=$owner" \
      --cpus=2 --memory=4g -p 127.0.0.1:45400:4000 \
      pingcap/tidb:v8.5.7 --store=unistore --path=/tmp/ags-test \
      --host=0.0.0.0 -P 4000 --status=10080 -L error
    for attempt in $(seq 1 60); do
      if mysql --protocol=TCP -h127.0.0.1 -P45400 -uroot --connect-timeout=2 -N -e 'SELECT tidb_version()' >/dev/null 2>&1; then
        echo 'Disposable TiDB ready on loopback'
        exit 0
      fi
      sleep 2
    done
    docker logs --tail 40 "$name"
    echo 'TiDB readiness timed out' >&2
    exit 1
    ;;
  stop)
    if ! docker container inspect "$name" >/dev/null 2>&1; then
      exit 0
    fi
    actual=$(docker inspect --format '{{index .Config.Labels "ags.ci.owner"}}' "$name")
    [[ $actual == "$owner" ]] || { echo 'Database owner mismatch; refusing stop' >&2; exit 1; }
    docker stop --timeout 15 "$name"
    ;;
  *) echo 'Usage: ci-database.sh start|stop' >&2; exit 2 ;;
esac
