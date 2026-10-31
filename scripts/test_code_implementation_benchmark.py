#!/usr/bin/env python3
"""Self-tests for scripts/code-implementation-benchmark.py.

No network: a local bare repository stands in for the fixture remote (the runner's
local-path branch), and a fake wrapper writes the wrappers' normalized JSON document.

    python -m unittest scripts/test_code_implementation_benchmark.py
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "code-implementation-benchmark.py"
_spec = importlib.util.spec_from_file_location("code_implementation_benchmark", SCRIPT)
runner = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(runner)

FAKE_WRAPPER = textwrap.dedent(
    """
    import json, os, subprocess, sys
    # modify: 0 = leave the tree alone, 1 = change README.md and commit, 2 = change without committing,
    # 3 = add an untracked scratch file only
    outcome, exit_code, modify = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
    repo = os.environ["BENCH_REPO"]
    if modify in (1, 2):
        with open(os.path.join(repo, "README.md"), "a", encoding="utf-8") as fh:
            fh.write("agent guessed\\n")
    if modify == 1:
        subprocess.run(["git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com",
                        "commit", "-qam", "agent change"], check=True)
    if modify == 3:
        with open(os.path.join(repo, "NOTES.txt"), "w", encoding="utf-8") as fh:
            fh.write("blocked: missing spec\\n")
    head = subprocess.check_output(["git", "-C", repo, "rev-parse", "HEAD"], text=True).strip()
    doc = {
        "competitor": "fake", "task_id": os.environ["BENCH_TASK_ID"], "outcome": outcome,
        "elapsed_s": 0.1, "head_sha": head, "model_calls": 0, "tokens_in": 0, "tokens_out": 0,
        "cost_usd": 0.0, "versions": {"fake": "1"}, "raw_ref": None,
        "env": {k: os.environ.get(k) for k in ("BENCH_REPO", "BENCH_PROMPT_FILE", "BENCH_OUTPUT_FILE",
                                                "BENCH_TASK_ID", "BENCH_BASE_REVISION", "BENCH_TIME_LIMIT_S")},
    }
    with open(os.environ["BENCH_OUTPUT_FILE"], "w", encoding="utf-8") as fh:
        json.dump(doc, fh)
    sys.exit(exit_code)
    """
)

BLOCKED_RULE = 'git diff --quiet "$BENCH_BASE_REVISION" HEAD && git diff --cached --quiet'


def git(repo: Path, *args: str) -> str:
    return subprocess.check_output(
        ["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@example.com", *args], text=True
    ).strip()


class RunnerSelfTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.tmp = tempfile.TemporaryDirectory(prefix="bench-selftest-")
        base = Path(cls.tmp.name)
        source = base / "source"
        source.mkdir()
        subprocess.run(["git", "init", "-q", str(source)], check=True)
        (source / "README.md").write_text("fixture\n", encoding="utf-8")
        git(source, "add", "README.md")
        git(source, "commit", "-qm", "base")
        cls.base_sha = git(source, "rev-parse", "HEAD")
        cls.bare = base / "fixture.git"
        subprocess.run(["git", "clone", "-q", "--bare", str(source), str(cls.bare)], check=True)
        cls.wrapper = base / "fake_wrapper.py"
        cls.wrapper.write_text(FAKE_WRAPPER, encoding="utf-8")
        cls.python = Path(sys.executable).as_posix()
        cls.counter = 0

    @classmethod
    def tearDownClass(cls) -> None:
        cls.tmp.cleanup()

    def wrapper_command(self, outcome: str, exit_code: int, modify: int) -> str:
        return f'"{self.python}" "{self.wrapper.as_posix()}" {outcome} {exit_code} {int(modify)}'

    def make_task(self, expected: str) -> dict:
        type(self).counter += 1
        return {
            "id": f"t{self.counter}",
            "repo": str(self.bare),
            "base_revision": self.base_sha,
            "prompt": "do the thing",
            "test_command": BLOCKED_RULE if expected == "blocked" else "test -f README.md",
            "expected_outcome": expected,
        }

    def run_task(self, expected: str, outcome: str, exit_code: int, modify: int, timeout: int = 300) -> dict:
        task = self.make_task(expected)
        root = Path(self.tmp.name) / f"work{self.counter}"
        return runner.benchmark_task(task, "fake", self.wrapper_command(outcome, exit_code, modify), root, timeout)

    def test_blocked_outcome_with_clean_tree_passes(self) -> None:
        result = self.run_task("blocked", "blocked", 3, modify=0)
        self.assertEqual(result["wrapper_outcome"], "blocked")
        self.assertTrue(result["tracked_tree_unchanged"])
        self.assertEqual(result["tests"]["exit_code"], 0)
        self.assertTrue(result["passed"])
        self.assertEqual(result["changed_files_vs_base"], [])
        self.assertEqual(result["diff_stat_vs_base"], "")

    def test_blocked_outcome_with_untracked_scratch_file_passes(self) -> None:
        result = self.run_task("blocked", "blocked", 3, modify=3)
        self.assertTrue(result["tracked_tree_unchanged"])
        self.assertTrue(result["passed"])
        self.assertEqual(result["changed_files_vs_base"], ["?? NOTES.txt"])

    def test_nonzero_exit_with_failed_outcome_does_not_pass(self) -> None:
        result = self.run_task("blocked", "failed", 3, modify=0)
        self.assertEqual(result["agent"]["exit_code"], 3)
        self.assertTrue(result["tracked_tree_unchanged"])
        self.assertEqual(result["wrapper_outcome"], "failed")
        self.assertFalse(result["passed"])

    def test_blocked_outcome_with_committed_tracked_change_does_not_pass(self) -> None:
        result = self.run_task("blocked", "blocked", 3, modify=1)
        self.assertEqual(result["wrapper_outcome"], "blocked")
        self.assertFalse(result["tracked_tree_unchanged"])
        self.assertNotEqual(result["tests"]["exit_code"], 0)
        self.assertFalse(result["passed"])
        # Base-relative evidence sees the committed change even though HEAD-relative fields are empty.
        self.assertEqual(result["changed_files"], [])
        self.assertEqual(result["changed_files_vs_base"], ["M\tREADME.md"])
        self.assertIn("README.md", result["diff_stat_vs_base"])
        self.assertEqual(result["changed_file_count_vs_base"], 1)

    def test_blocked_outcome_with_uncommitted_tracked_change_does_not_pass(self) -> None:
        # Exercises the working-tree-vs-base check: HEAD still equals the base, the index is
        # clean, but a tracked file was modified in place.
        result = self.run_task("blocked", "blocked", 3, modify=2)
        self.assertEqual(result["wrapper_outcome"], "blocked")
        self.assertEqual(result["tests"]["exit_code"], 0, "rule command only sees HEAD and the index")
        self.assertFalse(result["tracked_tree_unchanged"])
        self.assertFalse(result["passed"])
        self.assertEqual(result["changed_files_vs_base"], [" M README.md"])

    def test_missing_output_document_does_not_pass_blocked(self) -> None:
        result = self.run_task("blocked", "blocked", 3, modify=0)
        self.assertIsNone(result["agent_output_error"])
        # Same scenario but the wrapper crashes before writing the document.
        task = self.make_task("blocked")
        root = Path(self.tmp.name) / f"work{self.counter}"
        crashed = runner.benchmark_task(task, "fake", "exit 7", root, 300)
        self.assertEqual(crashed["agent_output_error"], "missing")
        self.assertIsNone(crashed["wrapper_outcome"])
        self.assertFalse(crashed["passed"])

    def test_pass_fixture_keeps_exit_code_rule(self) -> None:
        result = self.run_task("pass", "passed_agent", 0, modify=0)
        self.assertTrue(result["passed"])
        failed = self.run_task("pass", "failed", 1, modify=0)
        self.assertFalse(failed["passed"])

    def test_pass_fixture_reports_base_relative_changes(self) -> None:
        result = self.run_task("pass", "passed_agent", 0, modify=1)
        self.assertTrue(result["passed"])
        self.assertEqual(result["changed_files"], [])
        self.assertEqual(result["changed_files_vs_base"], ["M\tREADME.md"])
        self.assertIn("1 file changed", result["diff_stat_vs_base"])

    def test_unrecognized_expected_outcome_fails_closed(self) -> None:
        task = self.make_task("pass")
        task["expected_outcome"] = "maybe"
        root = Path(self.tmp.name) / f"work{self.counter}"
        with self.assertRaises(ValueError):
            runner.benchmark_task(task, "fake", self.wrapper_command("passed_agent", 0, 0), root, 300)
        self.assertFalse(root.exists(), "fail closed before any checkout happens")

    def test_wrapper_environment_exports_base_revision_and_time_limit(self) -> None:
        result = self.run_task("blocked", "blocked", 3, modify=0, timeout=300)
        env = result["agent_output"]["env"]
        self.assertEqual(env["BENCH_BASE_REVISION"], self.base_sha)
        self.assertEqual(env["BENCH_TIME_LIMIT_S"], "240")
        self.assertEqual(result["time_limit_s"], 240)
        for key in ("BENCH_REPO", "BENCH_PROMPT_FILE", "BENCH_OUTPUT_FILE", "BENCH_TASK_ID"):
            self.assertTrue(env[key], key)
        floor = self.run_task("blocked", "blocked", 3, modify=0, timeout=40)
        self.assertEqual(floor["agent_output"]["env"]["BENCH_TIME_LIMIT_S"], str(runner.MIN_TIME_LIMIT_S))

    def test_time_limit_arithmetic(self) -> None:
        self.assertEqual(runner.time_limit_seconds(1800), 1740)
        self.assertEqual(runner.time_limit_seconds(1800, 120), 1680)
        self.assertEqual(runner.time_limit_seconds(45), runner.MIN_TIME_LIMIT_S)
        self.assertEqual(runner.time_limit_seconds(10, 0), runner.MIN_TIME_LIMIT_S)


    # --- M4-03: trials, strata, flattened wrapper fields, false_success, incremental output ---

    STRATA = {
        "language": "go",
        "task_class": "historical_defect",
        "difficulty": "easy",
        "size_stratum": "small",
        "partition": "sealed",
        "fail_mode": "assert",
    }

    def test_record_carries_trial_run_id_strata_and_flattened_fields(self) -> None:
        task = {**self.make_task("pass"), **self.STRATA}
        root = Path(self.tmp.name) / f"work{self.counter}"
        result = runner.benchmark_task(
            task, "fake", self.wrapper_command("passed_agent", 0, 0), root, 300, trial=2, run_id="run-x"
        )
        self.assertEqual(result["trial"], 2)
        self.assertEqual(result["run_id"], "run-x")
        for field, value in self.STRATA.items():
            self.assertEqual(result[field], value, field)
        self.assertEqual(result["expected_outcome"], "pass")
        self.assertEqual(result["tokens_in"], 0)
        self.assertEqual(result["tokens_out"], 0)
        self.assertEqual(result["cost_usd"], 0.0)
        self.assertEqual(result["model_calls"], 0)
        self.assertEqual(result["elapsed_s"], 0.1)
        self.assertEqual(result["versions"], {"fake": "1"})
        self.assertEqual(result["head_sha"], self.base_sha)
        self.assertEqual(result["agent_output"]["outcome"], "passed_agent", "agent_output kept intact")
        self.assertTrue((root / "fake" / f"{task['id']}.trial2").is_dir(), "later trials get their own clone")

    def test_flattened_fields_are_null_without_output_document(self) -> None:
        task = self.make_task("pass")
        root = Path(self.tmp.name) / f"work{self.counter}"
        result = runner.benchmark_task(task, "fake", "exit 7", root, 300)
        for field in runner.FLATTENED_OUTPUT_FIELDS:
            self.assertIsNone(result[field], field)
        self.assertIsNone(result["language"], "strata absent from the manifest stay null")
        self.assertEqual(result["trial"], 0)
        self.assertFalse(result["false_success"])
        self.assertFalse(result["unsafe_edit_on_blocked"])

    def test_false_success_on_pass_task(self) -> None:
        task = self.make_task("pass")
        task["test_command"] = "exit 1"  # hidden tests fail
        root = Path(self.tmp.name) / f"work{self.counter}"
        claimed = runner.benchmark_task(task, "fake", self.wrapper_command("passed_agent", 0, 1), root, 300)
        self.assertFalse(claimed["passed"])
        self.assertTrue(claimed["false_success"])
        task = self.make_task("pass")
        task["test_command"] = "exit 1"
        root = Path(self.tmp.name) / f"work{self.counter}"
        honest = runner.benchmark_task(task, "fake", self.wrapper_command("failed", 1, 0), root, 300)
        self.assertFalse(honest["false_success"])
        self.assertFalse(self.run_task("pass", "passed_agent", 0, modify=0)["false_success"])

    def test_false_success_on_blocked_task(self) -> None:
        guessed = self.run_task("blocked", "passed_agent", 0, modify=1)
        self.assertFalse(guessed["passed"])
        self.assertTrue(guessed["false_success"])
        self.assertFalse(guessed["unsafe_edit_on_blocked"], "a claimed success is a false success instead")
        clean_failure = self.run_task("blocked", "failed", 1, modify=0)
        self.assertFalse(clean_failure["false_success"])
        self.assertFalse(clean_failure["unsafe_edit_on_blocked"], "tree unchanged")
        correct = self.run_task("blocked", "blocked", 2, modify=0)
        self.assertTrue(correct["passed"])
        self.assertFalse(correct["false_success"])
        self.assertFalse(correct["unsafe_edit_on_blocked"])

    def test_honest_failure_with_edits_on_blocked_task_is_unsafe_edit_not_false_success(self) -> None:
        for outcome, code in (("failed", 1), ("timeout", 3)):
            result = self.run_task("blocked", outcome, code, modify=1)
            self.assertFalse(result["tracked_tree_unchanged"])
            self.assertFalse(result["false_success"], outcome)
            self.assertTrue(result["unsafe_edit_on_blocked"], outcome)
        self.assertFalse(runner.is_false_success("blocked", None, 1), "missing document is not a claim")
        self.assertTrue(runner.is_unsafe_edit_on_blocked("blocked", None, False))
        self.assertFalse(runner.is_unsafe_edit_on_blocked("pass", "failed", False))

    def write_manifest(self, tasks: list[dict]) -> Path:
        type(self).counter += 1
        path = Path(self.tmp.name) / f"manifest{self.counter}.json"
        path.write_text(json.dumps({"tasks": tasks}), encoding="utf-8")
        return path

    def run_main(self, manifest: Path, output: Path, *extra: str) -> int:
        argv = [
            "--manifest", str(manifest),
            "--candidate-command", self.wrapper_command("passed_agent", 0, 0),
            "--baseline-command", self.wrapper_command("failed", 1, 0),
            "--output", str(output),
            "--timeout", "300",
            *extra,
        ]
        with contextlib.redirect_stdout(io.StringIO()):
            return runner.main(argv)

    def test_main_runs_trials_alternates_order_and_writes_jsonl(self) -> None:
        tasks = [{**self.make_task("pass"), **self.STRATA}, self.make_task("blocked")]
        manifest = self.write_manifest(tasks)
        output = Path(self.tmp.name) / f"out{self.counter}" / "run.json"
        code = self.run_main(manifest, output, "--trials", "2")
        self.assertEqual(code, 1, "baseline fails, so the data-collection exit status is 1")
        payload = json.loads(output.read_text(encoding="utf-8"))
        results = payload["results"]
        self.assertEqual(len(results), 2 * 2 * 2)
        self.assertEqual(payload["trials"], 2)
        self.assertEqual(payload["comparison"], "primary")
        self.assertEqual(payload["competitors"], {"candidate": "autonomous-engine", "baseline": "claude-cli"})
        run_ids = {r["run_id"] for r in results}
        self.assertEqual(run_ids, {payload["run_id"]})
        self.assertEqual(len(payload["run_id"]), 36)
        order = [(r["task_id"], r["trial"], r["competitor"]) for r in results]
        first, second = tasks[0]["id"], tasks[1]["id"]
        self.assertEqual(
            order,
            [
                (first, 0, "autonomous-engine"), (first, 0, "claude-cli"),
                (first, 1, "claude-cli"), (first, 1, "autonomous-engine"),
                (second, 0, "claude-cli"), (second, 0, "autonomous-engine"),
                (second, 1, "autonomous-engine"), (second, 1, "claude-cli"),
            ],
        )
        roles = {r["competitor"]: r["role"] for r in results}
        self.assertEqual(roles, {"autonomous-engine": "candidate", "claude-cli": "baseline"})
        self.assertEqual(results[0]["language"], "go")
        lines = output.with_suffix(".jsonl").read_text(encoding="utf-8").splitlines()
        self.assertEqual([json.loads(line) for line in lines], results)

    def test_jsonl_keeps_finished_records_when_a_later_task_crashes(self) -> None:
        good = self.make_task("pass")
        bad = self.make_task("pass")
        bad["base_revision"] = "f" * 40  # passes validation, fails at checkout
        manifest = self.write_manifest([good, bad])
        output = Path(self.tmp.name) / f"out{self.counter}" / "run.json"
        with self.assertRaises(subprocess.CalledProcessError):
            with contextlib.redirect_stderr(io.StringIO()):
                self.run_main(manifest, output)
        self.assertFalse(output.exists(), "final JSON is only written at the end")
        lines = output.with_suffix(".jsonl").read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 2)
        self.assertEqual({json.loads(line)["task_id"] for line in lines}, {good["id"]})

    def test_manifest_validated_before_any_task_runs(self) -> None:
        good = self.make_task("pass")
        bad_outcome = self.make_task("pass")
        bad_outcome["expected_outcome"] = "maybe"
        missing = self.make_task("pass")
        del missing["test_command"]
        manifest = self.write_manifest([good, bad_outcome, missing])
        output = Path(self.tmp.name) / f"out{self.counter}" / "run.json"
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            code = self.run_main(manifest, output)
        self.assertEqual(code, 2)
        self.assertFalse(output.with_suffix(".jsonl").exists(), "no task ran")
        self.assertFalse(output.exists())
        self.assertIn("'maybe'", stderr.getvalue())
        self.assertIn("test_command", stderr.getvalue())

    def test_validate_manifest_messages(self) -> None:
        self.assertEqual(runner.validate_manifest({"tasks": [self.make_task("pass")]}), [])
        self.assertTrue(runner.validate_manifest({"nope": []}))
        dup = self.make_task("pass")
        errors = runner.validate_manifest({"tasks": [dup, dict(dup), {"id": "x"}]})
        self.assertTrue(any("duplicate" in e for e in errors))
        self.assertTrue(any("prompt" in e for e in errors))
        self.assertTrue(any("'repo'" in e for e in errors))

    def test_jsonl_path(self) -> None:
        self.assertEqual(runner.jsonl_path(Path("a/run.json")), Path("a/run.jsonl"))
        self.assertEqual(runner.jsonl_path(Path("a/run")), Path("a/run.jsonl"))
        self.assertEqual(runner.jsonl_path(Path("a/run.jsonl")), Path("a/run.jsonl.jsonl"))

    def test_help_says_exit_status_is_not_a_gate(self) -> None:
        out = io.StringIO()
        with contextlib.redirect_stdout(out), self.assertRaises(SystemExit):
            runner.main(["--help"])
        text = " ".join(out.getvalue().split())
        self.assertIn("NOT a paired-comparison gate", text)
        self.assertIn("--trials", text)


    def test_manifest_file_paths_must_exist(self) -> None:
        base = Path(self.tmp.name) / f"mf{self.counter}"
        (base / "tasks" / "ok" / "hidden-tests").mkdir(parents=True)
        (base / "tasks" / "ok" / "prompt.md").write_text("do it", encoding="utf-8")
        good = {**self.make_task("pass"), "prompt_file": "tasks/ok/prompt.md", "hidden_tests_dir": "tasks/ok/hidden-tests"}
        del good["prompt"]
        missing_prompt = {**good, "id": "missing-prompt", "prompt_file": "tasks/nope/prompt.md"}
        missing_hidden = {**good, "id": "missing-hidden", "hidden_tests_dir": "tasks/nope/hidden-tests"}
        self.assertEqual(runner.validate_manifest({"tasks": [good]}, base), [])
        errors = runner.validate_manifest({"tasks": [good, missing_prompt, missing_hidden]}, base)
        self.assertEqual(len(errors), 2, errors)
        self.assertIn("prompt_file", errors[0])
        self.assertIn("hidden_tests_dir", errors[1])
        manifest = base / "tasks.json"
        manifest.write_text(json.dumps({"tasks": [good, missing_hidden]}), encoding="utf-8")
        output = base / "out" / "run.json"
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            code = self.run_main(manifest, output)
        self.assertEqual(code, 2)
        self.assertFalse(output.with_suffix(".jsonl").exists(), "rejected before any clone")
        self.assertIn("hidden_tests_dir", stderr.getvalue())

    def test_empty_inline_prompt_rejected_in_validator_and_runner(self) -> None:
        task = self.make_task("pass")
        task["prompt"] = ""
        task["prompt_file"] = "ignored-because-inline-prompt-wins.md"
        errors = runner.validate_manifest({"tasks": [task]})
        self.assertEqual(len(errors), 1)
        self.assertIn("inline 'prompt' is empty", errors[0])
        root = Path(self.tmp.name) / f"work{self.counter}"
        with self.assertRaises(ValueError):
            runner.benchmark_task(task, "fake", self.wrapper_command("passed_agent", 0, 0), root, 300)
        self.assertFalse(root.exists(), "fails before checkout")

if __name__ == "__main__":
    unittest.main()
