#!/usr/bin/env python3
"""Unit tests for scripts/third_party_notices.py (offline, standard library only).

    python -m unittest scripts/test_third_party_notices.py
"""

from __future__ import annotations

import importlib.util
import shutil
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
_spec = importlib.util.spec_from_file_location("third_party_notices", HERE / "third_party_notices.py")
tpn = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(tpn)


def _root_copy() -> Path:
    """A temporary root holding a copy of third_party/ (inventories and texts)."""
    d = Path(tempfile.mkdtemp())
    shutil.copytree(ROOT / "third_party", d / "third_party")
    return d


class ThirdPartyNoticesTest(unittest.TestCase):
    def test_missing_license_text_fails(self):
        empty = Path(tempfile.mkdtemp())
        problems = tpn.problems(root=ROOT, texts_dir=empty)
        self.assertTrue(any("no license text" in p and "go/" in p for p in problems), problems[:5])
        self.assertTrue(any("no license text" in p and "npm/" in p for p in problems), problems[:5])

    def test_every_component_has_license_text(self):
        self.assertEqual([p for p in tpn.problems(root=ROOT) if "no license text" in p], [])

    def test_disallowed_go_license_fails(self):
        root = _root_copy()
        csv_path = root / "third_party" / "go-licenses.csv"
        rows = csv_path.read_text(encoding="utf-8").splitlines()
        mod = rows[0].split(",")[0]
        rows[0] = ",".join(rows[0].split(",")[:2] + ["GPL-3.0"])
        csv_path.write_text("\n".join(rows) + "\n", encoding="utf-8")
        problems = tpn.problems(root=root)
        self.assertTrue(any(mod in p and "GPL-3.0" in p for p in problems), problems)

    def test_no_license_texts_for_benchmark_upstreams(self):
        self.assertFalse((ROOT / "third_party" / "licenses" / "benchmarks").exists())
        self.assertNotIn("fix.patch", tpn.render(root=ROOT))

    def test_committed_file_is_current(self):
        self.assertEqual(tpn.problems(root=ROOT), [])
        self.assertEqual((ROOT / "THIRD_PARTY_NOTICES.md").read_text(encoding="utf-8").replace("\r\n", "\n"), tpn.render(root=ROOT))


if __name__ == "__main__":
    unittest.main()
