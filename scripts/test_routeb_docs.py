#!/usr/bin/env python3
"""Red/green checks for the Route B preparation manuals."""

from __future__ import annotations

import os
import pathlib
import re
import subprocess
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
DOCS = [
    ROOT / "docs/operations/routeb-baseline.md",
    ROOT / "docs/operations/routeb-runbook.md",
    ROOT / "docs/operations/routeb-runbook-mini.md",
    ROOT / "docs/operations/routeb-runbook-imile-win.md",
    ROOT / "docs/operations/routeb-external-dependencies.md",
]

CURRENT_REVISION = "3a0c7e4f0ac5150d774efc66168907671061bcc5"
ROLLBACK_REVISION = "0013aa744c8c90637ec54562ffc248a1bf7c9a52"
HISTORICAL_REHEARSAL = "c980bb63"

# A production manager command is rejected when it is written as something an
# operator can copy and run. Mentioning the command name inside a prohibition
# is allowed.
EXECUTABLE_MANAGER = re.compile(
    r"(?m)^(?!\s*#)(?!.*\b(?:do not|never|not a|forbidden|refuse|refuses|must not)\b).*"
    r"\b(?:launchctl|systemctl)\s+(?:disable|bootout|enable|bootstrap|stop|start|mask|unmask)\b"
)


class RouteBDocsTest(unittest.TestCase):
    def test_manuals_exist(self) -> None:
        missing = [str(path.relative_to(ROOT)) for path in DOCS if not path.is_file()]
        self.assertEqual(missing, [])

    def test_current_target_is_exact_revision(self) -> None:
        for path in DOCS:
            text = path.read_text(encoding="utf-8")
            self.assertIn(CURRENT_REVISION, text, path.name)
            self.assertNotRegex(
                text,
                re.compile(r"candidate(?: revision)?(?: remains| is|:)\s*`?" + HISTORICAL_REHEARSAL),
                path.name,
            )

    def test_historical_rehearsal_is_not_the_rollback_target(self) -> None:
        joined = "\n".join(path.read_text(encoding="utf-8") for path in DOCS)
        self.assertIn(ROLLBACK_REVISION, joined)
        self.assertIn("no down migration", joined)
        self.assertIn("lifecycle_generation", joined)
        self.assertIn("forgejo_reopen_pending", joined)
        self.assertIn("forgejo_lifecycle_at", joined)
        self.assertNotRegex(joined, re.compile(r"rollback target(?: remains| is|:)\s*`?" + HISTORICAL_REHEARSAL))

    def test_no_executable_production_manager_commands(self) -> None:
        for path in DOCS:
            text = path.read_text(encoding="utf-8")
            self.assertIsNone(EXECUTABLE_MANAGER.search(text), path.name)
            self.assertNotIn("sudo systemctl", text, path.name)

    def test_readyz_is_not_external_health(self) -> None:
        joined = "\n".join(path.read_text(encoding="utf-8") for path in DOCS)
        self.assertIn("diagnostic_only", joined)
        self.assertIn("not a webhook proof", joined.lower())

    def test_source_contracts_are_not_called_production_adapters(self) -> None:
        joined = "\n".join(path.read_text(encoding="utf-8") for path in DOCS)
        self.assertIn("native_adapter_unavailable", joined)
        self.assertIn("backup_contract_unimplemented", joined)
        self.assertNotRegex(
            joined,
            re.compile(r"upgrade_adapter_(?:launchd|systemd)\.py`? (?:is|provides) (?:the |a )?production"),
        )


def fenced_block(text: str) -> str:
    match = re.search(r"```sh\n(.*?)```", text, re.S)
    if match is None:
        raise AssertionError("missing sh block")
    return match.group(1)


class RouteBBuildCommandTest(unittest.TestCase):
    def test_documented_commands_bind_checkout_head(self) -> None:
        head = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, check=True, text=True, capture_output=True,
        ).stdout.strip()
        tree = subprocess.run(
            ["git", "rev-parse", "HEAD^{tree}"], cwd=ROOT, check=True, text=True, capture_output=True,
        ).stdout.strip()
        build = (ROOT / "docs/operations/routeb-build.md").read_text(encoding="utf-8")
        mini = (ROOT / "docs/operations/routeb-runbook-mini.md").read_text(encoding="utf-8")
        linux = (ROOT / "docs/operations/routeb-linux-build-input.md").read_text(encoding="utf-8")
        baseline = "3a0c7e4f0ac5150d774efc66168907671061bcc5"
        self.assertIn("version=source-<built revision>", mini)
        self.assertIn("tree=<built tree>", mini)
        self.assertIn("a6fc9f08d5ca4c8d8fe4860dc8e551263455426f", mini)
        self.assertNotIn(f"GIT_SHA={baseline}", fenced_block(build))
        self.assertNotIn(f"GIT_SHA={baseline}", fenced_block(linux))
        env = os.environ.copy()
        env.pop("GIT_SHA", None)
        env.pop("GIT_TREE", None)
        env["REVIEWED_HEAD"] = head

        def run_block(text: str) -> subprocess.CompletedProcess[str]:
            script = fenced_block(text)
            script = "\n".join(
                line for line in script.splitlines()
                if not line.startswith(("git fetch", "git checkout", "test \"$(git status"))
            ) + "\n"
            needle = "GIT_SHA=\"$GIT_SHA\" GIT_TREE=\"$GIT_TREE\" \\\n"
            if needle not in script:
                raise AssertionError("documented command does not pass GIT_SHA and GIT_TREE")
            script = script.replace(needle, "", 1)
            script = script.replace(
                "scripts/build-exact-source.sh binary",
                'python3 -c "import os; print(\'seen=%s tree=%s\' % (os.environ[\'GIT_SHA\'], os.environ[\'GIT_TREE\']))"; true',
            )
            script = script.replace(
                'mkdir -p "$HOME/ags-routeb-linux-candidate"\n',
                "mkdir() { :; }\n",
            )
            wrapper = 'set -eu\ncd "$1"\n' + script
            return subprocess.run(
                ["/bin/sh", "-c", wrapper, "probe", str(ROOT)],
                check=False, text=True, capture_output=True, env=env,
            )

        for label, page in (("build", build), ("linux", linux)):
            completed = run_block(page)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertIn(f"seen={head} tree={tree}", completed.stdout, label)



if __name__ == "__main__":
    unittest.main()
