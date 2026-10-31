"""Shared code for the code-implementation benchmark competitor wrappers.

Both ``benchmarks/run-autonomous-engine.py`` and ``benchmarks/run-claude.py``
import this package. It contains the runner env-var contract, the normalized
output schema and its validator, and the per-competitor drivers.

Standard library only (Python 3.13).
"""

WRAPPER_VERSION = "1"
