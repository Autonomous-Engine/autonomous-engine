from __future__ import annotations

import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import helpers

from wrappers import ae, common
from wrappers.schema import load_schema, validate

FULL_SUCCESS = ["intake", "prd", "prd_approval", "research", "planning", "implementation", "review", "human_review", "qa", "passed"]
TOKEN = "test-token"


class AEWrapperTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="ae-wrapper-")
        self.root = Path(self._tmp.name)
        self.repo = helpers.make_repo(self.root)
        self.bench = self.repo["bench"]
        self.prompt_file = self.bench / ".benchmark-prompt.txt"
        self.prompt_file.write_text("Add a subtract(a, b) function to app.py.\nKeep add() unchanged.", encoding="utf-8")
        self.output_file = self.bench / ".benchmark-agent-output.txt"
        self.server = None

    def tearDown(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        self._tmp.cleanup()

    def start(self, fake: helpers.FakeAE) -> str:
        self.server, url = helpers.start_fake_ae(fake)
        return url

    def environ(self, url: str, **overrides: str) -> dict[str, str]:
        env = {
            "PATH": os.environ.get("PATH", ""),
            "BENCH_REPO": str(self.bench),
            "BENCH_PROMPT_FILE": str(self.prompt_file),
            "BENCH_OUTPUT_FILE": str(self.output_file),
            "BENCH_TASK_ID": "sample-fix",
            "BENCH_TIME_LIMIT_S": "30",
            "BENCH_RAW_DIR": str(self.root / "raw"),
            "BENCH_QUIET": "1",
            "AE_GATEWAY_URL": url,
            "AE_DASHBOARD_URL": url,
            "AE_API_TOKEN": TOKEN,
            "AE_PRODUCT_ID": "11111111-1111-1111-1111-111111111111",
            "AE_POLL_INTERVAL_S": "0.02",
            # the fork the product pushes to, as a path/URL; the clone has no origin at all
            "AE_FETCH_REMOTE": str(self.repo["remote"]),
        }
        for key in ("SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE"):
            if key in os.environ:
                env[key] = os.environ[key]
        env.update(overrides)
        return env

    def read_output(self) -> dict:
        doc = json.loads(self.output_file.read_text(encoding="utf-8"))
        self.assertEqual(validate(doc, load_schema()), [])
        self.assertTrue(Path(doc["raw_ref"]).is_file())
        return doc

    # --- scenarios ---------------------------------------------------------

    def test_success_checks_out_produced_commit(self) -> None:
        fake = helpers.FakeAE(FULL_SUCCESS)
        url = self.start(fake)
        branch = f"agent/{fake.work_id}/abcd1234"
        sha = helpers.push_agent_branch(self.root, self.repo["remote"], branch, {"app.py": "def add(a, b):\n    return a + b\n\n\ndef subtract(a, b):\n    return a - b\n"})
        fake.pending = [{"id": "p1", "work_item_id": fake.work_id, "branch": branch, "base_branch": "main", "status": "pending", "repository_id": "r1"}]

        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            code = ae.run(self.environ(url, BENCH_MAX_TOKENS="5000"))

        self.assertEqual(code, 0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "passed_agent")
        # the stdout mirror is printed even with BENCH_QUIET=1 (the runner keeps stdout)
        mirror = [line for line in stdout.getvalue().splitlines() if line.startswith("BENCH_RESULT ")]
        self.assertEqual(len(mirror), 1)
        self.assertEqual(json.loads(mirror[0][len("BENCH_RESULT "):]), doc)
        # the runner's own diff is empty after a checkout, so the wrapper records its own vs the base
        self.assertEqual(doc["details"]["changed_files"], ["app.py"])
        self.assertIn("app.py", doc["details"]["diff_stat"])
        self.assertEqual(doc["details"]["diff_base"], self.repo["base_sha"])
        self.assertEqual(doc["details"]["merged_branches"], [{"branch": branch, "head": sha}])
        self.assertFalse(doc["details"]["wrapper_side_merge"])
        self.assertEqual(doc["details"]["token_cap"], {"limit": 5000, "observed": 1801, "verified": True})
        self.assertEqual(doc["details"]["token_accounting"], "partial")
        self.assertTrue(doc["details"]["base_is_ancestor"])
        self.assertEqual(doc["details"]["checked_out_branch"], "bench-autonomous-engine")
        self.assertEqual(doc["details"]["exit_code"], 0)
        self.assertEqual(helpers.git(self.bench, "remote"), "")  # origin was never needed
        self.assertEqual(doc["competitor"], "autonomous-engine")
        self.assertEqual(doc["task_id"], "sample-fix")
        self.assertEqual(doc["head_sha"], sha)
        self.assertEqual(helpers.git(self.bench, "rev-parse", "HEAD"), sha)
        self.assertIn("subtract", (self.bench / "app.py").read_text(encoding="utf-8"))
        self.assertEqual(doc["model_calls"], 9)
        self.assertEqual(doc["tokens_in"], 1234)
        self.assertEqual(doc["tokens_out"], 567)
        self.assertIsNone(doc["cost_usd"])
        self.assertEqual(doc["versions"]["autonomous_engine"], "fake-1.0")
        self.assertEqual(doc["versions"]["agent_definition"]["model_id"], "fake-model-1")
        self.assertEqual(doc["details"]["work_item_id"], fake.work_id)
        self.assertEqual(doc["details"]["final_state"], "passed")
        self.assertEqual(doc["details"]["pending_changes"][0]["branch"], branch)
        self.assertEqual(doc["details"]["remote_branches"], {branch: sha})
        self.assertTrue(doc["details"]["base_is_ancestor"])
        # both human gates were approved through the dashboard endpoints
        self.assertEqual(fake.approvals, ["prd_approval", "human_review"])
        # intake carried the identical prompt and the benchmark metadata
        self.assertEqual(fake.intake_body["description"], self.prompt_file.read_text(encoding="utf-8"))
        self.assertEqual(fake.intake_body["product_id"], "11111111-1111-1111-1111-111111111111")
        self.assertEqual(fake.intake_body["metadata"]["benchmark"]["task_id"], "sample-fix")
        self.assertEqual(fake.intake_body["metadata"]["benchmark"]["base_sha"], self.repo["base_sha"])
        # the runner's prompt file survived the checkout untouched
        self.assertTrue(self.prompt_file.is_file())
        # every dashboard call carried the bearer token
        for req in fake.requests:
            if req["path"].startswith("/api/work-items"):
                self.assertEqual(req["auth"], "Bearer " + TOKEN, req)

    def test_config_snapshot_env_reaches_the_intake(self) -> None:
        # M4-06: AE_CONFIG_SNAPSHOT_ID / AE_ROLLOUT_ID are passed to the gateway as intake
        # metadata (honoured for benchmark-sourced intakes) and the requested snapshot is
        # recorded in versions; without them the intake carries neither key.
        snap = "a" * 64
        fake = helpers.FakeAE(["intake", "implementation", "failed"])
        url = self.start(fake)
        with contextlib.redirect_stdout(io.StringIO()):
            ae.run(self.environ(url, AE_CONFIG_SNAPSHOT_ID=snap, AE_ROLLOUT_ID="r-1"))
        self.assertEqual(fake.intake_body["metadata"]["config_snapshot_id"], snap)
        self.assertEqual(fake.intake_body["metadata"]["rollout_id"], "r-1")
        self.assertEqual(fake.intake_body["source"], "benchmark")
        self.assertEqual(self.read_output()["versions"]["requested_config_snapshot_id"], snap)

        self.server.shutdown()
        self.server.server_close()
        fake2 = helpers.FakeAE(["intake", "implementation", "failed"])
        url2 = self.start(fake2)
        with contextlib.redirect_stdout(io.StringIO()):
            ae.run(self.environ(url2))
        self.assertNotIn("config_snapshot_id", fake2.intake_body["metadata"])
        self.assertNotIn("rollout_id", fake2.intake_body["metadata"])

    def test_success_merges_multiple_subtask_branches(self) -> None:
        fake = helpers.FakeAE(["intake", "implementation", "passed"])
        url = self.start(fake)
        sha1 = helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/aaaa0001", {"one.py": "ONE = 1\n"})
        time.sleep(1.1)  # distinct committer timestamps -> deterministic merge order
        sha2 = helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/bbbb0002", {"two.py": "TWO = 2\n"})

        code = ae.run(self.environ(url))

        self.assertEqual(code, 0)
        doc = self.read_output()
        head = helpers.git(self.bench, "rev-parse", "HEAD")
        self.assertEqual(doc["head_sha"], head)
        self.assertNotIn(head, (sha1, sha2))  # a merge commit
        self.assertTrue((self.bench / "one.py").is_file())
        self.assertTrue((self.bench / "two.py").is_file())
        self.assertEqual(
            doc["details"]["merged_branches"],
            [{"branch": f"agent/{fake.work_id}/aaaa0001", "head": sha1}, {"branch": f"agent/{fake.work_id}/bbbb0002", "head": sha2}],
        )
        self.assertTrue(doc["details"]["wrapper_side_merge"])
        self.assertEqual(sorted(doc["details"]["changed_files"]), ["one.py", "two.py"])

    def test_token_cap_breach_cancels_and_fails(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "research", "planning", "implementation", "review", "qa", "passed"])
        fake.cost = {"total_input_tokens": 9000, "total_output_tokens": 500, "total_tokens": 9500, "total_llm_calls": 40}
        url = self.start(fake)
        helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/abcd1234", {"x.txt": "x\n"})

        code = ae.run(self.environ(url, BENCH_MAX_TOKENS="1000"))

        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIn("BENCH_MAX_TOKENS exceeded: 9500 > 1000", doc["details"]["reason"])
        self.assertEqual(doc["details"]["token_cap"], {"limit": 1000, "observed": 9500, "verified": True})
        self.assertTrue(fake.cancelled)
        self.assertTrue(doc["details"]["cancelled_after_token_cap"])
        self.assertIsNone(doc["head_sha"])  # never checked out; the run was cut short
        self.assertEqual(doc["tokens_in"], 9000)
        self.assertEqual(doc["model_calls"], 40)
        self.assertNotIn("passed", [h["state"] for h in doc["details"]["state_history"]])

    def test_token_cap_unverifiable_fails_closed(self) -> None:
        fake = helpers.FakeAE(["intake", "implementation", "passed"])
        fake.cost_error = True  # /api/work-items/cost answers 500 for the whole run
        url = self.start(fake)
        helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/abcd1234", {"x.txt": "x\n"})

        code = ae.run(self.environ(url, BENCH_MAX_TOKENS="100000"))

        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIn("cannot prove the cap was honored", doc["details"]["reason"])
        self.assertEqual(doc["details"]["token_cap"], {"limit": 100000, "observed": None, "verified": False})
        self.assertIsNone(doc["tokens_in"])
        self.assertIsNone(doc["model_calls"])
        self.assertIsNone(doc["head_sha"])  # never checked out: the cap gate comes first
        self.assertEqual(doc["details"]["final_state"], "passed")

    def test_zero_tagged_calls_is_unknown_not_zero(self) -> None:
        fake = helpers.FakeAE(["intake", "implementation", "passed"])
        fake.cost = {"total_input_tokens": 0, "total_output_tokens": 0, "total_tokens": 0, "total_llm_calls": 0}
        url = self.start(fake)
        helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/abcd1234", {"x.txt": "x\n"})

        code = ae.run(self.environ(url))  # no cap: partial accounting must not fail the run

        self.assertEqual(code, 0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "passed_agent")
        self.assertIsNone(doc["model_calls"])
        self.assertIsNone(doc["tokens_in"])
        self.assertIsNone(doc["tokens_out"])
        self.assertEqual(doc["details"]["token_accounting"], "partial")
        self.assertEqual(doc["details"]["cost_endpoint"]["total_llm_calls"], 0)
        self.assertNotIn("token_cap", doc["details"])  # only emitted when BENCH_MAX_TOKENS is set

    def test_success_with_existing_bench_branch_in_clone(self) -> None:
        # T4's runner creates a local branch named `bench`; a nested `bench/...`
        # ref would be a directory/file conflict with it.
        helpers.git(self.bench, "branch", "bench")
        fake = helpers.FakeAE(["intake", "implementation", "passed"])
        url = self.start(fake)
        sha = helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/abcd1234", {"x.txt": "x\n"})

        code = ae.run(self.environ(url))

        self.assertEqual(code, 0)
        doc = self.read_output()
        self.assertEqual(doc["head_sha"], sha)
        self.assertEqual(helpers.git(self.bench, "rev-parse", "--abbrev-ref", "HEAD"), "bench-autonomous-engine")
        self.assertIn("bench", helpers.git(self.bench, "branch", "--list", "bench"))  # the runner's branch survives

    def test_missing_fetch_remote_is_usage_error(self) -> None:
        env = self.environ("http://127.0.0.1:9", AE_FETCH_REMOTE="")
        with self.assertRaises(common.WrapperError) as ctx:
            ae.run(env)
        self.assertIn("AE_FETCH_REMOTE", str(ctx.exception))
        self.assertIn("fork", str(ctx.exception))

    def test_stateless_detail_is_failed_and_cancelled(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "research"], stateless=True)
        url = self.start(fake)
        started = time.monotonic()

        code = ae.run(self.environ(url, BENCH_TIME_LIMIT_S="20"))

        self.assertLess(time.monotonic() - started, 10.0)  # did not spin to the deadline
        self.assertEqual(code, common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIn("without item.state", doc["details"]["reason"])
        self.assertEqual(doc["details"]["malformed_polls"], ae.MAX_MALFORMED_POLLS)
        self.assertTrue(fake.cancelled)
        self.assertTrue(doc["details"]["cancelled_after_malformed_response"])

    def test_awaiting_user_approval_prompt_is_auto_answered(self) -> None:
        # The product's interaction surface parks PRD approval in awaiting_user with an
        # approval:prd prompt (cmd/product-agent). The wrapper must answer it, not report blocked.
        script = ["intake", "prd", "awaiting_user", "research", "planning", "implementation", "review", "awaiting_user", "qa", "passed"]
        approve_actions = [{"id": "approve", "label": "Approve", "semantic": "continue"}, {"id": "reject", "label": "Reject", "semantic": "cancel"}]
        fake = helpers.FakeAE(script, prompts={
            2: {"trigger": "approval:prd", "prompt_type": "approval", "headline": "PRD ready", "actions": approve_actions},
            7: {"trigger": "approval:human_review", "prompt_type": "approval", "headline": "Review ready", "actions": approve_actions},
        })
        url = self.start(fake)
        branch = f"agent/{fake.work_id}/abcd1234"
        sha = helpers.push_agent_branch(self.root, self.repo["remote"], branch, {"app.py": "def add(a, b):\n    return a + b\n\n\ndef subtract(a, b):\n    return a - b\n"})

        code = ae.run(self.environ(url))

        self.assertEqual(code, 0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "passed_agent")
        self.assertEqual(doc["head_sha"], sha)
        self.assertEqual([p["action"] for p in fake.answered_prompts], ["approve", "approve"])
        self.assertEqual([p["trigger"] for p in fake.answered_prompts], ["approval:prd", "approval:human_review"])
        self.assertEqual(fake.approvals, [])  # legacy gate endpoints were not needed
        answered = [a for a in doc["details"]["approvals"] if a.get("state") == "awaiting_user"]
        self.assertEqual([a["prompt_id"] for a in answered], ["prompt-2", "prompt-7"])
        self.assertNotIn("blocking_prompt", doc["details"])

    def test_awaiting_user_question_prompt_is_blocked(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "awaiting_user"], prompts={
            2: {"trigger": "agent_request", "prompt_type": "clarification", "headline": "Which escape table?", "actions": [{"id": "answer", "label": "Answer", "semantic": "ok"}]},
        })
        url = self.start(fake)

        code = ae.run(self.environ(url))

        self.assertEqual(code, common.EXIT_BLOCKED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "blocked")
        self.assertEqual(fake.answered_prompts, [])
        self.assertEqual(doc["details"]["blocking_prompt"]["trigger"], "agent_request")
        self.assertEqual(doc["details"]["blocking_prompt"]["headline"], "Which escape table?")
        self.assertFalse(fake.cancelled)

    def test_awaiting_user_without_any_prompt_is_blocked_after_grace(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "awaiting_user"])
        url = self.start(fake)

        code = ae.run(self.environ(url))

        self.assertEqual(code, common.EXIT_BLOCKED)
        self.assertEqual(self.read_output()["outcome"], "blocked")
        listed = [r for r in fake.requests if r["path"].startswith("/api/prompts")]
        self.assertGreaterEqual(len(listed), ae.AWAITING_USER_GRACE_POLLS)

    def test_blocked_exits_non_zero(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "prd_approval", "research", "blocked"], reasons={"blocked": "implementation failed: repo_env_broken"})
        url = self.start(fake)

        code = ae.run(self.environ(url))

        self.assertEqual(code, common.EXIT_BLOCKED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "blocked")
        self.assertIsNone(doc["head_sha"])
        self.assertIn("repo_env_broken", doc["details"]["reason"])
        self.assertEqual(doc["details"]["diff_base"], self.repo["base_sha"])
        self.assertEqual(doc["details"]["diff_head"], self.repo["base_sha"])
        self.assertEqual(doc["details"]["changed_files"], [])
        self.assertEqual(helpers.git(self.bench, "rev-parse", "HEAD"), self.repo["base_sha"])
        self.assertFalse(fake.cancelled)

    def test_awaiting_user_is_blocked(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "awaiting_user"])
        url = self.start(fake)
        self.assertEqual(ae.run(self.environ(url)), common.EXIT_BLOCKED)
        self.assertEqual(self.read_output()["outcome"], "blocked")

    def test_timeout_exits_non_zero_and_cancels(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "research"])  # never terminal
        url = self.start(fake)
        started = time.monotonic()

        code = ae.run(self.environ(url, BENCH_TIME_LIMIT_S="1"))

        elapsed = time.monotonic() - started
        self.assertEqual(code, common.EXIT_TIMEOUT)
        self.assertGreaterEqual(elapsed, 1.0)
        self.assertLess(elapsed, 15.0)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "timeout")
        self.assertGreaterEqual(doc["elapsed_s"], 1.0)
        self.assertTrue(fake.cancelled)
        self.assertTrue(doc["details"]["cancelled_after_timeout"])
        self.assertEqual(doc["details"]["final_state"], "research")

    def test_no_auto_approve_waits_at_gate_until_timeout(self) -> None:
        fake = helpers.FakeAE(["intake", "prd", "prd_approval", "research", "passed"])
        url = self.start(fake)
        code = ae.run(self.environ(url, BENCH_TIME_LIMIT_S="1", AE_AUTO_APPROVE="0"))
        self.assertEqual(code, common.EXIT_TIMEOUT)
        self.assertEqual(fake.approvals, [])
        self.assertEqual(self.read_output()["details"]["final_state"], "prd_approval")

    def test_failed_state_exits_one(self) -> None:
        for state in ("failed", "rejected", "cancelled"):
            with self.subTest(state=state):
                fake = helpers.FakeAE(["intake", "prd", state], reasons={state: "QA failed"})
                url = self.start(fake)
                try:
                    self.assertEqual(ae.run(self.environ(url)), common.EXIT_FAILED)
                    doc = self.read_output()
                    self.assertEqual(doc["outcome"], "failed")
                    self.assertIn(state, doc["details"]["reason"])
                finally:
                    self.server.shutdown()
                    self.server.server_close()
                    self.server = None

    def test_passed_without_branch_is_failed(self) -> None:
        fake = helpers.FakeAE(["intake", "passed"])
        url = self.start(fake)
        self.assertEqual(ae.run(self.environ(url)), common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["outcome"], "failed")
        self.assertIn("no agent/", doc["details"]["reason"])
        self.assertIsNone(doc["head_sha"])

    def test_head_not_containing_base_is_failed(self) -> None:
        fake = helpers.FakeAE(["intake", "passed"])
        url = self.start(fake)
        # An orphan branch that does not descend from the benchmark base.
        work = self.root / "orphan"
        subprocess.run(["git", "clone", "-q", str(self.repo["remote"]), str(work)], check=True)
        helpers.git(work, "checkout", "-q", "--orphan", f"agent/{fake.work_id}/00000000")
        (work / "other.txt").write_text("x\n", encoding="utf-8")
        helpers.git(work, "add", "-A")
        helpers.git(work, "commit", "-q", "-m", "orphan")
        helpers.git(work, "push", "-q", "origin", f"agent/{fake.work_id}/00000000")

        self.assertEqual(ae.run(self.environ(url)), common.EXIT_FAILED)
        doc = self.read_output()
        self.assertFalse(doc["details"]["base_is_ancestor"])
        self.assertIn("different base", doc["details"]["reason"])

    def test_repository_mismatch_is_failed(self) -> None:
        fake = helpers.FakeAE(["intake", "passed"], repository_id="repo-actual")
        url = self.start(fake)
        helpers.push_agent_branch(self.root, self.repo["remote"], f"agent/{fake.work_id}/abcd1234", {"x.txt": "x\n"})
        self.assertEqual(ae.run(self.environ(url, AE_REPOSITORY_ID="repo-expected")), common.EXIT_FAILED)
        self.assertIn("repo-actual", self.read_output()["details"]["reason"])

    def test_intake_rejected_is_failed(self) -> None:
        fake = helpers.FakeAE(FULL_SUCCESS)
        fake.reject_intake = True
        url = self.start(fake)
        self.assertEqual(ae.run(self.environ(url)), common.EXIT_FAILED)
        doc = self.read_output()
        self.assertEqual(doc["details"]["http_status"], 503)
        self.assertIn("not_onboarded", doc["details"]["reason"])

    def test_bad_token_is_failed_fast(self) -> None:
        fake = helpers.FakeAE(FULL_SUCCESS)
        url = self.start(fake)
        self.assertEqual(ae.run(self.environ(url, AE_API_TOKEN="wrong")), common.EXIT_FAILED)
        self.assertIn("AE_API_TOKEN", self.read_output()["details"]["reason"])

    def test_unreachable_dashboard_is_failed(self) -> None:
        fake = helpers.FakeAE(FULL_SUCCESS)
        url = self.start(fake)
        env = self.environ(url, AE_DASHBOARD_URL="http://127.0.0.1:9")  # discard port: refused
        self.assertEqual(ae.run(env), common.EXIT_FAILED)
        self.assertIn("unreachable", self.read_output()["details"]["reason"])

    # --- configuration errors ------------------------------------------------

    def test_missing_token_is_usage_error(self) -> None:
        env = self.environ("http://127.0.0.1:9", AE_API_TOKEN="")
        with self.assertRaises(common.WrapperError):
            ae.run(env)
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(ae.main([]), common.EXIT_USAGE)

    def test_missing_runner_vars_is_usage_error(self) -> None:
        env = self.environ("http://127.0.0.1:9")
        del env["BENCH_REPO"]
        with self.assertRaises(common.WrapperError):
            ae.run(env)

    def test_raw_dir_inside_repo_rejected(self) -> None:
        env = self.environ("http://127.0.0.1:9", BENCH_RAW_DIR=str(self.bench / "raw"))
        with self.assertRaises(common.WrapperError):
            ae.run(env)

    def test_help_and_show_config_via_entry_script(self) -> None:
        script = helpers.BENCHMARKS_DIR / "run-autonomous-engine.py"
        proc = subprocess.run([sys.executable, str(script), "--help"], capture_output=True, text=True, check=False)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        for name in ("AE_GATEWAY_URL", "AE_API_TOKEN", "AE_PRODUCT_ID", "AE_REPOSITORY_ID", "AE_FETCH_REMOTE", "BENCH_TIME_LIMIT_S", "BENCH_MAX_TOKENS", "LOWER BOUND"):
            self.assertIn(name, proc.stdout)
        env = dict(os.environ, AE_API_TOKEN="secret-token", AE_GATEWAY_URL="http://gw:1", AE_FETCH_REMOTE="https://example.invalid/fork.git")
        proc = subprocess.run([sys.executable, str(script), "--show-config"], capture_output=True, text=True, check=False, env=env)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertNotIn("secret-token", proc.stdout)
        self.assertIn("http://gw:1", proc.stdout)


class AECostTests(unittest.TestCase):
    """M4-05: cost_usd is the dashboard's priced cost, null whenever unknown."""

    def _result(self) -> common.Result:
        return common.Result(competitor="autonomous-engine", task_id="t", outcome="failed", elapsed_s=0.0, raw_ref="r")

    def test_priced_cost_is_reported(self) -> None:
        result = self._result()
        ae._apply_cost({"total_llm_calls": 3, "total_input_tokens": 10, "total_output_tokens": 5,
                        "cost_usd": 0.0123, "cost_unknown": False}, result)
        self.assertAlmostEqual(result.cost_usd, 0.0123)
        self.assertEqual(result.tokens_in, 10)

    def test_unknown_cost_stays_null(self) -> None:
        for body in (
            {"total_llm_calls": 3, "cost_usd": None, "cost_unknown": True},   # no prices yet
            {"total_llm_calls": 3, "cost_usd": 0.5, "cost_unknown": True},    # contradictory: trust the flag
            {"total_llm_calls": 3, "total_input_tokens": 1},                  # pre-M4-05 dashboard
            {"total_llm_calls": 0, "cost_usd": 0.0, "cost_unknown": False},   # nothing tagged = unknown
            {"total_llm_calls": 3, "cost_usd": True, "cost_unknown": False},  # not a number
        ):
            result = self._result()
            ae._apply_cost(body, result)
            self.assertIsNone(result.cost_usd, body)


if __name__ == "__main__":
    unittest.main()
