#!/usr/bin/env python3
"""Run reproducible, command-based code implementation comparisons."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path


DEFAULT_TIME_LIMIT_MARGIN_S = 60
MIN_TIME_LIMIT_S = 30


def find_bash() -> str:
    """Shell used for competitor and test commands (``bash -lc``).

    On Windows a bare ``bash`` usually resolves to the WSL launcher stub in System32, so
    prefer the bash shipped with Git. Set BENCH_BASH to override on any platform.
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
    started = time.monotonic()
    try:
        proc = subprocess.run(
            [BASH, "-lc", command],
            cwd=cwd,
            env=env,
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
        return {
            "exit_code": proc.returncode,
            "timed_out": False,
            "wall_seconds": round(time.monotonic() - started, 3),
            "stdout": proc.stdout,
            "stderr": proc.stderr,
        }
    except subprocess.TimeoutExpired as exc:
        return {
            "exit_code": None,
            "timed_out": True,
            "wall_seconds": round(time.monotonic() - started, 3),
            "stdout": exc.stdout or "",
            "stderr": exc.stderr or "",
        }


def git(repo: Path, *args: str) -> str:
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()


def git_lines(repo: Path, *args: str) -> list[str]:
    """Like git() but preserves leading whitespace on each line, which `git status
    --porcelain` uses for the status columns (" M file" = modified in the working tree)."""
    output = subprocess.check_output(["git", "-C", str(repo), *args], text=True)
    return [line.rstrip("\r\n") for line in output.splitlines() if line.strip()]


def is_remote_repo(repo: str) -> bool:
    return "://" in repo or repo.startswith("git@")


def manifest_path(value: str, manifest_dir: Path | None) -> Path:
    """Resolve a manifest-relative path (new-style fields) against the manifest directory."""
    path = Path(value)
    if path.is_absolute() or manifest_dir is None:
        return path.resolve()
    return (manifest_dir / path).resolve()


def checkout_base(task: dict, worktree: Path) -> None:
    if is_remote_repo(task["repo"]):
        # Sealed checkout: fetch exactly the pinned revision so that no later history
        # (for example the upstream fix of a historical defect) is reachable from the clone,
        # then drop the remote so the clone holds no path back to upstream.
        env = os.environ.copy()
        env["GIT_TERMINAL_PROMPT"] = "0"  # never hang on a credential prompt

        def sealed_git(*args: str) -> str:
            return subprocess.check_output(["git", "-C", str(worktree), *args], text=True, env=env).strip()

        worktree.mkdir(parents=True, exist_ok=True)
        sealed_git("init", "--quiet")
        sealed_git("config", "core.autocrlf", "false")
        sealed_git("remote", "add", "origin", task["repo"])
        sealed_git("fetch", "--quiet", "--depth", "1", "origin", task["base_revision"])
        sealed_git("checkout", "--quiet", "-b", "bench", "FETCH_HEAD")
        sealed_git("remote", "remove", "origin")
        head = sealed_git("rev-parse", "HEAD")
        if head != task["base_revision"]:
            raise RuntimeError(f"{task['id']}: checked out {head}, expected base_revision {task['base_revision']}")
        reachable = sealed_git("rev-list", "--all", "--count")
        if reachable != "1":
            raise RuntimeError(f"{task['id']}: sealed clone has {reachable} reachable commits, expected 1")
        if sealed_git("remote"):
            raise RuntimeError(f"{task['id']}: sealed clone still has a remote configured")
        return
    source = Path(task["repo"]).resolve()
    subprocess.run(["git", "clone", "--quiet", "--no-hardlinks", str(source), str(worktree)], check=True)
    subprocess.run(
        ["git", "-C", str(worktree), "checkout", "--quiet", task["base_revision"]], check=True
    )


def time_limit_seconds(timeout: int, margin: int = DEFAULT_TIME_LIMIT_MARGIN_S) -> int:
    """Wall-time budget exported to the wrapper: the runner timeout minus a margin so the
    wrapper can cancel its work item and write BENCH_OUTPUT_FILE before it is killed."""
    return max(MIN_TIME_LIMIT_S, int(timeout) - int(margin))


def read_agent_output(path: Path) -> tuple[dict | None, str | None]:
    """Parse the wrapper's normalized JSON document. Returns (document, error)."""
    if not path.exists():
        return None, "missing"
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        return None, f"invalid JSON: {exc}"
    if not isinstance(document, dict):
        return None, "not a JSON object"
    return document, None


def tracked_tree_unchanged(worktree: Path, base_revision: str) -> bool:
    """True when no tracked file differs from the pinned base revision: neither in HEAD
    (wrappers may commit), nor in the index, nor in the working tree."""
    for args in (
        ["diff", "--quiet", base_revision, "HEAD"],
        ["diff", "--cached", "--quiet"],
        ["diff", "--quiet", base_revision],
    ):
        proc = subprocess.run(["git", "-C", str(worktree), *args], capture_output=True, check=False)
        if proc.returncode != 0:
            return False
    return True


EXPECTED_OUTCOMES = ("pass", "blocked")

# Manifest strata copied into every result record so aggregation does not need a join.
STRATA_FIELDS = (
    "language",
    "task_class",
    "difficulty",
    "size_stratum",
    "partition",
    "fail_mode",
    "expected_outcome",
)

# Normalized wrapper fields (benchmarks/wrappers/output_schema.json) flattened to the top level
# of each result record. null means unknown, never zero.
FLATTENED_OUTPUT_FIELDS = (
    "tokens_in",
    "tokens_out",
    "cost_usd",
    "model_calls",
    "elapsed_s",
    "head_sha",
    "versions",
)

REQUIRED_TASK_FIELDS = ("id", "repo", "base_revision", "test_command")


def prompt_problem(task: dict) -> str | None:
    """An inline `prompt` takes precedence over `prompt_file` (as in benchmark_task) and must be
    a non-empty string; an empty inline prompt is rejected rather than sent to the agents."""
    if "prompt" in task:
        prompt = task["prompt"]
        if not isinstance(prompt, str) or not prompt.strip():
            return "inline 'prompt' is empty"
        return None
    if not task.get("prompt_file"):
        return "needs 'prompt' or 'prompt_file'"
    return None


def validate_manifest(manifest: object, manifest_dir: Path | None = None) -> list[str]:
    """Every problem in the manifest, checked before any task runs so a bad entry cannot
    abort a run halfway. File paths (`prompt_file`, `hidden_tests_dir`) are resolved like
    benchmark_task resolves them and must exist. Returns a list of messages; empty means valid."""
    if not isinstance(manifest, dict) or not isinstance(manifest.get("tasks"), list):
        return ["manifest must be an object with a 'tasks' list"]
    errors: list[str] = []
    seen: set[str] = set()
    for index, task in enumerate(manifest["tasks"]):
        if not isinstance(task, dict):
            errors.append(f"tasks[{index}]: not an object")
            continue
        label = task.get("id") or f"tasks[{index}]"
        for field in REQUIRED_TASK_FIELDS:
            if not task.get(field):
                errors.append(f"{label}: missing required field {field!r}")
        problem = prompt_problem(task)
        if problem:
            errors.append(f"{label}: {problem}")
        elif "prompt" not in task and not manifest_path(task["prompt_file"], manifest_dir).is_file():
            errors.append(f"{label}: prompt_file {task['prompt_file']!r} does not exist")
        hidden_dir = task.get("hidden_tests_dir")
        if hidden_dir and not manifest_path(hidden_dir, manifest_dir).is_dir():
            errors.append(f"{label}: hidden_tests_dir {hidden_dir!r} does not exist")
        expected = task.get("expected_outcome", "pass")
        if expected not in EXPECTED_OUTCOMES:
            errors.append(f"{label}: unrecognized expected_outcome {expected!r}; expected one of {EXPECTED_OUTCOMES}")
        task_id = task.get("id")
        if task_id:
            if task_id in seen:
                errors.append(f"{label}: duplicate task id")
            seen.add(task_id)
    return errors


def is_false_success(expected: str, wrapper_outcome: str | None, tests_exit_code: int | None) -> bool:
    """D7 zero-tolerance signal: the wrapper CLAIMED success (outcome passed_agent) when it was
    wrong. pass task: the hidden tests did not pass. blocked task: any claim of success, since
    the correct outcome is a blocker."""
    if wrapper_outcome != "passed_agent":
        return False
    if expected == "blocked":
        return True
    return tests_exit_code != 0


def is_unsafe_edit_on_blocked(expected: str, wrapper_outcome: str | None, tree_unchanged: bool | None) -> bool:
    """Safety signal kept apart from false success: on a blocked task the tracked tree changed
    versus the base without a claim of success (an honest failed/timeout that left edits)."""
    return expected == "blocked" and wrapper_outcome != "passed_agent" and tree_unchanged is False


def jsonl_path(output: Path) -> Path:
    """Incremental results file next to the final JSON: run-001.json -> run-001.jsonl."""
    candidate = output.with_suffix(".jsonl")
    if candidate == output:
        candidate = output.with_name(output.name + ".jsonl")
    return candidate


def append_jsonl(path: Path, record: dict) -> None:
    """Append one finished record. The file is opened in append mode, so records from an
    earlier (possibly crashed) run with the same --output are kept; each carries its run_id."""
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(record) + "\n")
        fh.flush()


def changes_vs_base(worktree: Path, base_revision: str) -> tuple[list[str], str]:
    """Base-relative change evidence. Wrappers commit or check out a produced commit, so
    HEAD-relative `git status`/`git diff` always look clean; compare against the pinned base.

    Returns (changed_files, diff_stat): committed changes as `git diff --name-status
    <base>..HEAD` lines, followed by every `git status --porcelain` line (uncommitted
    tracked modifications and untracked files), and the `git diff --stat <base>..HEAD` text.
    """
    committed = git_lines(worktree, "diff", "--name-status", f"{base_revision}..HEAD")
    uncommitted = git_lines(worktree, "status", "--porcelain")
    stat = git(worktree, "diff", "--stat", f"{base_revision}..HEAD")
    return committed + uncommitted, stat


def benchmark_task(
    task: dict,
    competitor: str,
    command: str,
    root: Path,
    timeout: int,
    manifest_dir: Path | None = None,
    time_limit_margin: int = DEFAULT_TIME_LIMIT_MARGIN_S,
    trial: int = 0,
    run_id: str | None = None,
) -> dict:
    expected = task.get("expected_outcome", "pass")
    if expected not in EXPECTED_OUTCOMES:
        # Fail closed: an unknown value must not silently fall back to the pass rule.
        raise ValueError(f"{task.get('id', '?')}: unrecognized expected_outcome {expected!r}; expected one of {EXPECTED_OUTCOMES}")
    problem = prompt_problem(task)
    if problem:
        raise ValueError(f"{task.get('id', '?')}: {problem}")

    # Trial 0 keeps the historical layout; later trials get their own clean clone.
    worktree = root / competitor / (task["id"] if trial == 0 else f"{task['id']}.trial{trial}")
    worktree.parent.mkdir(parents=True, exist_ok=True)
    checkout_base(task, worktree)

    if "prompt" in task:
        prompt = task["prompt"]
    else:
        prompt = manifest_path(task["prompt_file"], manifest_dir).read_text(encoding="utf-8")

    prompt_file = worktree / ".benchmark-prompt.txt"
    output_file = worktree / ".benchmark-agent-output.txt"
    prompt_file.write_text(prompt, encoding="utf-8")
    time_limit = time_limit_seconds(timeout, time_limit_margin)
    env = os.environ.copy()
    env.update(
        {
            "BENCH_REPO": str(worktree),
            "BENCH_PROMPT_FILE": str(prompt_file),
            "BENCH_OUTPUT_FILE": str(output_file),
            "BENCH_TASK_ID": task["id"],
            "BENCH_BASE_REVISION": task["base_revision"],
            "BENCH_TIME_LIMIT_S": str(time_limit),
        }
    )

    agent = run(command, worktree, env, timeout)
    agent_output, agent_output_error = read_agent_output(output_file)
    prompt_file.unlink(missing_ok=True)
    output_file.unlink(missing_ok=True)
    changed_files = git_lines(worktree, "status", "--porcelain")
    diff = git(worktree, "diff", "--stat")
    changed_files_vs_base, diff_stat_vs_base = changes_vs_base(worktree, task["base_revision"])

    hidden_patch = task.get("hidden_test_patch")
    patch_result = None
    if hidden_patch:
        patch_path = Path(hidden_patch).resolve()
        patch_result = run(f'git apply -- "{patch_path}"', worktree, env, timeout)

    # Held-out test files are copied in only after the agent has exited.
    hidden_files: list[str] = []
    hidden_dir = task.get("hidden_tests_dir")
    if hidden_dir:
        source_dir = manifest_path(hidden_dir, manifest_dir)
        hidden_files = sorted(str(p.relative_to(source_dir)) for p in source_dir.rglob("*") if p.is_file())
        shutil.copytree(source_dir, worktree, dirs_exist_ok=True)

    tests = run(task["test_command"], worktree, env, timeout)
    wrapper_outcome = agent_output.get("outcome") if agent_output else None
    tree_unchanged = None
    if expected == "blocked":
        # Correct outcome: the wrapper's normalized document says the agent reported a
        # blocker, no tracked file differs from the pinned base revision, and the fixture's
        # rule command holds. A non-zero exit on its own (token cap, wrapper exception,
        # timeout) never counts as a blocker.
        tree_unchanged = tracked_tree_unchanged(worktree, task["base_revision"])
        passed = wrapper_outcome == "blocked" and tree_unchanged and tests["exit_code"] == 0
    else:
        passed = agent["exit_code"] == 0 and tests["exit_code"] == 0
    strata = {field: task.get(field) for field in STRATA_FIELDS}
    strata["expected_outcome"] = expected
    flattened = {field: (agent_output.get(field) if agent_output else None) for field in FLATTENED_OUTPUT_FIELDS}
    return {
        "run_id": run_id,
        "task_id": task["id"],
        "competitor": competitor,
        "trial": trial,
        **strata,
        **flattened,
        "base_revision": task["base_revision"],
        "time_limit_s": time_limit,
        "agent": agent,
        "agent_output": agent_output,
        "agent_output_error": agent_output_error,
        "wrapper_outcome": wrapper_outcome,
        "hidden_patch": patch_result,
        "hidden_files": hidden_files,
        "tests": tests,
        "tracked_tree_unchanged": tree_unchanged,
        "passed": passed,
        "false_success": is_false_success(expected, wrapper_outcome, tests["exit_code"]),
        "unsafe_edit_on_blocked": is_unsafe_edit_on_blocked(expected, wrapper_outcome, tree_unchanged),
        # HEAD-relative (kept for backward compatibility; wrappers usually leave these empty).
        "changed_file_count": len(changed_files),
        "changed_files": changed_files,
        "diff_stat": diff,
        # Base-relative: what the competitor actually changed since the pinned revision.
        "changed_file_count_vs_base": len(changed_files_vs_base),
        "changed_files_vs_base": changed_files_vs_base,
        "diff_stat_vs_base": diff_stat_vs_base,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run each manifest task against a candidate and a baseline competitor in "
        "sealed clones, score with held-out tests, and write per-trial result records.",
        epilog="Exit status: 0 only when every result passed, 1 otherwise, 2 for an invalid "
        "manifest. The exit status is a data-collection signal, NOT a paired-comparison gate "
        "(a blocked task a competitor cannot win also yields 1). Gate on "
        "scripts/benchmark-report.py instead.",
    )
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--candidate-command", required=True)
    parser.add_argument("--baseline-command", required=True)
    parser.add_argument("--candidate-name", default="autonomous-engine")
    parser.add_argument("--baseline-name", default="claude-cli")
    parser.add_argument(
        "--output",
        default="benchmark-results/results.json",
        help="final JSON document; every record is also appended, as soon as it finishes, to "
        "the same path with a .jsonl suffix",
    )
    parser.add_argument("--timeout", type=int, default=1800)
    parser.add_argument(
        "--time-limit-margin",
        type=int,
        default=DEFAULT_TIME_LIMIT_MARGIN_S,
        help="BENCH_TIME_LIMIT_S = --timeout minus this margin (floor %d s)" % MIN_TIME_LIMIT_S,
    )
    parser.add_argument("--trials", type=int, default=1, help="runs per task per competitor (default 1)")
    parser.add_argument(
        "--comparison",
        choices=("primary", "ablation", "rollout"),
        default="primary",
        help="run metadata for the report: primary = full Autonomous Engine versus Claude CLI; "
        "ablation = the baseline is Autonomous Engine without Codex/orchestration (same model); "
        "rollout = M4-06 shadow evaluation, candidate configuration snapshot versus the production "
        "default (both Autonomous Engine)",
    )
    parser.add_argument("--keep-worktrees", action="store_true")
    args = parser.parse_args(argv)
    if args.trials < 1:
        parser.error("--trials must be at least 1")
    if args.candidate_name == args.baseline_name:
        parser.error("--candidate-name and --baseline-name must differ")

    manifest_file = Path(args.manifest).resolve()
    manifest = json.loads(manifest_file.read_text(encoding="utf-8"))
    manifest_dir = manifest_file.parent
    problems = validate_manifest(manifest, manifest_dir)
    if problems:
        for problem in problems:
            print(f"manifest error: {problem}", file=sys.stderr)
        return 2
    run_id = str(uuid.uuid4())
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    incremental = jsonl_path(output)
    roles = {args.candidate_name: "candidate", args.baseline_name: "baseline"}
    temp = Path(tempfile.mkdtemp(prefix="code-implementation-benchmark-"))
    results = []
    try:
        competitors = (
            (args.candidate_name, args.candidate_command),
            (args.baseline_name, args.baseline_command),
        )
        for index, task in enumerate(manifest["tasks"]):
            for trial in range(args.trials):
                # Alternate execution order per (task index + trial) to reduce warm-cache/order bias.
                ordered = competitors if (index + trial) % 2 == 0 else tuple(reversed(competitors))
                for name, command in ordered:
                    record = benchmark_task(
                        task, name, command, temp, args.timeout, manifest_dir, args.time_limit_margin,
                        trial=trial, run_id=run_id,
                    )
                    record["role"] = roles[name]
                    record["comparison"] = args.comparison
                    append_jsonl(incremental, record)
                    results.append(record)
    finally:
        if args.keep_worktrees:
            print(f"worktrees retained at {temp}")
        else:
            shutil.rmtree(temp, ignore_errors=True)

    payload = {
        "manifest": str(manifest_file),
        "run_id": run_id,
        "trials": args.trials,
        "comparison": args.comparison,
        "competitors": {"candidate": args.candidate_name, "baseline": args.baseline_name},
        "results": results,
    }
    output.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(payload, indent=2))
    return 0 if all(result["passed"] for result in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
