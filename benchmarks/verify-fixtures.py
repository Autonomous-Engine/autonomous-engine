#!/usr/bin/env python3
"""Verify the sealed benchmark fixtures under benchmarks/tasks/.

For every fixture directory the verifier:

1. validates fixture.json, prompt.md and the matching entry in tasks.json;
2. makes a *sealed* clone: an empty repository that fetches exactly the pinned
   base SHA (depth 1) from the public remote, so no later history (including the
   upstream fix) is reachable from the checkout;
3. copies hidden-tests/ into the clone and runs the fixture's test command,
   which must FAIL (non-zero exit);
4. renders the reference patch from upstream (see `reference_patch`), applies it
   and runs the test command again, which must PASS.

The corpus ships no upstream code. fixture.json's `reference` names the upstream
commit and paths of the fix, and the patch is rebuilt from a shallow fetch on
every run.

Blocked fixtures (expected_outcome == "blocked") have no hidden tests and no
reference patch. For them the verifier checks that structurally, and runs the
test command on the untouched clone, where it must pass (the command is the
"tree unchanged" rule the runner applies after the agent exits).

The script prints a table and exits non-zero if any fixture fails.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

BENCH_DIR = Path(__file__).resolve().parent
TASKS_DIR = BENCH_DIR / "tasks"
MANIFEST = BENCH_DIR / "tasks.json"
CORPUS_TARGETS = BENCH_DIR / "corpus-targets.json"
RUNNER_PATH = BENCH_DIR.parent / "scripts" / "code-implementation-benchmark.py"


def load_runner():
    """Import the benchmark runner so shared behaviour (time-limit export) has one source."""
    import importlib.util

    spec = importlib.util.spec_from_file_location("code_implementation_benchmark", RUNNER_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RUNNER = load_runner()


def canonical_repo_url(url: str) -> str:
    """Canonical form used in fixture.json/tasks.json: no surrounding whitespace, no trailing
    slash, no `.git` suffix. Case is preserved (GitHub paths are case-insensitive, but the
    fixture should spell the repository the way upstream does)."""
    url = url.strip()
    while url.endswith("/"):
        url = url[:-1]
    if url.endswith(".git"):
        url = url[: -len(".git")]
    return url


def repo_key(url: str) -> str:
    """Pairing key for the cross-fixture leak check: canonical form, case-folded."""
    return canonical_repo_url(url).casefold()


def same_repo(a: str, b: str) -> bool:
    return repo_key(a) == repo_key(b)

TASK_CLASSES = {
    "historical_defect",
    "small_feature",
    "cross_file_refactor",
    "dependency_or_config",
    "blocked_or_underspecified",
}
FAIL_MODES = {"assert", "build"}
DIFFICULTIES = {"easy", "medium", "hard"}
SIZE_STRATA = {"small", "medium", "large"}
PARTITIONS = {"development", "sealed"}
OUTCOMES = {"pass", "blocked"}
REQUIRED_FIELDS = (
    "id",
    "repo",
    "base_sha",
    "language",
    "task_class",
    "difficulty",
    "size_stratum",
    "test_command",
    "partition",
    "expected_outcome",
)
SHA_RE = re.compile(r"^[0-9a-f]{40}$")

# Corpus-level targets (D5 mix per language and overall) live in corpus-targets.json; see
# load_targets() for the schema and check_targets() for the semantics.
TARGET_KEYS = {
    "min_total",
    "min_per_class",
    "min_blocked",
    "min_sealed_fraction",
    "min_per_difficulty",
    "min_non_small",
}


class FixtureError(Exception):
    pass


def find_bash() -> str:
    """Locate the bash used for test commands.

    The runner executes test commands with ``bash -lc``. On Windows a bare ``bash`` often
    resolves to the WSL launcher stub in System32, so prefer the bash shipped with Git.
    Set BENCH_BASH to override.
    """
    override = os.environ.get("BENCH_BASH")
    if override:
        return override
    if os.name == "nt":
        git_exe = shutil.which("git")
        if git_exe:
            for parent in Path(git_exe).resolve().parents:
                for candidate in (parent / "bin" / "bash.exe", parent / "usr" / "bin" / "bash.exe"):
                    if candidate.is_file():
                        return str(candidate)
    return "bash"


BASH = find_bash()


def run(command: str, cwd: Path, env: dict[str, str], timeout: int) -> dict:
    """Run a shell command the same way the benchmark runner does (bash -lc)."""
    started = time.monotonic()
    try:
        proc = subprocess.run(
            [BASH, "-lc", command],
            cwd=cwd,
            env=env,
            text=True,
            encoding="utf-8",
            errors="replace",
            capture_output=True,
            timeout=timeout,
            check=False,
        )
        return {
            "exit_code": proc.returncode,
            "timed_out": False,
            "wall_seconds": round(time.monotonic() - started, 1),
            "stdout": proc.stdout,
            "stderr": proc.stderr,
        }
    except subprocess.TimeoutExpired as exc:
        return {
            "exit_code": None,
            "timed_out": True,
            "wall_seconds": round(time.monotonic() - started, 1),
            "stdout": exc.stdout or "",
            "stderr": exc.stderr or "",
        }


def git(repo: Path, *args: str, env: dict[str, str] | None = None) -> str:
    proc = subprocess.run(
        ["git", "-C", str(repo), *args],
        text=True,
        encoding="utf-8",
        errors="replace",
        capture_output=True,
        env=env,
        check=False,
    )
    if proc.returncode != 0:
        raise FixtureError(f"git {' '.join(args)} failed: {proc.stderr.strip() or proc.stdout.strip()}")
    return proc.stdout.strip()


def sealed_clone(repo_url: str, base_sha: str, dest: Path, env: dict[str, str]) -> None:
    """Fetch exactly base_sha into an empty repository (no other refs, depth 1)."""
    dest.mkdir(parents=True, exist_ok=True)
    git(dest, "init", "-q", env=env)
    git(dest, "config", "core.autocrlf", "false", env=env)
    git(dest, "config", "advice.detachedHead", "false", env=env)
    git(dest, "remote", "add", "origin", repo_url, env=env)
    git(dest, "fetch", "-q", "--depth", "1", "origin", base_sha, env=env)
    git(dest, "checkout", "-q", "-b", "bench", "FETCH_HEAD", env=env)
    # Close the easy path back to upstream: no remote stays configured in the clone.
    git(dest, "remote", "remove", "origin", env=env)
    head = git(dest, "rev-parse", "HEAD", env=env)
    if head != base_sha:
        raise FixtureError(f"checked out {head}, expected {base_sha}")
    reachable = git(dest, "rev-list", "--all", "--count", env=env)
    if reachable != "1":
        raise FixtureError(f"sealed clone has {reachable} reachable commits, expected 1")
    if git(dest, "remote", env=env):
        raise FixtureError("sealed clone still has a remote configured")


def load_json(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise FixtureError(f"missing {path.name}")
    except json.JSONDecodeError as exc:
        raise FixtureError(f"{path.name} is not valid JSON: {exc}")


def hidden_test_files(task_dir: Path) -> list[Path]:
    hidden = task_dir / "hidden-tests"
    if not hidden.is_dir():
        return []
    return sorted(p for p in hidden.rglob("*") if p.is_file())


def validate_fixture(task_dir: Path, manifest_by_id: dict[str, dict]) -> dict:
    fixture = load_json(task_dir / "fixture.json")
    missing = [f for f in REQUIRED_FIELDS if not fixture.get(f)]
    if missing:
        raise FixtureError(f"fixture.json missing fields: {', '.join(missing)}")
    if fixture["id"] != task_dir.name:
        raise FixtureError(f"fixture id {fixture['id']!r} does not match directory {task_dir.name!r}")
    if not SHA_RE.match(fixture["base_sha"]):
        raise FixtureError("base_sha must be a full 40-hex commit SHA")
    if not re.match(r"^https://", fixture["repo"]):
        raise FixtureError("repo must be a public https:// URL")
    if fixture["repo"] != canonical_repo_url(fixture["repo"]):
        raise FixtureError(
            f"repo URL {fixture['repo']!r} is not canonical; use {canonical_repo_url(fixture['repo'])!r} "
            "(no trailing slash, no .git suffix)"
        )
    if fixture["task_class"] not in TASK_CLASSES:
        raise FixtureError(f"unknown task_class {fixture['task_class']!r}")
    if fixture["difficulty"] not in DIFFICULTIES:
        raise FixtureError(f"unknown difficulty {fixture['difficulty']!r}")
    if fixture["size_stratum"] not in SIZE_STRATA:
        raise FixtureError(f"unknown size_stratum {fixture['size_stratum']!r}")
    if fixture["partition"] not in PARTITIONS:
        raise FixtureError(f"unknown partition {fixture['partition']!r}")
    if fixture["expected_outcome"] not in OUTCOMES:
        raise FixtureError(f"unknown expected_outcome {fixture['expected_outcome']!r}")
    if fixture.get("base_test_status", "pass") not in ("pass", "fail"):
        raise FixtureError("base_test_status must be 'pass' (default) or 'fail'")
    blocked = fixture["expected_outcome"] == "blocked"
    if blocked:
        if "fail_mode" in fixture:
            raise FixtureError("blocked fixture must not declare fail_mode")
    elif fixture.get("fail_mode") not in FAIL_MODES:
        raise FixtureError(f"fail_mode is required and must be one of {sorted(FAIL_MODES)}")
    exceptions = fixture.get("reverse_apply_exceptions", {})
    if not isinstance(exceptions, dict) or any(not isinstance(v, str) or not v.strip() for v in exceptions.values()):
        raise FixtureError("reverse_apply_exceptions must map other fixture ids to a non-empty reason")

    prompt_path = task_dir / "prompt.md"
    if not prompt_path.is_file() or not prompt_path.read_text(encoding="utf-8").strip():
        raise FixtureError("prompt.md is missing or empty")
    prompt = prompt_path.read_text(encoding="utf-8")

    hidden = hidden_test_files(task_dir)
    if (task_dir / "reference").exists():
        raise FixtureError("reference/ must not exist: the corpus ships no upstream code (declare reference in fixture.json)")

    if blocked:
        if fixture["task_class"] != "blocked_or_underspecified":
            raise FixtureError("expected_outcome=blocked requires task_class=blocked_or_underspecified")
        if hidden:
            raise FixtureError("blocked fixture must not ship hidden tests")
        if "reference" in fixture:
            raise FixtureError("blocked fixture must not declare a reference")
    else:
        if not hidden:
            raise FixtureError("hidden-tests/ has no files")
        validate_reference(fixture)

    # Leakage checks: the prompt must not name the hidden test files or their test functions.
    lowered = prompt.lower()
    for token in ("hidden-tests", "hidden_test", "reference patch", "fixture.json"):
        if token in lowered:
            raise FixtureError(f"prompt.md mentions {token!r} (leak)")
    for path in hidden:
        if path.name.lower() in lowered:
            raise FixtureError(f"prompt.md names hidden test file {path.name} (leak)")
        for func in re.findall(r"^func\s+(Test\w+)", path.read_text(encoding="utf-8"), re.M):
            if func.lower() in lowered:
                raise FixtureError(f"prompt.md names hidden test function {func} (leak)")

    # Manifest consistency.
    entry = manifest_by_id.get(fixture["id"])
    if entry is None:
        raise FixtureError("no entry in tasks.json")
    checks = {
        "repo": entry.get("repo"),
        "base_sha": entry.get("base_revision"),
        "test_command": entry.get("test_command"),
        "expected_outcome": entry.get("expected_outcome", "pass"),
    }
    for field, manifest_value in checks.items():
        if manifest_value != fixture[field]:
            raise FixtureError(f"tasks.json {field!r} ({manifest_value!r}) != fixture.json ({fixture[field]!r})")
    prompt_file = entry.get("prompt_file")
    if prompt_file is None or (BENCH_DIR / prompt_file).resolve() != prompt_path.resolve():
        raise FixtureError(f"tasks.json prompt_file must point at tasks/{task_dir.name}/prompt.md")
    if "prompt" in entry:
        raise FixtureError("tasks.json must reference prompt_file, not embed an inline prompt")
    hidden_dir = entry.get("hidden_tests_dir")
    if blocked:
        if hidden_dir or entry.get("hidden_test_patch"):
            raise FixtureError("blocked entry must not declare hidden tests in tasks.json")
    else:
        if hidden_dir is None or (BENCH_DIR / hidden_dir).resolve() != (task_dir / "hidden-tests").resolve():
            raise FixtureError(f"tasks.json hidden_tests_dir must point at tasks/{task_dir.name}/hidden-tests")
    for field in ("language", "task_class", "difficulty", "size_stratum", "partition"):
        if entry.get(field) != fixture[field]:
            raise FixtureError(f"tasks.json {field!r} ({entry.get(field)!r}) != fixture.json ({fixture[field]!r})")
    if entry.get("fail_mode") != fixture.get("fail_mode"):
        raise FixtureError(f"tasks.json fail_mode ({entry.get('fail_mode')!r}) != fixture.json ({fixture.get('fail_mode')!r})")

    fixture["_hidden"] = hidden
    fixture["_blocked"] = blocked
    return fixture


def classify_failure(result: dict) -> str:
    """Classify a failing test run: 'build' (hidden tests did not compile / toolchain could
    not run the package) or 'assert' (tests compiled and failed). Anything else is reported
    as-is and never matches a declared fail_mode."""
    text = (result["stdout"] + result["stderr"]).lower()
    if result["timed_out"]:
        return "timeout"
    if "build failed" in text or "[setup failed]" in text or "cannot find module" in text or "go.mod file not found" in text:
        return "build"
    if "--- fail" in text or "panic:" in text:
        return "assert"
    return f"exit={result['exit_code']}"


def validate_reference(fixture: dict) -> None:
    """A non-blocked fixture's `reference`: {"steps": [{"commit", "paths", "drop_hunks_containing"?}]}."""
    ref = fixture.get("reference")
    if not isinstance(ref, dict) or not isinstance(ref.get("steps"), list) or not ref["steps"]:
        raise FixtureError("fixture.json needs reference.steps (upstream commit and paths of the fix)")
    for step in ref["steps"]:
        if not isinstance(step, dict) or not re.fullmatch(r"[0-9a-f]{40}", str(step.get("commit", ""))):
            raise FixtureError("each reference step needs a 40-hex upstream commit")
        paths = step.get("paths")
        if not isinstance(paths, list) or not paths or not all(isinstance(p, str) and p for p in paths):
            raise FixtureError("each reference step needs a non-empty list of paths")
        drop = step.get("drop_hunks_containing", [])
        if not isinstance(drop, list) or not all(isinstance(d, str) and d for d in drop):
            raise FixtureError("drop_hunks_containing must be a list of non-empty strings")


def drop_hunks(diff: str, needles: list[str]) -> str:
    """Remove every hunk whose text contains one of the needles (and a file left with none)."""
    if not needles:
        return diff
    out = []
    for part in re.split(r"(?m)^(?=diff --git )", diff):
        if not part:
            continue
        pieces = re.split(r"(?m)^(?=@@ )", part)
        kept = [h for h in pieces[1:] if not any(n in h for n in needles)]
        if pieces[1:] and not kept:
            continue
        out.append(pieces[0] + "".join(kept))
    return "".join(out)


_PATCH_CACHE: dict[str, bytes] = {}


def reference_patch(fixture: dict, work: Path, env: dict[str, str]) -> bytes:
    """Render the fixture's reference patch from upstream.

    Each step is `git diff --binary -M <base_sha> <commit> -- <paths>`, minus any hunk
    that contains a `drop_hunks_containing` string. Both commits are fetched at depth 1
    into a scratch repository, never into the sealed clone. A fixture that already
    carries `_patch` (bytes) uses it as is.
    """
    if "_patch" in fixture:
        return fixture["_patch"]
    if fixture["id"] in _PATCH_CACHE:
        return _PATCH_CACHE[fixture["id"]]
    validate_reference(fixture)
    up = work / "_upstream" / re.sub(r"[^A-Za-z0-9._-]+", "_", repo_key(fixture["repo"]))
    if not (up / ".git").is_dir():
        up.mkdir(parents=True, exist_ok=True)
        git(up, "init", "-q", env=env)
        git(up, "remote", "add", "origin", canonical_repo_url(fixture["repo"]), env=env)
    base = fixture["base_sha"]
    parts = []
    for step in fixture["reference"]["steps"]:
        git(up, "fetch", "-q", "--depth", "1", "origin", base, step["commit"], env=env)
        # Raw output: git() strips, which would cut a trailing context line.
        proc = subprocess.run(
            ["git", "-C", str(up), "diff", "--binary", "-M", base, step["commit"], "--", *step["paths"]],
            capture_output=True, env=env, check=False,
        )
        if proc.returncode != 0:
            raise FixtureError(f"git diff for reference step {step['commit'][:12]} failed: {proc.stderr.decode(errors='replace').strip()}")
        diff = proc.stdout.decode("utf-8").replace("\r\n", "\n")
        if not diff.strip():
            raise FixtureError(f"reference step {step['commit'][:12]} renders an empty diff for {step['paths']}")
        parts.append(drop_hunks(diff, step.get("drop_hunks_containing", [])))
    patch = "".join(parts).encode("utf-8")
    _PATCH_CACHE[fixture["id"]] = patch
    return patch


def patched_paths(patch_text: str) -> list[str]:
    paths: list[str] = []
    for line in patch_text.splitlines():
        for prefix in ("--- a/", "+++ b/"):
            if line.startswith(prefix):
                path = line[len(prefix):].strip()
                if path and path not in paths:
                    paths.append(path)
    return paths


def cross_fixture_leak_check(fixture: dict, others: list[dict], clone: Path, work: Path, env: dict[str, str]) -> str:
    """Fail if another fixture's reference patch applies cleanly in reverse to this base,
    which means this base already contains that fixture's answer.

    The check runs per patched file with `-C0` (match on the changed lines only): a later
    base on the same repository usually drifts the *context* around the fix (imports,
    neighbouring functions), so a full-context reverse apply would report "does not apply"
    while the fix itself is present byte for byte. `-C0` per file is a strict superset of
    the full-context check.
    """
    exceptions = fixture.get("reverse_apply_exceptions", {})
    notes = []
    for other in others:
        if other["id"] == fixture["id"] or not same_repo(other.get("repo", ""), fixture["repo"]):
            continue
        if "_patch" not in other and ("reference" not in other or other.get("expected_outcome") == "blocked"):
            continue
        patch_text = reference_patch(other, work, env).replace(b"\r\n", b"\n")
        patch = work / f"{fixture['id']}.reverse-check.{other['id']}.patch"
        patch.write_bytes(patch_text)
        leaked = []
        for path in patched_paths(patch_text.decode("utf-8", errors="replace")):
            proc = subprocess.run(
                [
                    "git", "-C", str(clone), "apply", "--reverse", "--check", "-C0",
                    f"--include={path}", "--whitespace=nowarn", str(patch.resolve()),
                ],
                text=True,
                encoding="utf-8",
                errors="replace",
                capture_output=True,
                env=env,
                check=False,
            )
            if proc.returncode == 0:
                leaked.append(path)
        if leaked:
            if other["id"] in exceptions:
                notes.append(f"excepted:{other['id']}({','.join(leaked)})")
                continue
            raise FixtureError(
                f"cross-fixture leak: {other['id']}'s reference patch reverses cleanly on this base for "
                f"{', '.join(leaked)}, i.e. base {fixture['base_sha'][:12]} already contains that fixture's fix"
            )
    unused = set(exceptions) - {o["id"] for o in others}
    if unused:
        raise FixtureError(f"reverse_apply_exceptions names unknown fixtures: {', '.join(sorted(unused))}")
    return ",".join(notes) if notes else "clean"


def tail(text: str, lines: int = 25) -> str:
    rows = text.strip().splitlines()
    return "\n".join(rows[-lines:])


def verify_one(task_dir: Path, fixture: dict, others: list[dict], work: Path, timeout: int, verbose: bool) -> dict:
    env = os.environ.copy()
    env.update(
        {
            "GIT_TERMINAL_PROMPT": "0",
            "BENCH_TASK_ID": fixture["id"],
            "BENCH_BASE_REVISION": fixture["base_sha"],
            "BENCH_TIME_LIMIT_S": str(RUNNER.time_limit_seconds(timeout)),
        }
    )
    clone = work / fixture["id"]
    started = time.monotonic()
    sealed_clone(fixture["repo"], fixture["base_sha"], clone, env)
    env["BENCH_REPO"] = str(clone)

    row = {"baseline": "-", "before": "-", "after": "-", "fail_mode": "-"}
    row["xleak"] = cross_fixture_leak_check(fixture, others, clone, work, env)

    if fixture["_blocked"]:
        result = run(fixture["test_command"], clone, env, timeout)
        if result["exit_code"] != 0:
            raise FixtureError(
                "blocked fixture's test command must pass on the untouched clone; "
                f"exit={result['exit_code']}\n{tail(result['stdout'] + result['stderr'])}"
            )
        row["baseline"] = "rule-ok"
        row["before"] = "n/a"
        row["after"] = "n/a"
        row["fail_mode"] = "blocked-rule"
        row["seconds"] = round(time.monotonic() - started, 1)
        return row

    # Baseline sanity: the repository's own test command on the untouched base checkout.
    # A harness problem (missing toolchain, broken shell) or a flaky upstream suite would
    # otherwise masquerade as a "discriminating" failure once the hidden tests are added.
    baseline = run(fixture["test_command"], clone, env, timeout)
    if verbose:
        print(f"--- {fixture['id']} baseline (exit={baseline['exit_code']})\n{tail(baseline['stdout'] + baseline['stderr'], 60)}")
    expect_base_pass = fixture.get("base_test_status", "pass") == "pass"
    if expect_base_pass and baseline["exit_code"] != 0:
        raise FixtureError(
            f"the repository's own tests fail at the base revision (exit={baseline['exit_code']}); "
            f"fix the environment or set base_test_status\n{tail(baseline['stdout'] + baseline['stderr'], 40)}"
        )
    if not expect_base_pass and baseline["exit_code"] == 0:
        raise FixtureError("fixture declares base_test_status=fail but the base test command passed")
    row["baseline"] = "PASS" if baseline["exit_code"] == 0 else "FAIL(declared)"

    # Hidden test paths must not already exist at the base revision; copying them in
    # must add evaluator files, never overwrite agent-visible ones.
    hidden_root = task_dir / "hidden-tests"
    for path in fixture["_hidden"]:
        rel = path.relative_to(hidden_root)
        if (clone / rel).exists():
            raise FixtureError(f"hidden test {rel} already exists in the base checkout")
    shutil.copytree(hidden_root, clone, dirs_exist_ok=True)

    before = run(fixture["test_command"], clone, env, timeout)
    if verbose:
        print(f"--- {fixture['id']} before (exit={before['exit_code']})\n{tail(before['stdout'] + before['stderr'], 60)}")
    if before["exit_code"] == 0:
        raise FixtureError("hidden tests PASSED at the base revision (fixture is not discriminating)")
    row["before"] = "FAIL(ok)"
    observed = classify_failure(before)
    declared = fixture["fail_mode"]
    if observed != declared:
        raise FixtureError(
            f"fail_mode mismatch: fixture.json declares {declared!r} but the base run failed as {observed!r}\n"
            f"{tail(before['stdout'] + before['stderr'], 30)}"
        )
    row["fail_mode"] = f"{observed}(ok)"

    patch = work / f"{fixture['id']}.reference.patch"
    patch.write_bytes(reference_patch(fixture, work, env))
    git(clone, "apply", "--whitespace=nowarn", str(patch.resolve()), env=env)

    after = run(fixture["test_command"], clone, env, timeout)
    if verbose:
        print(f"--- {fixture['id']} after (exit={after['exit_code']})\n{tail(after['stdout'] + after['stderr'], 60)}")
    if after["exit_code"] != 0:
        raise FixtureError(
            f"hidden tests still fail after the reference patch (exit={after['exit_code']})\n"
            f"{tail(after['stdout'] + after['stderr'], 40)}"
        )
    row["after"] = "PASS(ok)"
    row["seconds"] = round(time.monotonic() - started, 1)
    return row


def load_targets(path: Path = CORPUS_TARGETS) -> dict:
    """Read and validate corpus-targets.json.

    Schema: {"languages": {<language>: <targets>}, "overall": <targets>} where <targets> has
    any of: min_total (int), min_per_class ({task_class: int}), min_blocked (int, fixtures with
    expected_outcome == "blocked"), min_sealed_fraction (0..1, share of partition == "sealed"),
    min_per_difficulty ({difficulty: int}), min_non_small (int, size_stratum medium or large).
    Keys starting with "$" are comments.
    """
    data = load_json(path)

    def check_block(name: str, block: object) -> None:
        if not isinstance(block, dict):
            raise FixtureError(f"{path.name}: {name} must be an object")
        for key, value in block.items():
            if key.startswith("$"):
                continue
            if key not in TARGET_KEYS:
                raise FixtureError(f"{path.name}: {name} has unknown key {key!r}")
            if key in ("min_per_class", "min_per_difficulty"):
                allowed = TASK_CLASSES if key == "min_per_class" else DIFFICULTIES
                if not isinstance(value, dict):
                    raise FixtureError(f"{path.name}: {name}.{key} must be an object")
                for sub, n in value.items():
                    if sub not in allowed:
                        raise FixtureError(f"{path.name}: {name}.{key} has unknown value {sub!r}")
                    if not isinstance(n, int) or isinstance(n, bool) or n < 0:
                        raise FixtureError(f"{path.name}: {name}.{key}.{sub} must be a non-negative integer")
            elif key == "min_sealed_fraction":
                if not isinstance(value, (int, float)) or isinstance(value, bool) or not 0 <= value <= 1:
                    raise FixtureError(f"{path.name}: {name}.{key} must be a number between 0 and 1")
            elif not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise FixtureError(f"{path.name}: {name}.{key} must be a non-negative integer")

    languages = data.get("languages")
    if not isinstance(languages, dict) or not languages:
        raise FixtureError(f"{path.name}: 'languages' must be a non-empty object")
    for lang, block in languages.items():
        check_block(f"languages.{lang}", block)
    check_block("overall", data.get("overall", {}))
    return data


def _target_problems(label: str, fixtures: list[dict], targets: dict) -> list[str]:
    problems = []
    total = len(fixtures)
    if total < targets.get("min_total", 0):
        problems.append(f"{label} total: {total} (minimum {targets['min_total']})")
    classes: dict[str, int] = {}
    difficulties: dict[str, int] = {}
    for f in fixtures:
        classes[f.get("task_class", "?")] = classes.get(f.get("task_class", "?"), 0) + 1
        difficulties[f.get("difficulty", "?")] = difficulties.get(f.get("difficulty", "?"), 0) + 1
    for cls, lo in targets.get("min_per_class", {}).items():
        if classes.get(cls, 0) < lo:
            problems.append(f"{label} {cls}: {classes.get(cls, 0)} (minimum {lo})")
    for diff, lo in targets.get("min_per_difficulty", {}).items():
        if difficulties.get(diff, 0) < lo:
            problems.append(f"{label} difficulty {diff}: {difficulties.get(diff, 0)} (minimum {lo})")
    blocked = sum(1 for f in fixtures if f.get("expected_outcome") == "blocked")
    if blocked < targets.get("min_blocked", 0):
        problems.append(f"{label} blocked: {blocked} (minimum {targets['min_blocked']})")
    non_small = sum(1 for f in fixtures if f.get("size_stratum") in ("medium", "large"))
    if non_small < targets.get("min_non_small", 0):
        problems.append(f"{label} medium/large size: {non_small} (minimum {targets['min_non_small']})")
    if "min_sealed_fraction" in targets:
        sealed = sum(1 for f in fixtures if f.get("partition") == "sealed")
        fraction = sealed / total if total else 0.0
        if fraction < targets["min_sealed_fraction"]:
            problems.append(
                f"{label} sealed fraction: {sealed}/{total} = {fraction:.2f} (minimum {targets['min_sealed_fraction']})"
            )
    return problems


def check_targets(fixtures: list[dict], targets: dict) -> list[str]:
    """Check the whole corpus (never only the --only subset) against corpus-targets.json:
    every language present must have targets, every language with targets must meet them,
    and the corpus as a whole must meet the "overall" targets."""
    problems = []
    unknown = {f.get("task_class", "?") for f in fixtures} - TASK_CLASSES
    if unknown:
        problems.append(f"unknown classes: {', '.join(sorted(unknown))}")
    by_lang: dict[str, list[dict]] = {}
    for f in fixtures:
        by_lang.setdefault(f.get("language", "?"), []).append(f)
    languages = targets.get("languages", {})
    untargeted = set(by_lang) - set(languages)
    if untargeted:
        problems.append(
            f"no targets in {CORPUS_TARGETS.name} for language(s): {', '.join(sorted(untargeted))}"
        )
    for lang in sorted(languages):
        problems.extend(_target_problems(lang, by_lang.get(lang, []), languages[lang]))
    problems.extend(_target_problems("overall", fixtures, targets.get("overall", {})))
    return problems


def corpus_summary(fixtures: list[dict]) -> str:
    by_lang: dict[str, int] = {}
    for f in fixtures:
        by_lang[f.get("language", "?")] = by_lang.get(f.get("language", "?"), 0) + 1
    sealed = sum(1 for f in fixtures if f.get("partition") == "sealed")
    langs = ", ".join(f"{lang} {n}" for lang, n in sorted(by_lang.items()))
    return f"{len(fixtures)} fixtures ({langs}); sealed {sealed}/{len(fixtures)}"


def load_corpus() -> list[dict]:
    """Read every fixture.json under tasks/ (raw, for corpus-wide checks)."""
    corpus = []
    for task_dir in sorted(p for p in TASKS_DIR.iterdir() if p.is_dir()):
        try:
            data = load_json(task_dir / "fixture.json")
        except FixtureError as exc:
            raise FixtureError(f"{task_dir.name}: {exc}")
        data["_dir"] = task_dir
        data.setdefault("id", task_dir.name)
        corpus.append(data)
    return corpus


def print_table(rows: list[dict]) -> None:
    headers = ["id", "class", "difficulty", "lang", "base", "xleak", "baseline", "before", "after", "fail-mode", "result", "secs"]
    table = [headers]
    for r in rows:
        table.append(
            [
                r["id"],
                r["class"],
                r["difficulty"],
                r["lang"],
                r["base"][:12],
                r.get("xleak", "-"),
                r.get("baseline", "-"),
                r.get("before", "-"),
                r.get("after", "-"),
                r.get("fail_mode", "-"),
                r["result"],
                str(r.get("seconds", "-")),
            ]
        )
    widths = [max(len(row[i]) for row in table) for i in range(len(headers))]
    for idx, row in enumerate(table):
        print("  ".join(cell.ljust(widths[i]) for i, cell in enumerate(row)))
        if idx == 0:
            print("  ".join("-" * w for w in widths))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--only", action="append", default=[], help="verify only this task id (repeatable)")
    parser.add_argument("--work-dir", help="directory for sealed clones (default: a fresh temp dir)")
    parser.add_argument("--keep", action="store_true", help="keep the sealed clones after the run")
    parser.add_argument("--timeout", type=int, default=900, help="per-command timeout in seconds")
    parser.add_argument("--verbose", action="store_true", help="print test output for every phase")
    args = parser.parse_args()

    if not TASKS_DIR.is_dir():
        print(f"no tasks directory at {TASKS_DIR}", file=sys.stderr)
        return 2
    manifest = load_json(MANIFEST)
    manifest_by_id = {t["id"]: t for t in manifest.get("tasks", [])}

    try:
        corpus = load_corpus()
    except FixtureError as exc:
        print(str(exc), file=sys.stderr)
        return 2

    task_dirs = sorted(p for p in TASKS_DIR.iterdir() if p.is_dir())
    if args.only:
        task_dirs = [p for p in task_dirs if p.name in set(args.only)]
        unknown = set(args.only) - {p.name for p in task_dirs}
        if unknown:
            print(f"unknown task ids: {', '.join(sorted(unknown))}", file=sys.stderr)
            return 2

    work = Path(args.work_dir) if args.work_dir else Path(tempfile.mkdtemp(prefix="ae-bench-verify-"))
    work.mkdir(parents=True, exist_ok=True)
    print(f"verifying {len(task_dirs)} fixture(s); sealed clones under {work}; shell: {BASH}")

    rows: list[dict] = []
    failures = 0
    for task_dir in task_dirs:
        row = {"id": task_dir.name, "class": "?", "difficulty": "?", "lang": "?", "base": "?", "result": "?"}
        try:
            fixture = validate_fixture(task_dir, manifest_by_id)
            row.update(
                {
                    "class": fixture["task_class"],
                    "difficulty": fixture["difficulty"],
                    "lang": fixture["language"],
                    "base": fixture["base_sha"],
                }
            )
            row.update(verify_one(task_dir, fixture, corpus, work, args.timeout, args.verbose))
            row["result"] = "OK"
        except FixtureError as exc:
            failures += 1
            row["result"] = "FAIL"
            print(f"\n[{task_dir.name}] {exc}\n", file=sys.stderr)
        rows.append(row)

    print()
    print_table(rows)

    manifest_only = set(manifest_by_id) - {p.name for p in sorted(TASKS_DIR.iterdir()) if p.is_dir()}
    if manifest_only:
        failures += 1
        print(f"\ntasks.json lists ids without a fixture directory: {', '.join(sorted(manifest_only))}", file=sys.stderr)

    if args.only:
        # The targets are a property of the whole corpus; a subset run cannot meet them and
        # must not pretend to have checked them.
        print(f"\ncorpus targets: SKIPPED (--only given; run without --only to check {CORPUS_TARGETS.name})")
    else:
        try:
            problems = check_targets(corpus, load_targets())
        except FixtureError as exc:
            problems = [str(exc)]
        if problems:
            failures += 1
            print(f"\ncorpus does not meet {CORPUS_TARGETS.name}:\n  " + "\n  ".join(problems), file=sys.stderr)
        else:
            print(f"\ncorpus targets: OK ({corpus_summary(corpus)})")

    if args.keep:
        print(f"sealed clones kept at {work}")
    else:
        shutil.rmtree(work, ignore_errors=True)

    print(f"\n{len(rows) - failures}/{len(rows)} fixtures verified" + (", FAILURES PRESENT" if failures else ""))
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
