#!/usr/bin/env python3
"""Collect the license text of every third-party component into third_party/licenses/.

    python scripts/collect_license_texts.py

Run it after regenerating the inventories in third_party/ (see
scripts/third_party_notices.py), then run `third_party_notices.py --write`.
It needs the Go module cache (`go mod download`) and installed node_modules in
dashboard/ and portal/. Components it cannot resolve are printed; add their
text by hand (for example from the upstream repository's LICENSE file).
"""

from __future__ import annotations

import csv
import json
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import third_party_notices as tpn  # noqa: E402

ROOT = tpn.ROOT
OUT = tpn.TEXTS
LICENSE_NAMES = re.compile(r"^(LICEN[CS]E|COPYING|NOTICE)([.-].*)?$", re.IGNORECASE)


def _license_files(d: Path) -> list[Path]:
    if not d.is_dir():
        return []
    return sorted(p for p in d.iterdir() if p.is_file() and LICENSE_NAMES.match(p.name))


def _join(files: list[Path]) -> str:
    parts = []
    for f in files:
        parts.append(f"==> {f.name} <==\n{f.read_text(encoding='utf-8', errors='replace').strip()}\n")
    return "\n".join(parts)


def _go_text(mod: str, url: str) -> str:
    out = subprocess.run(["go", "list", "-f", "{{.Module.Dir}}|{{.Dir}}", mod], cwd=ROOT, capture_output=True, text=True)
    if out.returncode != 0:
        out = subprocess.run(["go", "list", "-m", "-f", "{{.Dir}}|{{.Dir}}", mod], cwd=ROOT, capture_output=True, text=True)
    if out.returncode != 0:
        return ""
    mod_dir, pkg_dir = (Path(p) for p in out.stdout.strip().split("|"))
    m = re.search(r"/blob/[^/]+/(.+)$", url) or re.search(r":([^:]+)$", url)
    files = []
    if m:
        f = mod_dir / m.group(1).replace("\\", "/")
        if f.exists():
            files.append(f)
    for f in _license_files(pkg_dir) + _license_files(mod_dir):
        if f not in files:
            files.append(f)
    return _join(files)


def _npm_text(pkg: str) -> str:
    name = pkg.rsplit("@", 1)[0]
    for app in ("dashboard", "portal"):
        files = _license_files(ROOT / app / "node_modules" / name)
        if files:
            return _join(files)
    return ""


def main() -> int:
    go_urls = {}
    with (ROOT / "third_party" / "go-licenses.csv").open(encoding="utf-8", newline="") as f:
        for row in csv.reader(f):
            if row:
                go_urls[row[0]] = row[1]
    missing = []
    for kind, name in tpn.components():
        if kind == "go":
            text = _go_text(name, go_urls.get(name, ""))
        else:
            text = _npm_text(name)
        target = OUT / tpn.text_path(kind, name)
        if not text:
            if not target.exists():
                missing.append(f"{kind} {name}")
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text, encoding="utf-8", newline="\n")
    for m in missing:
        print(f"unresolved: {m}", file=sys.stderr)
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main())
