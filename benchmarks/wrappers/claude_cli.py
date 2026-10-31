"""Claude CLI competitor wrapper (verified against Claude Code 2.1.267).

Runs ``claude`` non-interactively in ``BENCH_REPO`` with the prompt from
``BENCH_PROMPT_FILE`` on stdin, restricted to the permissions Autonomous
Engine's implementation agent gets (see README.md "Equal-permission policy"),
then commits whatever the agent changed so the result has a ``head_sha`` -
Autonomous Engine's system commits on behalf of its agent, so the wrapper does
the same for Claude. Claude itself is denied git write commands.

Flags (all present in ``claude --help`` for 2.1.267):

* ``-p`` / ``--print``                 non-interactive
* ``--output-format json``             one JSON result document on stdout
* ``--permission-mode dontAsk``        nothing that would prompt is allowed
* ``--permission-prompts none``        and nobody is asked
* ``--tools``                          shrink the built-in tool set
* ``--allowedTools``                   pre-approve exactly that set
* ``--disallowedTools``                deny git publish ops (push/remote/
                                       anything that leaves the repo) and the
                                       destructive shell patterns AE rejects
* ``--strict-mcp-config``              no MCP servers (AE has none)
* ``--setting-sources project``        ignore user/local settings and hooks;
                                       the repo's own .claude/ still applies
* ``--disable-slash-commands``         no skills (AE has none)
* ``--no-session-persistence``         do not litter the operator's sessions
* ``--model``, ``--max-budget-usd``, ``--effort``  from BENCH_MODEL,
                                       BENCH_MAX_BUDGET_USD, BENCH_EFFORT

Budgets: ``BENCH_MAX_TOKENS`` (shared with the AE wrapper) is checked against
the ``usage`` block of Claude's JSON result after the run - the CLI has no
token-ceiling flag (verified: only ``--max-budget-usd`` and ``--autocompact``
exist in 2.1.267), so it cannot be cut short mid-run. ``BENCH_MAX_BUDGET_USD``
is Claude-only and is refused unless ``BENCH_MAX_TOKENS`` is also set, so no
run is ever capped only on the Claude side.

Exit code: the wrapper's own code space (0 passed_agent, 1 failed, 2 blocked,
3 timeout, 4 usage error) - Claude's raw status is recorded in
``details.claude_exit_code`` and is never returned verbatim, because raw 2/3
would collide with blocked/timeout.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from typing import Any, Mapping

from . import common
from .common import (
    EXIT_USAGE,
    OUTCOME_FAILED,
    OUTCOME_PASSED,
    OUTCOME_TIMEOUT,
    BenchEnv,
    Deadline,
    GitError,
    RawLog,
    Result,
    WrapperError,
    git,
)

COMPETITOR = "claude-cli"

# Built-in tools Claude may use. Mirrors AE's implementation-agent surface:
# read_file/list_files/search_code -> Read/Glob/Grep, write_file/edit_file ->
# Write/Edit, run_command (sh -c) -> Bash.
ALLOWED_TOOLS: tuple[str, ...] = ("Read", "Edit", "Write", "Glob", "Grep", "Bash")

# Denied even though Bash is allowed. Prefix-matched by Claude's rule engine.
# 1) git PUBLISH operations only - anything that leaves the clone. AE's
#    executor (internal/llm/tools/executor.go dangerousCommandReason) does not
#    block local git commands; its "no git" is a prompt instruction, so local
#    git (commit/checkout/reset/...) stays allowed here for parity. Force-push
#    to main is covered by the blanket push deny.
# 2) The destructive patterns AE's executor rejects, as far as prefix rules
#    can express them (chained forms such as `cd x && sudo ...` are NOT caught
#    by prefix rules; AE's regexes do catch them - disclosed in the README).
DISALLOWED_TOOLS: tuple[str, ...] = (
    "Bash(git push*)",
    "Bash(git remote*)",
    "Bash(git send-email*)",
    "Bash(git request-pull*)",
    "Bash(git svn*)",
    "Bash(sudo *)",
    "Bash(su *)",
    "Bash(doas *)",
    "Bash(mkfs*)",
    "Bash(dd *)",
    "Bash(chmod -R 777*)",
    "Bash(rm -rf /*)",
    "Bash(rm -rf ~*)",
    "Bash(rm -fr /*)",
    "Bash(rm -fr ~*)",
    "WebFetch",
    "WebSearch",
    "Agent",
    "Task",
)

SETTING_SOURCES = "project"


def claude_binary(environ: Mapping[str, str]) -> str:
    name = environ.get("BENCH_CLAUDE_BIN", "").strip() or "claude"
    path = shutil.which(name, path=environ.get("PATH"))
    if not path:
        raise WrapperError(f"claude CLI not found on PATH (looked for {name!r}); set BENCH_CLAUDE_BIN to override")
    return path


def build_command(binary: str, environ: Mapping[str, str]) -> list[str]:
    """The exact argv. The prompt is passed on stdin, never as an argument."""
    cmd = [
        binary,
        "-p",
        "--output-format",
        "json",
        "--permission-mode",
        "dontAsk",
        "--permission-prompts",
        "none",
        "--tools",
        ",".join(ALLOWED_TOOLS),
        "--allowedTools",
        *ALLOWED_TOOLS,
        "--disallowedTools",
        *DISALLOWED_TOOLS,
        "--strict-mcp-config",
        "--setting-sources",
        SETTING_SOURCES,
        "--disable-slash-commands",
        "--no-session-persistence",
    ]
    model = environ.get("BENCH_MODEL", "").strip()
    if model:
        cmd += ["--model", model]
    budget = environ.get("BENCH_MAX_BUDGET_USD", "").strip()
    if budget:
        try:
            if float(budget) <= 0:
                raise ValueError
        except ValueError as exc:
            raise WrapperError(f"BENCH_MAX_BUDGET_USD must be a positive number, got {budget!r}") from exc
        if common.int_env(environ, "BENCH_MAX_TOKENS", None) is None:
            raise WrapperError(
                "BENCH_MAX_BUDGET_USD is a Claude-only kill switch (the Autonomous Engine wrapper cannot "
                "observe billed USD); refusing to run without BENCH_MAX_TOKENS, the cap both competitors share"
            )
        cmd += ["--max-budget-usd", budget]
    effort = environ.get("BENCH_EFFORT", "").strip()
    if effort:
        cmd += ["--effort", effort]
    return cmd


def claude_version(binary: str, environ: Mapping[str, str]) -> str | None:
    try:
        proc = subprocess.run([binary, "--version"], capture_output=True, text=True, timeout=60, check=False, env=dict(environ))
    except (OSError, subprocess.TimeoutExpired):
        return None
    out = (proc.stdout or "").strip() or (proc.stderr or "").strip()
    return out or None


def _kill_tree(proc: subprocess.Popen[Any]) -> None:
    if proc.poll() is not None:
        return
    if os.name == "nt":
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(proc.pid)], capture_output=True, check=False)
    else:
        import signal

        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
    try:
        proc.kill()
    except OSError:
        pass


def parse_result_json(stdout: str) -> dict[str, Any] | None:
    """Return the ``type: result`` document from Claude's stdout, if any."""
    text = stdout.strip()
    if not text:
        return None
    try:
        doc = json.loads(text)
    except json.JSONDecodeError:
        doc = None
        # Tolerate stray lines before/after the document.
        for line in reversed(text.splitlines()):
            line = line.strip()
            if line.startswith("{"):
                try:
                    doc = json.loads(line)
                    break
                except json.JSONDecodeError:
                    continue
    if isinstance(doc, list):
        docs = [d for d in doc if isinstance(d, dict) and d.get("type") == "result"]
        doc = docs[-1] if docs else None
    return doc if isinstance(doc, dict) else None


def extract_metrics(doc: dict[str, Any]) -> dict[str, Any]:
    usage = doc.get("usage") if isinstance(doc.get("usage"), dict) else {}
    tokens_in = None
    tokens_out = None
    if usage:
        parts = [usage.get("input_tokens"), usage.get("cache_creation_input_tokens"), usage.get("cache_read_input_tokens")]
        nums = [int(p) for p in parts if isinstance(p, (int, float)) and not isinstance(p, bool)]
        tokens_in = sum(nums) if nums else None
        if isinstance(usage.get("output_tokens"), (int, float)):
            tokens_out = int(usage["output_tokens"])
    # num_turns counts assistant turns, i.e. API round trips (a run with one
    # tool call reports num_turns=2). usage.iterations is coarser (observed 1
    # for that same run) and is deliberately not used.
    num_turns = doc.get("num_turns")
    model_calls = int(num_turns) if isinstance(num_turns, int) and not isinstance(num_turns, bool) else None
    cost = doc.get("total_cost_usd")
    cost_usd = float(cost) if isinstance(cost, (int, float)) and not isinstance(cost, bool) else None
    model_usage = doc.get("modelUsage") if isinstance(doc.get("modelUsage"), dict) else {}
    return {
        "tokens_in": tokens_in,
        "tokens_out": tokens_out,
        "model_calls": model_calls,
        "cost_usd": cost_usd,
        "models": sorted(model_usage.keys()),
        "usage": usage,
        "is_error": bool(doc.get("is_error", False)),
        "subtype": doc.get("subtype"),
        "permission_denials": doc.get("permission_denials") if isinstance(doc.get("permission_denials"), list) else [],
        "num_turns": doc.get("num_turns"),
        "session_id": doc.get("session_id"),
        "duration_ms": doc.get("duration_ms"),
    }


def commit_working_tree(env: BenchEnv, log: RawLog) -> tuple[str | None, list[str]]:
    """Stage and commit everything Claude changed (except the runner's own files)."""
    excludes = []
    for path in (env.prompt_file, env.output_file):
        rel = common.relpath_inside(path, env.repo)
        if rel:
            excludes.append(f":(exclude){rel}")
    changed = [line for line in git(env.repo, "status", "--porcelain", "--", ".", *excludes).splitlines() if line.strip()]
    if not changed:
        log.log("no uncommitted working-tree changes (Claude may have committed itself)")
        return common.git_head(env.repo), []
    git(env.repo, "add", "-A", "--", ".", *excludes)
    git(env.repo, *common.GIT_BENCH_IDENTITY, "commit", "--quiet", "-m", f"benchmark: claude-cli output for {env.task_id}")
    head = git(env.repo, "rev-parse", "HEAD")
    log.log(f"committed {len(changed)} changed path(s) as {head}")
    return head, changed


def run(environ: Mapping[str, str] | None = None) -> int:
    environ = os.environ if environ is None else environ
    env = BenchEnv.from_environ(COMPETITOR, environ)
    binary = claude_binary(environ)
    command = build_command(binary, environ)
    deadline = Deadline(env.time_limit_s)
    log = RawLog(env.raw_dir / "wrapper.log", echo=common.echo_logs(environ))
    stdout_path = env.raw_dir / "claude-result.json"
    stderr_path = env.raw_dir / "claude-stderr.log"
    details: dict[str, Any] = {
        "command": command,
        "raw_dir": str(env.raw_dir),
        "raw_files": {"stdout": str(stdout_path), "stderr": str(stderr_path)},
        "allowed_tools": list(ALLOWED_TOOLS),
        "disallowed_tools": list(DISALLOWED_TOOLS),
    }
    result = Result(competitor=COMPETITOR, task_id=env.task_id, outcome=OUTCOME_FAILED, elapsed_s=0.0, raw_ref=str(env.raw_dir / "wrapper.log"), details=details)
    base_sha: str | None = None

    def finish(outcome: str, reason: str | None = None) -> int:
        common.record_diff_vs_base(env, result, base_sha, log)
        return common.finalize(env, result, deadline, log, outcome, reason)

    try:
        log.log(f"claude-cli wrapper start task={env.task_id} repo={env.repo} time_limit_s={env.time_limit_s} max_tokens={env.max_tokens}")
        version = claude_version(binary, environ)
        result.versions = common.base_versions({"claude_cli": version, "claude_binary": binary, "model_requested": environ.get("BENCH_MODEL", "").strip() or None})
        log.log(f"claude --version: {version}")
        prompt = env.read_prompt()
        base_sha = common.git_head(env.repo)
        details["base_sha"] = base_sha
        details["max_tokens"] = env.max_tokens
        log.log("command: " + " ".join(_quote(c) for c in command))

        popen_kwargs: dict[str, Any] = {}
        if os.name != "nt":
            popen_kwargs["start_new_session"] = True
        child_env = dict(environ)  # passed through unchanged; nothing is injected
        with open(stderr_path, "w", encoding="utf-8") as stderr_fh:
            proc = subprocess.Popen(
                command,
                cwd=str(env.repo),
                env=child_env,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=stderr_fh,
                text=True,
                encoding="utf-8",
                errors="replace",
                **popen_kwargs,
            )
            timed_out = False
            try:
                stdout, _ = proc.communicate(input=prompt, timeout=max(0.1, deadline.remaining()))
            except subprocess.TimeoutExpired:
                timed_out = True
                _kill_tree(proc)
                try:
                    stdout, _ = proc.communicate(timeout=30)
                except subprocess.TimeoutExpired:
                    stdout = ""
        stdout_path.write_text(stdout or "", encoding="utf-8")
        details["claude_exit_code"] = None if timed_out else proc.returncode

        doc = parse_result_json(stdout or "")
        if doc is not None:
            metrics = extract_metrics(doc)
            result.tokens_in = metrics["tokens_in"]
            result.tokens_out = metrics["tokens_out"]
            result.model_calls = metrics["model_calls"]
            result.cost_usd = metrics["cost_usd"]
            result.versions["models"] = metrics["models"]
            details["claude"] = {k: metrics[k] for k in ("is_error", "subtype", "num_turns", "session_id", "duration_ms", "permission_denials")}
            details["claude"]["result_text"] = str(doc.get("result", ""))[:2000]
        else:
            details["claude"] = {"parse_error": "no JSON result document on stdout"}

        # Commit whatever the agent left, in every non-usage outcome, so head_sha
        # is meaningful even for partial work. Claude has already exited.
        try:
            head, changed = commit_working_tree(env, log)
            result.head_sha = head
            details["status_porcelain"] = changed
        except GitError as exc:
            details["commit_error"] = str(exc)
            log.log(f"commit of working tree failed: {exc}")
        details["base_is_ancestor"] = bool(base_sha and result.head_sha) and common.is_ancestor(env.repo, base_sha, result.head_sha)

        if timed_out:
            return finish(OUTCOME_TIMEOUT, f"claude exceeded BENCH_TIME_LIMIT_S={env.time_limit_s}")
        if proc.returncode != 0:
            # Raw status is in details.claude_exit_code; the wrapper's own exit
            # code comes from EXIT_FOR_OUTCOME so 2/3 cannot collide with
            # blocked/timeout.
            return finish(OUTCOME_FAILED, f"claude exited {proc.returncode}")
        if doc is None:
            return finish(OUTCOME_FAILED, "claude exited 0 but printed no JSON result document")
        if bool(doc.get("is_error")) or doc.get("subtype") not in (None, "success"):
            return finish(OUTCOME_FAILED, f"claude exited 0 but reported is_error={doc.get('is_error')} subtype={doc.get('subtype')!r}")
        if env.max_tokens is not None:
            total = common.tokens_total(result)
            if total is None:
                return finish(OUTCOME_FAILED, f"BENCH_MAX_TOKENS={env.max_tokens} is set but Claude's result has no usable usage block; cannot prove the cap was honored")
            if total > env.max_tokens:
                return finish(OUTCOME_FAILED, f"BENCH_MAX_TOKENS exceeded: {total} > {env.max_tokens} tokens")
        # A pass needs a real commit that builds on the benchmark base - the same
        # rule the AE wrapper applies to the product's branch.
        if result.head_sha is None:
            return finish(OUTCOME_FAILED, "claude reported success but no commit could be recorded in BENCH_REPO")
        if not details["base_is_ancestor"]:
            return finish(OUTCOME_FAILED, f"resulting head {result.head_sha} does not contain benchmark base {base_sha}; the checkout no longer builds on the pinned base")
        return finish(OUTCOME_PASSED)
    except WrapperError:
        log.close()
        raise
    except Exception as exc:  # noqa: BLE001
        details["exception"] = f"{type(exc).__name__}: {exc}"
        return finish(OUTCOME_FAILED, f"wrapper exception: {type(exc).__name__}: {exc}")


def _quote(arg: str) -> str:
    return arg if arg and all(ch.isalnum() or ch in "-_./=:,+" for ch in arg) else '"' + arg.replace('"', '\\"') + '"'


HELP = f"""Claude CLI competitor wrapper for scripts/code-implementation-benchmark.py.

Runs `claude -p --output-format json ...` in BENCH_REPO with BENCH_PROMPT_FILE on
stdin, restricted to the tools Autonomous Engine's implementation agent gets, then
commits the resulting working tree so the run has a head_sha.
Writes BENCH_OUTPUT_FILE (schema: benchmarks/wrappers/output_schema.json).

Runner contract (exported by the runner):
  BENCH_REPO, BENCH_PROMPT_FILE, BENCH_OUTPUT_FILE, BENCH_TASK_ID
Shared wrapper knobs:
  BENCH_TIME_LIMIT_S     wall-clock cap in seconds (default {int(common.DEFAULT_TIME_LIMIT_S)})
  BENCH_MAX_TOKENS       prompt+completion token cap checked against Claude's JSON usage
                         after the run (no CLI token flag exists); breach = failed.
                         Same cap as run-autonomous-engine.
  BENCH_RAW_DIR          raw-log root, outside BENCH_REPO (default <tmp>/bench-wrappers)
Claude CLI:
  BENCH_CLAUDE_BIN       executable name/path (default: claude, resolved on PATH)
  BENCH_MODEL            --model (record it; fairness rule 3 says fix model ids)
  BENCH_MAX_BUDGET_USD   --max-budget-usd; Claude-only, REFUSED unless BENCH_MAX_TOKENS is
                         also set (the AE wrapper cannot observe billed USD)
  BENCH_EFFORT           --effort (low|medium|high|xhigh|max)

Allowed tools:    {', '.join(ALLOWED_TOOLS)}
Denied patterns:  {', '.join(DISALLOWED_TOOLS)}

Exit codes: 0 passed_agent, 1 failed, 2 blocked, 3 timeout, 4 usage error (the same
table as run-autonomous-engine). Claude's raw exit status is recorded in
details.claude_exit_code, never returned verbatim. This wrapper never returns 2:
print-mode Claude has no escalation channel, so "blocked" cannot be observed.
A pass additionally requires a recorded head_sha that contains the base revision.
"""


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="run-claude", description=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--print-command", action="store_true", help="print the exact claude argv that would run and exit (no API calls)")
    args = parser.parse_args(argv)
    try:
        if args.print_command:
            binary = shutil.which(os.environ.get("BENCH_CLAUDE_BIN", "").strip() or "claude") or "claude"
            print(json.dumps(build_command(binary, os.environ), indent=2))
            return 0
        return run()
    except WrapperError as exc:
        print(f"run-claude: {exc}", file=sys.stderr)
        return EXIT_USAGE
