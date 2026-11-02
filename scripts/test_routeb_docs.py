#!/usr/bin/env python3
"""Red/green checks for the Route B preparation manuals."""

from __future__ import annotations

import pathlib
import re
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


if __name__ == "__main__":
    unittest.main()
