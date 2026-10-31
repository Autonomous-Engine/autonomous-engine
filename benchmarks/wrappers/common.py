"""Runner contract, result model, logging and git helpers shared by both wrappers.

The runner (``scripts/code-implementation-benchmark.py``) executes each
competitor as a shell command inside a clean clone and exports:

* ``BENCH_REPO``         - the clone (also the cwd)
* ``BENCH_PROMPT_FILE``  - identical task prompt
* ``BENCH_OUTPUT_FILE``  - where the wrapper writes its normalized JSON report
* ``BENCH_TASK_ID``      - stable task id

This module adds the wrapper-level knobs shared by every competitor:

* ``BENCH_TIME_LIMIT_S`` - wall-clock cap enforced by the wrapper itself
                           (default 1800 s, the runner's own default)
* ``BENCH_MAX_TOKENS``   - token cap (prompt + completion) enforced for BOTH
                           competitors; a breach is a ``failed`` outcome
* ``BENCH_RAW_DIR``      - directory for raw logs. Must be OUTSIDE the clone,
                           because the runner counts every untracked file in
                           ``BENCH_REPO`` as an agent change.
"""

from __future__ import annotations

import dataclasses
import datetime as _dt
import json
import os
import platform
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any, Mapping

from . import WRAPPER_VERSION
from .schema import assert_valid, load_schema

# --- outcomes and exit codes -------------------------------------------------

OUTCOME_PASSED = "passed_agent"
OUTCOME_FAILED = "failed"
OUTCOME_BLOCKED = "blocked"
OUTCOME_TIMEOUT = "timeout"
OUTCOMES = (OUTCOME_PASSED, OUTCOME_FAILED, OUTCOME_BLOCKED, OUTCOME_TIMEOUT)

EXIT_OK = 0
EXIT_FAILED = 1
EXIT_BLOCKED = 2
EXIT_TIMEOUT = 3
EXIT_USAGE = 4

EXIT_FOR_OUTCOME = {
    OUTCOME_PASSED: EXIT_OK,
    OUTCOME_FAILED: EXIT_FAILED,
    OUTCOME_BLOCKED: EXIT_BLOCKED,
    OUTCOME_TIMEOUT: EXIT_TIMEOUT,
}

DEFAULT_TIME_LIMIT_S = 1800.0


class WrapperError(Exception):
    """Configuration / usage error. The wrapper exits with EXIT_USAGE."""


# --- environment -------------------------------------------------------------


def _require(environ: Mapping[str, str], name: str) -> str:
    value = environ.get(name, "").strip()
    if not value:
        raise WrapperError(f"{name} is required")
    return value


def float_env(environ: Mapping[str, str], name: str, default: float) -> float:
    raw = environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError as exc:
        raise WrapperError(f"{name} must be a number, got {raw!r}") from exc
    if value <= 0:
        raise WrapperError(f"{name} must be > 0, got {raw!r}")
    return value


def int_env(environ: Mapping[str, str], name: str, default: int | None) -> int | None:
    raw = environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise WrapperError(f"{name} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise WrapperError(f"{name} must be > 0, got {raw!r}")
    return value


def bool_env(environ: Mapping[str, str], name: str, default: bool) -> bool:
    raw = environ.get(name, "").strip().lower()
    if not raw:
        return default
    if raw in ("1", "true", "yes", "on"):
        return True
    if raw in ("0", "false", "no", "off"):
        return False
    raise WrapperError(f"{name} must be a boolean (1/0/true/false), got {raw!r}")


@dataclasses.dataclass
class BenchEnv:
    """The runner contract plus the shared wrapper knobs, validated."""

    competitor: str
    repo: Path
    prompt_file: Path
    output_file: Path
    task_id: str
    time_limit_s: float
    raw_dir: Path
    max_tokens: int | None = None

    @classmethod
    def from_environ(cls, competitor: str, environ: Mapping[str, str] | None = None) -> "BenchEnv":
        environ = os.environ if environ is None else environ
        repo = Path(_require(environ, "BENCH_REPO")).resolve()
        if not repo.is_dir():
            raise WrapperError(f"BENCH_REPO is not a directory: {repo}")
        if not (repo / ".git").exists():
            raise WrapperError(f"BENCH_REPO is not a git checkout: {repo}")
        prompt_file = Path(_require(environ, "BENCH_PROMPT_FILE")).resolve()
        if not prompt_file.is_file():
            raise WrapperError(f"BENCH_PROMPT_FILE does not exist: {prompt_file}")
        output_file = Path(_require(environ, "BENCH_OUTPUT_FILE")).resolve()
        task_id = _require(environ, "BENCH_TASK_ID")
        time_limit_s = float_env(environ, "BENCH_TIME_LIMIT_S", DEFAULT_TIME_LIMIT_S)
        max_tokens = int_env(environ, "BENCH_MAX_TOKENS", None)

        raw_root = environ.get("BENCH_RAW_DIR", "").strip()
        raw_root_path = Path(raw_root).resolve() if raw_root else Path(tempfile.gettempdir()) / "bench-wrappers"
        if _is_inside(raw_root_path, repo):
            raise WrapperError(
                f"BENCH_RAW_DIR ({raw_root_path}) must not be inside BENCH_REPO ({repo}); "
                "the runner counts every untracked file in the clone as an agent change"
            )
        stamp = _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        raw_dir = raw_root_path / competitor / _safe_name(task_id) / f"{stamp}-{os.getpid()}"
        raw_dir.mkdir(parents=True, exist_ok=True)

        return cls(
            competitor=competitor,
            repo=repo,
            prompt_file=prompt_file,
            output_file=output_file,
            task_id=task_id,
            time_limit_s=time_limit_s,
            raw_dir=raw_dir,
            max_tokens=max_tokens,
        )

    def read_prompt(self) -> str:
        prompt = self.prompt_file.read_text(encoding="utf-8")
        if not prompt.strip():
            raise WrapperError(f"BENCH_PROMPT_FILE is empty: {self.prompt_file}")
        return prompt


def echo_logs(environ: Mapping[str, str]) -> bool:
    """Wrapper log lines are echoed to stderr unless BENCH_QUIET=1 (the raw file is always written)."""
    return environ.get("BENCH_QUIET", "").strip() not in ("1", "true", "yes", "on")


def _safe_name(value: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "-_." else "_" for ch in value)[:80] or "task"


def _is_inside(path: Path, parent: Path) -> bool:
    try:
        path.relative_to(parent)
        return True
    except ValueError:
        return False


# --- deadline ----------------------------------------------------------------


class Deadline:
    """Monotonic wall-clock budget."""

    def __init__(self, seconds: float):
        self.seconds = float(seconds)
        self.started = time.monotonic()

    def elapsed(self) -> float:
        return time.monotonic() - self.started

    def remaining(self) -> float:
        return max(0.0, self.seconds - self.elapsed())

    def expired(self) -> bool:
        return self.remaining() <= 0.0


# --- raw log -----------------------------------------------------------------


class RawLog:
    """Append-only timestamped log; echoes to stderr so the runner captures it too."""

    def __init__(self, path: Path, echo: bool = True):
        self.path = path
        self.echo = echo
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = open(self.path, "a", encoding="utf-8")

    def log(self, message: str) -> None:
        stamp = _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"
        line = f"{stamp} {message}"
        self._fh.write(line + "\n")
        self._fh.flush()
        if self.echo:
            print(line, file=sys.stderr, flush=True)

    def close(self) -> None:
        try:
            self._fh.close()
        except OSError:
            pass


# --- result ------------------------------------------------------------------


@dataclasses.dataclass
class Result:
    """One normalized result; serialized to BENCH_OUTPUT_FILE."""

    competitor: str
    task_id: str
    outcome: str
    elapsed_s: float
    raw_ref: str
    head_sha: str | None = None
    model_calls: int | None = None
    tokens_in: int | None = None
    tokens_out: int | None = None
    cost_usd: float | None = None
    versions: dict[str, Any] = dataclasses.field(default_factory=dict)
    details: dict[str, Any] = dataclasses.field(default_factory=dict)

    @property
    def exit_code(self) -> int:
        return EXIT_FOR_OUTCOME[self.outcome]

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "competitor": self.competitor,
            "task_id": self.task_id,
            "outcome": self.outcome,
            "elapsed_s": round(float(self.elapsed_s), 3),
            "head_sha": self.head_sha,
            "model_calls": self.model_calls,
            "tokens_in": self.tokens_in,
            "tokens_out": self.tokens_out,
            "cost_usd": self.cost_usd,
            "versions": dict(self.versions),
            "raw_ref": self.raw_ref,
            "details": _jsonable(self.details),
        }


def _jsonable(value: Any) -> Any:
    """Coerce a nested structure to JSON-serializable primitives."""
    if isinstance(value, dict):
        return {str(k): _jsonable(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(v) for v in value]
    if isinstance(value, Path):
        return str(value)
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    return str(value)


def write_result(env: BenchEnv, result: Result, log: RawLog | None = None) -> dict[str, Any]:
    """Validate, write BENCH_OUTPUT_FILE, mirror into the raw dir, echo to stdout.

    The runner deletes BENCH_OUTPUT_FILE after the agent command returns, so the
    document is also copied to ``<raw_dir>/output.json`` and printed on stdout
    (which the runner keeps in ``agent.stdout``). The stdout mirror is part of
    the contract and is printed regardless of BENCH_QUIET.
    """
    doc = result.to_dict()
    errors = assert_valid(doc, load_schema(), raise_on_error=False)
    if errors:
        # A wrapper bug, not a competitor failure. Fail loudly rather than
        # emitting a document the aggregator cannot trust.
        raise WrapperError("normalized output failed schema validation: " + "; ".join(errors))
    text = json.dumps(doc, indent=2, sort_keys=True) + "\n"
    env.output_file.parent.mkdir(parents=True, exist_ok=True)
    env.output_file.write_text(text, encoding="utf-8")
    (env.raw_dir / "output.json").write_text(text, encoding="utf-8")
    print("BENCH_RESULT " + json.dumps(doc, sort_keys=True), flush=True)
    if log is not None:
        log.log(f"result: outcome={result.outcome} exit={result.exit_code} head_sha={result.head_sha} output={env.output_file}")
    return doc


def finalize(env: BenchEnv, result: Result, deadline: Deadline, log: RawLog, outcome: str, reason: str | None = None) -> int:
    """Shared end-of-run path for every driver: set outcome, record the reason,
    write the normalized document, close the log, return the wrapper exit code
    (always from EXIT_FOR_OUTCOME, never a competitor's raw status)."""
    result.outcome = outcome
    result.elapsed_s = deadline.elapsed()
    if reason:
        result.details["reason"] = reason
        log.log(f"{outcome}: {reason}")
    record_token_cap(env, result)
    result.details["exit_code"] = result.exit_code
    write_result(env, result, log)
    log.close()
    return result.exit_code


def tokens_total(result: Result) -> int | None:
    """Prompt + completion tokens, or None when either side is unknown."""
    if result.tokens_in is None or result.tokens_out is None:
        return None
    return int(result.tokens_in) + int(result.tokens_out)


def record_diff_vs_base(env: BenchEnv, result: Result, base_sha: str | None, log: RawLog, timeout: float | None = 120) -> None:
    """Fill details.changed_files / diff_stat / diff_base / diff_head from ``git diff <base>..HEAD``.

    Both wrappers leave a clean working tree (AE checks out the produced branch,
    the Claude wrapper commits), so the runner's own ``git status``/``git diff``
    come back empty; this is the equivalent record against the runner's pinned
    base revision. All four keys are always present (null when unknown).
    """
    head = git_head(env.repo)
    result.details["diff_base"] = base_sha
    result.details["diff_head"] = head
    result.details["changed_files"] = None
    result.details["diff_stat"] = None
    if not base_sha or not head:
        return
    try:
        names = git(env.repo, "diff", "--name-only", f"{base_sha}..HEAD", timeout=timeout)
        stat = git(env.repo, "diff", "--stat", f"{base_sha}..HEAD", timeout=timeout)
    except GitError as exc:
        log.log(f"diff vs base failed (non-fatal): {exc}")
        return
    result.details["changed_files"] = [line for line in names.splitlines() if line.strip()]
    result.details["diff_stat"] = stat


def record_token_cap(env: BenchEnv, result: Result) -> None:
    """Always emit details.token_cap = {limit, observed, verified} when BENCH_MAX_TOKENS is set."""
    if env.max_tokens is None:
        return
    observed = tokens_total(result)
    result.details["token_cap"] = {"limit": env.max_tokens, "observed": observed, "verified": observed is not None}


# --- git ---------------------------------------------------------------------


class GitError(RuntimeError):
    pass


def git(repo: Path, *args: str, check: bool = True, timeout: float | None = 300) -> str:
    """Run git in ``repo`` and return stripped stdout. Raises GitError on failure or timeout."""
    try:
        proc = subprocess.run(
            ["git", "-C", str(repo), *args],
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
            encoding="utf-8",
            errors="replace",
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git {' '.join(args)} exceeded {timeout}s") from exc
    if check and proc.returncode != 0:
        raise GitError(f"git {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip() or proc.stdout.strip()}")
    return proc.stdout.strip()


def git_head(repo: Path) -> str | None:
    try:
        return git(repo, "rev-parse", "HEAD")
    except GitError:
        return None


def is_ancestor(repo: Path, ancestor: str, descendant: str, timeout: float | None = 300) -> bool:
    """True when ``ancestor`` is reachable from ``descendant`` (git merge-base --is-ancestor)."""
    try:
        proc = subprocess.run(
            ["git", "-C", str(repo), "merge-base", "--is-ancestor", ancestor, descendant],
            capture_output=True,
            text=True,
            check=False,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return False
    return proc.returncode == 0


def git_version() -> str | None:
    try:
        return subprocess.run(["git", "--version"], text=True, capture_output=True, check=False).stdout.strip() or None
    except OSError:
        return None


# Identity used for commits the *wrapper* makes (never the competitor).
GIT_BENCH_IDENTITY = ["-c", "user.name=benchmark-wrapper", "-c", "user.email=benchmark-wrapper@localhost"]


# --- misc --------------------------------------------------------------------


def base_versions(extra: Mapping[str, Any] | None = None) -> dict[str, Any]:
    versions: dict[str, Any] = {
        "wrapper": WRAPPER_VERSION,
        "python": platform.python_version(),
        "platform": platform.platform(),
        "git": git_version(),
    }
    if extra:
        versions.update(extra)
    return versions


def relpath_inside(path: Path, repo: Path) -> str | None:
    """Return ``path`` relative to ``repo`` (posix style) or None if outside."""
    try:
        return path.resolve().relative_to(repo.resolve()).as_posix()
    except ValueError:
        return None
