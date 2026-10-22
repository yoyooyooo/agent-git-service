#!/usr/bin/env python3
"""Real Forgejo protected-branch regression on a new disposable loopback container.

Uses generated fixture identities only. Never connects to a deployed provider.
Stops (does not remove) its own container on exit; Docker owns its retained data.
"""
import json
import os
import secrets
import subprocess
import time
import urllib.request


def command(args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, **kwargs)
    if result.returncode:
        # Commands that create fixture credentials must not echo their output.
        raise RuntimeError("fixture command failed: " + args[0])
    return result.stdout.strip()


def main():
    name = "ags-projection-test-" + secrets.token_hex(6)
    started = False
    try:
        command(["docker", "run", "-d", "--name", name,
                 "-p", "127.0.0.1::3000",
                 "-e", "FORGEJO__database__DB_TYPE=sqlite3",
                 "-e", "FORGEJO__security__INSTALL_LOCK=true",
                 "-e", "FORGEJO__service__DISABLE_REGISTRATION=true",
                 "-e", "FORGEJO__actions__ENABLED=false",
                 "codeberg.org/forgejo/forgejo:12"])
        started = True
        binding = json.loads(command(["docker", "inspect", name]))[0]["NetworkSettings"]["Ports"]["3000/tcp"][0]
        base = "http://127.0.0.1:" + binding["HostPort"]
        for _ in range(120):
            try:
                with urllib.request.urlopen(base + "/api/v1/version", timeout=2) as response:
                    if response.status == 200:
                        break
            except (OSError, ValueError):
                time.sleep(1)
        else:
            raise RuntimeError("disposable Forgejo startup timed out")
        env = dict(os.environ)
        env["FIXTURE_PASSWORD"] = secrets.token_urlsafe(32)
        tokens = {}
        for user in ["fixture-operator", "projection-bot"]:
            command(["docker", "exec", "--user", "git", "-e", "FIXTURE_PASSWORD", name,
                     "sh", "-c", 'forgejo admin user create --username "$1" --email "$1@example.test" --password "$FIXTURE_PASSWORD" --admin --must-change-password=false', "fixture", user], env=env)
            tokens[user] = command(["docker", "exec", "--user", "git", name, "forgejo", "admin", "user", "generate-access-token", "--username", user, "--token-name", "projection-test", "--scopes", "all", "--raw"])
        env.update(AGS_TEST_FORGEJO_URL=base, AGS_TEST_FORGEJO_BOT=tokens["projection-bot"], AGS_TEST_FORGEJO_OPERATOR=tokens["fixture-operator"])
        print("Disposable Forgejo ready; testing native protected-branch refusal and exact-head bot projection", flush=True)
        result = subprocess.run(["go", "test", "./internal/forgejointegration", "-run", "^TestRealForgejoProtectedPushIdentity$", "-v", "-count=1", "-timeout=3m"], env=env)
        return result.returncode
    finally:
        if started:
            command(["docker", "stop", "--time", "10", name])
            print("Stopped isolated fixture container: " + name, flush=True)


if __name__ == "__main__":
    raise SystemExit(main())
