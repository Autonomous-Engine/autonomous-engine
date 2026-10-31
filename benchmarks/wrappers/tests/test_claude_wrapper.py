from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import helpers

from wrappers import claude_cli, common
from wrappers.schema import load_schema, validate


class ClaudeWrapperTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="claude-wrapper-")
        self.root = Path(self._tmp.name)
        self.repo = helpers.make_repo(self.root)
        self.bench = self.repo["bench"]
        self.prompt_file = self.bench / ".benchmark-prompt.txt"
        self.prompt_file.write_text("Add a subtract(a, b) function to app.py.\nKeep add() unchanged.", encoding="utf-8")
        self.output_file = self.bench / ".benchmark-agent-output.txt"
        self.bin_dir = self.root / "fakebin"
        helpers.make_fake_claude(self.bin_dir)
        self.args_file = self.root / "claude-args.json"
        self.pid_file = self.root / "claude.pid"

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def environ(self, mode: str = "success", **overrides: str) -> dict[str, str]:
        env = {
            "PATH": helpers.path_with(self.bin_dir, dict(os.environ)),
            "BENCH_REPO": str(self.bench),
            "BENCH_PROMPT_FILE": str(self.prompt_file),
            "BENCH_OUTPUT_FILE": str(self.output_file),
            "BENCH_TASK_ID": "sample-fix",
            "BENCH_TIME_LIMIT_S": "30",
            "BENCH_RAW_DIR": str(self.root / "raw"),
            "BENCH_QUIET": "1",
            "FAKE_CLAUDE_MODE": mode,
            "FAKE_CLAUDE_ARGS_FILE": str(self.args_file),
            "FAKE_CLAUDE_PID_FILE": str(self.pid_file),
        }
        for key in ("SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE", "PATHEXT", "COMSPEC"):
            if key in os.environ:
                env[key] = os.environ[key]
        env.update(overrides)
        return env

    def read_output(self) -> dict:
        doc = json.loads(self.output_file.read_text(encoding="utf-8"))
        self.assertEqual(validate(doc, load_schema()), [])
        self.assertTrue(Path(doc["raw_ref"]).is_file())
        return doc

    def recorded_args(self) -> dict:
        return json.loads(self.args_file.read_text(encoding="utf-8"))

    # --- scenarios ---------------------------------------------------------

    def test_success_commits_working_tree_and_normalizes_metrics(self) -> None:
        code = claude_cli.run(self.environ(BENCH_MAX_TOKENS="5000"))

        self.assertEqual(code, 0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "passed_agent")
        self.assertEqual(doc["competitor"], "claude-cli")
        head = helpers.git(self.bench, "rev-parse", "HEAD")
        self.assertEqual(doc["head_sha"], head)
        self.assertNotEqual(head, self.repo["base_sha"])
        committed = helpers.git(self.bench, "show", "--name-only", "--format=", "HEAD").splitlines()
        self.assertIn("claude-output.txt", committed)
        self.assertNotIn(".benchmark-prompt.txt", committed)
        self.assertNotIn(".benchmark-agent-output.txt", committed)
        self.assertTrue(self.prompt_file.is_file())
        self.assertEqual(doc["tokens_in"], 10 + 100 + 1000)
        self.assertEqual(doc["tokens_out"], 42)
        self.assertEqual(doc["model_calls"], 3)
        self.assertAlmostEqual(doc["cost_usd"], 0.1234)
        self.assertEqual(doc["versions"]["claude_cli"], "0.0.0-fake (Claude Code)")
        self.assertEqual(doc["versions"]["models"], ["claude-fake-1"])
        self.assertEqual(doc["details"]["claude_exit_code"], 0)
        self.assertEqual(doc["details"]["exit_code"], 0)
        self.assertEqual(doc["details"]["claude"]["subtype"], "success")
        self.assertEqual(doc["details"]["status_porcelain"], ["?? claude-output.txt"])
        # diff against the runner's pinned base, since the committed tree leaves the runner's own diff empty
        self.assertEqual(doc["details"]["changed_files"], ["claude-output.txt"])
        self.assertIn("claude-output.txt", doc["details"]["diff_stat"])
        self.assertEqual(doc["details"]["diff_base"], self.repo["base_sha"])
        self.assertEqual(doc["details"]["diff_head"], head)
        self.assertEqual(doc["details"]["token_cap"], {"limit": 5000, "observed": 1152, "verified": True})
        self.assertTrue(doc["details"]["base_is_ancestor"])

        recorded = self.recorded_args()
        argv = recorded["argv"]
        self.assertEqual(recorded["prompt"], self.prompt_file.read_text(encoding="utf-8"))
        self.assertEqual(Path(recorded["cwd"]).resolve(), self.bench.resolve())
        self.assertIn("-p", argv)
        self.assertEqual(argv[argv.index("--output-format") + 1], "json")
        self.assertEqual(argv[argv.index("--permission-mode") + 1], "dontAsk")
        self.assertEqual(argv[argv.index("--permission-prompts") + 1], "none")
        self.assertEqual(argv[argv.index("--tools") + 1], "Read,Edit,Write,Glob,Grep,Bash")
        self.assertEqual(argv[argv.index("--setting-sources") + 1], "project")
        for flag in ("--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"):
            self.assertIn(flag, argv)
        self.assertIn("Bash(git push*)", argv)
        self.assertIn("Bash(git remote*)", argv)
        self.assertIn("Bash(sudo *)", argv)
        # local git stays allowed: AE's executor does not block it either
        for local_git in ("Bash(git commit*)", "Bash(git checkout*)", "Bash(git reset*)", "Bash(git stash*)", "Bash(git branch*)"):
            self.assertNotIn(local_git, argv)
        self.assertNotIn("--dangerously-skip-permissions", argv)
        self.assertNotIn("--allow-dangerously-skip-permissions", argv)
        self.assertNotIn("bypassPermissions", argv)
        self.assertNotIn("--model", argv)
        self.assertNotIn("--max-budget-usd", argv)

    def test_nonzero_exit_is_failed_in_wrapper_code_space(self) -> None:
        code = claude_cli.run(self.environ("fail"))
        # Claude exited 7; the wrapper reports failed (1), never the raw status,
        # so a raw 2/3 can never masquerade as blocked/timeout.
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertEqual(doc["details"]["claude_exit_code"], 7)
        self.assertEqual(doc["details"]["exit_code"], common.EXIT_FAILED)
        self.assertIn("claude exited 7", doc["details"]["reason"])
        self.assertEqual(doc["head_sha"], self.repo["base_sha"])  # nothing to commit
        self.assertEqual(doc["tokens_in"], 5)
        self.assertEqual(doc["model_calls"], 2)
        self.assertTrue(doc["details"]["claude"]["is_error"])

    def test_error_reported_with_exit_zero_is_still_failure(self) -> None:
        code = claude_cli.run(self.environ("error_exit0"))
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertEqual(doc["details"]["claude_exit_code"], 0)
        self.assertIn("error_max_budget_usd", doc["details"]["reason"])

    def test_timeout_kills_claude_and_exits_non_zero(self) -> None:
        started = time.monotonic()
        code = claude_cli.run(self.environ("hang", BENCH_TIME_LIMIT_S="1"))
        elapsed = time.monotonic() - started
        self.assertEqual(code, common.EXIT_TIMEOUT)
        self.assertLess(elapsed, 40.0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "timeout")
        self.assertIsNone(doc["details"]["claude_exit_code"])
        self.assertGreaterEqual(doc["elapsed_s"], 1.0)
        # partial work is still committed so head_sha is honest about what exists
        self.assertIn("half-done.txt", helpers.git(self.bench, "show", "--name-only", "--format=", "HEAD").splitlines())
        self.assertEqual(doc["details"]["changed_files"], ["half-done.txt"])
        # the hung child (the sleeping python behind the shim) must actually be dead
        self.assertTrue(self.pid_file.is_file(), "fake claude never reached its sleep")
        pid = int(self.pid_file.read_text(encoding="utf-8").strip())
        self.assertTrue(helpers.wait_for_death(pid), f"fake claude pid {pid} still alive after timeout kill")

    def test_model_budget_and_effort_flags(self) -> None:
        code = claude_cli.run(self.environ(BENCH_MODEL="claude-fake-1", BENCH_MAX_BUDGET_USD="2.5", BENCH_EFFORT="high", BENCH_MAX_TOKENS="5000"))
        self.assertEqual(code, 0)
        argv = self.recorded_args()["argv"]
        self.assertEqual(argv[argv.index("--model") + 1], "claude-fake-1")
        self.assertEqual(argv[argv.index("--max-budget-usd") + 1], "2.5")
        self.assertEqual(argv[argv.index("--effort") + 1], "high")
        self.assertEqual(self.read_output()["versions"]["model_requested"], "claude-fake-1")

    def test_bad_budget_is_usage_error(self) -> None:
        with self.assertRaises(common.WrapperError):
            claude_cli.run(self.environ(BENCH_MAX_BUDGET_USD="free", BENCH_MAX_TOKENS="5000"))

    def test_budget_without_shared_token_cap_is_refused(self) -> None:
        with self.assertRaises(common.WrapperError) as ctx:
            claude_cli.run(self.environ(BENCH_MAX_BUDGET_USD="2.5"))
        self.assertIn("BENCH_MAX_TOKENS", str(ctx.exception))
        self.assertFalse(self.args_file.exists())  # nothing was executed

    def test_token_cap_breach_is_failed(self) -> None:
        code = claude_cli.run(self.environ(BENCH_MAX_TOKENS="100"))  # fake reports 1110 in + 42 out
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIn("BENCH_MAX_TOKENS exceeded: 1152 > 100", doc["details"]["reason"])
        self.assertEqual(doc["details"]["token_cap"], {"limit": 100, "observed": 1152, "verified": True})
        self.assertEqual(doc["details"]["claude_exit_code"], 0)
        self.assertEqual(doc["tokens_in"], 1110)

    def test_token_cap_emitted_on_every_path_when_set(self) -> None:
        code = claude_cli.run(self.environ("hang", BENCH_TIME_LIMIT_S="1", BENCH_MAX_TOKENS="5000"))
        self.assertEqual(code, common.EXIT_TIMEOUT)
        doc = self.read_output()
        self.assertEqual(doc["details"]["token_cap"], {"limit": 5000, "observed": None, "verified": False})

    def test_success_without_commit_is_failed(self) -> None:
        code = claude_cli.run(self.environ("nuke_git"))
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIsNone(doc["head_sha"])
        self.assertFalse(doc["details"]["base_is_ancestor"])
        self.assertIn("no commit could be recorded", doc["details"]["reason"])
        self.assertIn("commit_error", doc["details"])
        self.assertIsNone(doc["details"]["diff_head"])
        self.assertIsNone(doc["details"]["changed_files"])

    def test_success_on_head_not_containing_base_is_failed(self) -> None:
        code = claude_cli.run(self.environ("orphan"))
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIsNotNone(doc["head_sha"])
        self.assertNotEqual(doc["head_sha"], self.repo["base_sha"])
        self.assertFalse(doc["details"]["base_is_ancestor"])
        self.assertIn("does not contain benchmark base", doc["details"]["reason"])
        self.assertEqual(doc["details"]["claude_exit_code"], 0)

    def test_bad_token_cap_is_usage_error(self) -> None:
        with self.assertRaises(common.WrapperError):
            claude_cli.run(self.environ(BENCH_MAX_TOKENS="0"))

    def test_missing_binary_is_usage_error(self) -> None:
        env = self.environ(BENCH_CLAUDE_BIN="definitely-not-claude-xyz")
        with self.assertRaises(common.WrapperError):
            claude_cli.run(env)

    def test_parse_result_json_tolerates_noise(self) -> None:
        doc = claude_cli.parse_result_json('warning line\n{"type":"result","subtype":"success","is_error":false}\n')
        self.assertEqual(doc["subtype"], "success")
        self.assertIsNone(claude_cli.parse_result_json(""))
        self.assertIsNone(claude_cli.parse_result_json("not json"))

    def test_help_and_print_command_via_entry_script(self) -> None:
        script = helpers.BENCHMARKS_DIR / "run-claude.py"
        proc = subprocess.run([sys.executable, str(script), "--help"], capture_output=True, text=True, check=False)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        for name in ("BENCH_MODEL", "BENCH_MAX_BUDGET_USD", "BENCH_MAX_TOKENS", "BENCH_TIME_LIMIT_S", "Bash(git push*)", "details.claude_exit_code", "never returns 2"):
            self.assertIn(name, proc.stdout)
        proc = subprocess.run([sys.executable, str(script), "--print-command"], capture_output=True, text=True, check=False, env=self.environ())
        self.assertEqual(proc.returncode, 0, proc.stderr)
        argv = json.loads(proc.stdout)
        self.assertIn("--output-format", argv)
        self.assertFalse(self.args_file.exists())  # nothing was executed


if __name__ == "__main__":
    unittest.main()
