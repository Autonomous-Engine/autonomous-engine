#!/usr/bin/env sh
# Shim for scripts/code-implementation-benchmark.py --baseline-command.
# Execs the Python wrapper next to this file; all configuration is via env vars
# (see benchmarks/wrappers/README.md). Override the interpreter with BENCH_PYTHON.
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
py="${BENCH_PYTHON:-}"
if [ -z "$py" ]; then
  if command -v python3 >/dev/null 2>&1 && python3 -c 'import sys' >/dev/null 2>&1; then
    py=python3
  else
    py=python
  fi
fi
exec "$py" "$here/run-claude.py" "$@"
