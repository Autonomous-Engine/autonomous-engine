#!/usr/bin/env python3
"""Claude CLI competitor wrapper entry point. See benchmarks/wrappers/README.md."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from wrappers.claude_cli import main  # noqa: E402

if __name__ == "__main__":
    raise SystemExit(main())
