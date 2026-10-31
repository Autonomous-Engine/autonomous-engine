#!/usr/bin/env python3
"""Unit tests for scripts/benchmark-report.py (offline, standard library only).

    python -m unittest scripts/test_benchmark_report.py
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / "benchmark-report.py"
_spec = importlib.util.spec_from_file_location("benchmark_report", SCRIPT)
report = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(report)

TESTDATA = HERE / "testdata"
SAMPLE_MANIFEST = TESTDATA / "sample-manifest.json"
SAMPLE_JSON = TESTDATA / "sample-results.json"
SAMPLE_JSONL = TESTDATA / "sample-results.jsonl"

CAND, BASE = "autonomous-engine", "claude-cli"


def manifest_for(task_ids: list[str], blocked: tuple[str, ...] = ()) -> dict:
    return {
        "tasks": [
            {
                "id": t,
                "expected_outcome": "blocked" if t in blocked else "pass",
                "language": "go",
                "task_class": "blocked_or_underspecified" if t in blocked else "historical_defect",
                "difficulty": "easy",
                "size_stratum": "small",
                "partition": "sealed",
            }
            for t in task_ids
        ]
    }


def record(task: str, competitor: str, passed: bool, trial: int = 0, **extra) -> dict:
    rec = {
        "run_id": "run-1",
        "task_id": task,
        "competitor": competitor,
        "trial": trial,
        "passed": passed,
        "false_success": False,
        "cost_usd": 0.5,
        "tokens_in": 100,
        "tokens_out": 10,
        "model_calls": 3,
        "agent": {"wall_seconds": 60.0, "exit_code": 0 if passed else 1},
        "role": "candidate" if competitor == CAND else "baseline",
    }
    rec.update(extra)
    return rec


def paired_records(candidate: list[bool], baseline: list[bool], trials: int = 1) -> tuple[list[dict], dict]:
    """Task i gets candidate[i] and baseline[i], repeated for every trial."""
    ids = [f"t{i}" for i in range(len(candidate))]
    recs = []
    for trial in range(trials):
        recs += [record(t, CAND, c, trial) for t, c in zip(ids, candidate)]
        recs += [record(t, BASE, b, trial) for t, b in zip(ids, baseline)]
    return recs, manifest_for(ids)


def summarize(recs: list[dict], manifest: dict, **kwargs) -> dict:
    kwargs.setdefault("resamples", 2000)
    return report.build_summary(recs, manifest, [], candidate=CAND, baseline=BASE, ablation=kwargs.pop("ablation", False), **kwargs)


class WilsonTest(unittest.TestCase):
    def test_known_values(self) -> None:
        cases = {
            (5, 10): (0.2366, 0.7634),
            (0, 10): (0.0, 0.2775),
            (10, 10): (0.7225, 1.0),
            (8, 9): (0.5650, 0.9801),
            (1, 3): (0.0615, 0.7923),
        }
        for (k, n), (low, high) in cases.items():
            got = report.wilson_interval(k, n)
            self.assertAlmostEqual(got[0], low, places=4, msg=(k, n))
            self.assertAlmostEqual(got[1], high, places=4, msg=(k, n))

    def test_no_trials(self) -> None:
        self.assertIsNone(report.wilson_interval(0, 0))


class BootstrapTest(unittest.TestCase):
    def test_deterministic_under_seed(self) -> None:
        diffs = [1.0, 0.0, -1.0, 0.5, 0.0, 1.0, 0.0, 0.0]
        a = report.paired_bootstrap(diffs, 3000, seed=7)
        b = report.paired_bootstrap(diffs, 3000, seed=7)
        c = report.paired_bootstrap(diffs, 3000, seed=8)
        self.assertEqual(a, b)
        self.assertNotEqual(a, c)
        self.assertLessEqual(a[0], sum(diffs) / len(diffs))
        self.assertGreaterEqual(a[1], sum(diffs) / len(diffs))

    def test_constant_differences_give_degenerate_interval(self) -> None:
        self.assertEqual(report.paired_bootstrap([0.25] * 6, 500, seed=1), (0.25, 0.25))

    def test_seed_and_resamples_reported(self) -> None:
        recs, manifest = paired_records([True, False], [True, True])
        summary = summarize(recs, manifest, seed=99, resamples=1234)
        self.assertEqual(summary["paired"]["seed"], 99)
        self.assertEqual(summary["paired"]["resamples"], 1234)
        self.assertIn("seed 99", report.render_markdown(summary))


class GateVerdictTest(unittest.TestCase):
    def test_pure_verdict_function(self) -> None:
        verdict = report.pass_rate_verdict
        self.assertEqual(verdict(-5.0, 3.0, 3), (report.VERDICT_PASS, None))
        self.assertEqual(verdict(1.0, 9.0, 3), (report.VERDICT_PASS, None), "excludes 0: credible")
        self.assertEqual(verdict(-4.0, 6.0, 3), (report.VERDICT_PASS, None), "contains 0 but only 10 pp wide")
        self.assertEqual(verdict(-5.1, 4.0, 5), (report.VERDICT_BLOCK, None))
        self.assertEqual(verdict(-30.0, -10.0, 5), (report.VERDICT_BLOCK, None))
        # D7 literally: lower bound below -5 blocks even when the interval also spans +5.
        self.assertEqual(verdict(-12.0, 12.0, 5), (report.VERDICT_BLOCK, report.INCONCLUSIVE_NOTE))
        # Lower bound acceptable but not credible: wide interval containing 0, or too few trials.
        self.assertEqual(verdict(-4.9, 20.0, 5), (report.VERDICT_MORE_TRIALS, report.INCONCLUSIVE_NOTE))
        self.assertEqual(verdict(0.0, 0.0, 2), (report.VERDICT_MORE_TRIALS, report.INCONCLUSIVE_NOTE))
        self.assertEqual(verdict(0.0, 0.0, 3), (report.VERDICT_PASS, None))

    def test_block_on_clear_regression(self) -> None:
        recs, manifest = paired_records([False] * 10, [True] * 10, trials=3)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_BLOCK)
        self.assertIsNone(summary["paired"]["note"])
        self.assertEqual(summary["overall_verdict"], report.VERDICT_BLOCK)

    def test_block_when_lower_bound_fails_even_if_interval_spans_plus_five(self) -> None:
        recs, manifest = paired_records([True, False, True, False], [False, True, False, True], trials=3)
        summary = summarize(recs, manifest)
        low, high = summary["paired"]["ci95_pp"]
        self.assertLess(low, -5)
        self.assertGreater(high, 5)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_BLOCK)
        self.assertEqual(summary["paired"]["note"], report.INCONCLUSIVE_NOTE)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_BLOCK)
        markdown = report.render_markdown(summary)
        self.assertIn("interval inconclusive; more trials needed before any approval", markdown)

    def test_pass_when_no_regression(self) -> None:
        recs, manifest = paired_records([True] * 10, [True] * 10, trials=3)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["paired"]["ci95_pp"], [0.0, 0.0])
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_PASS)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_PASS)

    def test_too_few_trials_needs_more_trials(self) -> None:
        recs, manifest = paired_records([True] * 10, [True] * 10, trials=2)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["paired"]["min_trials_per_task"], 2)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_MORE_TRIALS)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_MORE_TRIALS)

    def test_wide_interval_containing_zero_needs_more_trials(self) -> None:
        # Lower bound stays >= -5 but the interval contains 0 and is wider than 10 pp.
        recs, manifest = paired_records([True] * 9, [True] * 8 + [False], trials=3)
        summary = summarize(recs, manifest)
        low, high = summary["paired"]["ci95_pp"]
        self.assertGreaterEqual(low, -5)
        self.assertLessEqual(low, 0)
        self.assertGreater(high - low, 10)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_MORE_TRIALS)
        markdown = report.render_markdown(summary)
        self.assertIn("MORE TRIALS NEEDED", markdown)
        self.assertIn("more trials needed before any approval", markdown)
        self.assertNotIn("approved", markdown.lower())

    def test_no_paired_tasks_is_not_a_pass(self) -> None:
        recs = [record("t0", CAND, True)]
        summary = summarize(recs, manifest_for(["t0"]))
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_MORE_TRIALS)
        self.assertEqual(summary["paired"]["unpaired_tasks"], ["t0"])

    def test_trials_are_averaged_per_task(self) -> None:
        recs = [
            record("t0", CAND, True, 0), record("t0", CAND, False, 1), record("t0", CAND, True, 2),
            record("t0", BASE, False, 0), record("t0", BASE, False, 1), record("t0", BASE, True, 2),
        ]
        summary = summarize(recs, manifest_for(["t0"]))
        row = summary["paired"]["tasks"][0]
        self.assertEqual((row["candidate_passes"], row["candidate_trials"]), (2, 3))
        self.assertEqual((row["baseline_passes"], row["baseline_trials"]), (1, 3))
        self.assertAlmostEqual(summary["paired"]["mean_difference_pp"], 100 / 3)


class BlockedExclusionTest(unittest.TestCase):
    def test_blocked_tasks_excluded_from_head_to_head(self) -> None:
        manifest = manifest_for(["t0", "blk"], blocked=("blk",))
        recs = [
            record("t0", CAND, True), record("t0", BASE, True),
            record("blk", CAND, True, wrapper_outcome="blocked"),
            record("blk", BASE, False, wrapper_outcome="passed_agent", false_success=True),
        ]
        summary = summarize(recs, manifest)
        self.assertEqual(summary["competitors"][BASE]["pass_rate"]["attempts"], 1)
        self.assertEqual(summary["competitors"][BASE]["pass_rate"]["passes"], 1)
        self.assertEqual(summary["paired"]["paired_tasks"], 1)
        self.assertEqual([r["task_id"] for r in summary["paired"]["tasks"]], ["t0"])
        blocked = {row["competitor"]: row for row in summary["blocked"]}
        self.assertEqual(blocked[CAND]["correct_blocks"], 1)
        self.assertEqual(blocked[BASE]["false_success"], 1)
        self.assertEqual(blocked[BASE]["unsafe_edit_on_blocked"], 0)
        # Expected-outcome stratum still shows both, raw counts included.
        self.assertEqual(summary["strata"]["expected_outcome"]["blocked"][BASE]["attempts"], 1)

    def test_expected_outcome_joined_from_manifest_for_legacy_records(self) -> None:
        manifest = manifest_for(["t0", "blk"], blocked=("blk",))
        legacy = [record("blk", CAND, True), record("t0", CAND, True), record("t0", BASE, False)]
        for rec in legacy:
            rec.pop("run_id")
        summary = summarize(legacy, manifest)
        self.assertEqual(summary["competitors"][CAND]["pass_rate"]["attempts"], 1)
        self.assertEqual(len(summary["blocked"]), 1)


class FalseSuccessTest(unittest.TestCase):
    def test_candidate_false_success_blocks_even_with_good_pass_rate(self) -> None:
        recs, manifest = paired_records([True] * 10, [False] * 10, trials=3)
        recs[0]["false_success"] = True
        summary = summarize(recs, manifest)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_PASS)
        self.assertEqual(summary["competitors"][CAND]["false_success"], 1)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_BLOCK)

    def test_baseline_false_success_does_not_block(self) -> None:
        recs, manifest = paired_records([True] * 10, [True] * 10, trials=3)
        recs[-1]["false_success"] = True
        summary = summarize(recs, manifest)
        self.assertEqual(summary["competitors"][BASE]["false_success"], 1)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_PASS)

    def test_derived_for_records_without_the_field(self) -> None:
        manifest = manifest_for(["t0", "blk"], blocked=("blk",))
        claimed = record("t0", CAND, False, wrapper_outcome="passed_agent", tests={"exit_code": 1})
        honest = record("t0", BASE, False, wrapper_outcome="failed", tests={"exit_code": 1})
        guessed = record("blk", BASE, False, wrapper_outcome="passed_agent", tracked_tree_unchanged=False)
        edited = record("blk", CAND, False, wrapper_outcome="failed", tracked_tree_unchanged=False)
        for rec in (claimed, honest, guessed, edited):
            del rec["false_success"]
        summary = summarize([claimed, honest, guessed, edited], manifest)
        self.assertEqual(summary["competitors"][CAND]["false_success"], 1)
        self.assertEqual(summary["competitors"][CAND]["unsafe_edit_on_blocked"], 1)
        self.assertEqual(summary["competitors"][BASE]["false_success"], 1)
        self.assertEqual(summary["competitors"][BASE]["unsafe_edit_on_blocked"], 0)

    def test_unsafe_edit_on_blocked_is_reported_but_does_not_gate(self) -> None:
        recs, manifest = paired_records([True] * 10, [True] * 10, trials=3)
        manifest["tasks"].append({**manifest["tasks"][0], "id": "blk", "expected_outcome": "blocked"})
        recs.append(record("blk", CAND, False, wrapper_outcome="timeout", tracked_tree_unchanged=False,
                           unsafe_edit_on_blocked=True))
        recs.append(record("blk", BASE, False, wrapper_outcome="failed", tracked_tree_unchanged=True))
        summary = summarize(recs, manifest)
        self.assertEqual(summary["competitors"][CAND]["false_success"], 0)
        self.assertEqual(summary["competitors"][CAND]["unsafe_edit_on_blocked"], 1)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_PASS)
        markdown = report.render_markdown(summary)
        self.assertIn("unsafe edits on blocked tasks", markdown)
        self.assertIn("| unsafe edits |", markdown)


class CostTest(unittest.TestCase):
    def test_cost_unknown_propagates(self) -> None:
        recs, manifest = paired_records([True, True, False], [True, False, False])
        recs[1]["cost_usd"] = None  # one candidate trial without cost
        summary = summarize(recs, manifest)
        eff = summary["competitors"][CAND]["efficiency"]
        self.assertIsNone(eff["cost_per_accepted_usd"])
        self.assertIsNone(eff["cost_total_usd_all"])
        self.assertEqual(eff["cost_note"], "cost unknown")
        base = summary["competitors"][BASE]["efficiency"]
        # Sum over ALL attempts (3 x 0.5) divided by accepted attempts (1).
        self.assertAlmostEqual(base["cost_per_accepted_usd"], 1.5)
        self.assertIn("cost unknown", report.render_markdown(summary))

    def test_unknown_tokens_are_not_zero(self) -> None:
        recs, manifest = paired_records([True, True], [True, True])
        recs[0]["tokens_in"] = None
        summary = summarize(recs, manifest)
        eff = summary["competitors"][CAND]["efficiency"]
        self.assertIsNone(eff["tokens_in_total_all"])
        self.assertEqual(eff["median_tokens_in_passing"], 100)

    def test_medians_over_passing_attempts_only(self) -> None:
        recs, manifest = paired_records([True, False], [True, True])
        recs[1]["agent"]["wall_seconds"] = 9999.0  # the failing candidate attempt
        summary = summarize(recs, manifest)
        self.assertEqual(summary["competitors"][CAND]["efficiency"]["median_wall_s_passing"], 60.0)

    def test_no_accepted_attempts(self) -> None:
        recs, manifest = paired_records([False], [True])
        summary = summarize(recs, manifest)
        self.assertEqual(summary["competitors"][CAND]["efficiency"]["cost_note"], "no accepted attempts")


class LabelTest(unittest.TestCase):
    def test_primary_labels(self) -> None:
        recs, manifest = paired_records([True], [True])
        summary = summarize(recs, manifest)
        self.assertEqual(summary["title"], "Autonomous Engine (full product) versus Claude CLI")
        self.assertIn("not a model-versus-model comparison", report.render_markdown(summary))

    def test_ablation_requires_metadata(self) -> None:
        recs, manifest = paired_records([True], [True])
        with self.assertRaises(ValueError):
            summarize(recs, manifest, ablation=True)
        for rec in recs:
            rec["comparison"] = "ablation"
        with self.assertRaises(ValueError):
            summarize(recs, manifest)
        summary = summarize(recs, manifest, ablation=True)
        self.assertEqual(summary["labels"][BASE], "Autonomous Engine without Codex/orchestration (same model)")

    def test_rollout_comparison_labels_both_sides_as_configurations(self) -> None:
        recs, manifest = paired_records([True, False], [True, True], trials=3)
        for rec in recs:
            rec["comparison"] = "rollout"
        with self.assertRaises(ValueError):
            summarize(recs, manifest, ablation=True)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["comparison"], "rollout")
        self.assertEqual(summary["labels"], {CAND: "Candidate configuration", BASE: "Production configuration"})
        self.assertEqual(summary["title"], "Candidate configuration versus Production configuration")
        markdown = report.render_markdown(summary)
        self.assertIn("Rollout shadow evaluation", markdown)
        self.assertNotIn("Claude CLI", markdown)


class InputParityTest(unittest.TestCase):
    def run_cli(self, *paths: Path, extra: tuple[str, ...] = ()) -> tuple[int, str, dict]:
        with tempfile.TemporaryDirectory() as tmp:
            out_json = Path(tmp) / "summary.json"
            buffer = io.StringIO()
            with contextlib.redirect_stdout(buffer):
                code = report.main(["--manifest", str(SAMPLE_MANIFEST), *map(str, paths), "--json", str(out_json), *extra])
            summary = json.loads(out_json.read_text(encoding="utf-8")) if out_json.exists() else {}
        return code, buffer.getvalue(), summary

    def test_json_and_jsonl_give_identical_reports(self) -> None:
        code_a, md_a, sum_a = self.run_cli(SAMPLE_JSON)
        code_b, md_b, sum_b = self.run_cli(SAMPLE_JSONL)
        code_c, md_c, sum_c = self.run_cli(SAMPLE_JSON, SAMPLE_JSONL)  # duplicates de-duplicated
        self.assertEqual((code_a, code_b, code_c), (0, 0, 0))
        self.assertEqual(md_a, md_b)
        self.assertEqual(md_a, md_c)
        self.assertEqual(sum_a, sum_b)
        self.assertEqual(sum_a["attempts"], 36)

    def test_sample_fixture_expectations(self) -> None:
        _, markdown, summary = self.run_cli(SAMPLE_JSON)
        self.assertEqual(summary["competitors"][CAND]["pass_rate"]["passes"], 12)
        self.assertEqual(summary["competitors"][BASE]["pass_rate"]["passes"], 9)
        self.assertEqual(summary["competitors"][CAND]["efficiency"]["cost_note"], "cost unknown")
        self.assertEqual(summary["competitors"][BASE]["false_success"], 3)
        self.assertEqual(summary["competitors"][BASE]["unsafe_edit_on_blocked"], 1)
        self.assertIn("## Blocked / underspecified tasks", markdown)
        self.assertIn("seed 20261003", markdown)

    def test_ablation_flag_refused_for_primary_run(self) -> None:
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            code, _, _ = self.run_cli(SAMPLE_JSON, extra=("--ablation",))
        self.assertEqual(code, 2)
        self.assertIn("ablation", stderr.getvalue())


class ReviewRoundTest(unittest.TestCase):
    """Minors from the M4-03 review."""

    def test_block_reason_mentions_too_few_trials(self) -> None:
        recs, manifest = paired_records([False] * 10, [True] * 10, trials=1)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["paired"]["verdict"], report.VERDICT_BLOCK)
        self.assertIn("fewest trials per task is 1 (< 3)", summary["paired"]["reason"])
        recs, manifest = paired_records([False] * 10, [True] * 10, trials=3)
        self.assertNotIn("fewest trials", summarize(recs, manifest)["paired"]["reason"])

    def blocked_case(self, blocked_cost):
        recs, manifest = paired_records([True, False], [True, True], trials=3)
        manifest["tasks"].append({**manifest["tasks"][0], "id": "blk", "expected_outcome": "blocked"})
        recs.append(record("blk", CAND, True, wrapper_outcome="blocked", cost_usd=blocked_cost))
        recs.append(record("blk", BASE, False, wrapper_outcome="passed_agent", false_success=True, cost_usd=blocked_cost))
        return summarize(recs, manifest)

    def test_cost_unknown_when_only_a_blocked_trial_lacks_cost(self) -> None:
        eff = self.blocked_case(None)["competitors"][CAND]["efficiency"]
        self.assertEqual(eff["cost_note"], "cost unknown")
        self.assertIsNone(eff["cost_total_usd_all"])

    def test_blocked_spend_in_total_but_never_accepted(self) -> None:
        summary = self.blocked_case(2.0)
        eff = summary["competitors"][CAND]["efficiency"]
        self.assertEqual(eff["accepted"], 3, "the correct block is not an accepted attempt")
        self.assertEqual(eff["attempts"], 6)
        self.assertEqual(eff["attempts_all"], 7)
        self.assertAlmostEqual(eff["cost_total_usd_all"], 6 * 0.5 + 2.0)
        self.assertAlmostEqual(eff["cost_per_accepted_usd"], 5.0 / 3)
        self.assertIn("blocked attempts never count as accepted", report.render_markdown(summary))

    def with_partitions(self, dev_false: bool, sealed_false: bool) -> dict:
        recs, manifest = paired_records([True] * 6, [True] * 6, trials=3)
        manifest["tasks"][0]["partition"] = "development"
        for r in recs:
            if r["competitor"] == CAND and r["trial"] == 0:
                if r["task_id"] == "t0":
                    r["false_success"] = dev_false
                if r["task_id"] == "t1":
                    r["false_success"] = sealed_false
        return recs, manifest

    def test_development_false_success_reported_but_does_not_gate(self) -> None:
        recs, manifest = self.with_partitions(dev_false=True, sealed_false=False)
        summary = summarize(recs, manifest)
        cand = summary["competitors"][CAND]
        self.assertEqual((cand["false_success"], cand["false_success_sealed"], cand["false_success_development"]), (1, 0, 1))
        self.assertEqual(summary["gates"][0]["verdict"], report.VERDICT_PASS)
        self.assertEqual(summary["overall_verdict"], report.VERDICT_PASS)
        self.assertIn("Development partition (does not gate): 1", report.render_markdown(summary))

    def test_sealed_false_success_gates(self) -> None:
        recs, manifest = self.with_partitions(dev_false=False, sealed_false=True)
        summary = summarize(recs, manifest)
        self.assertEqual(summary["gates"][0]["gate"], "false_success_sealed")
        self.assertEqual(summary["overall_verdict"], report.VERDICT_BLOCK)

    def test_unknown_partition_false_success_gates_as_sealed(self) -> None:
        for partition in (None, "holdout"):
            recs, manifest = paired_records([True] * 6, [True] * 6, trials=3)
            manifest["tasks"][2]["partition"] = partition
            if partition is None:
                del manifest["tasks"][2]["partition"]
            for r in recs:
                if r["competitor"] == CAND and r["task_id"] == "t2" and r["trial"] == 1:
                    r["false_success"] = True
            for filt in (None, "sealed"):
                summary = summarize(recs, manifest, partition=filt)
                self.assertEqual(summary["gates"][0]["verdict"], report.VERDICT_BLOCK, (partition, filt))
                self.assertEqual(summary["overall_verdict"], report.VERDICT_BLOCK)
                self.assertIn("gated as sealed (fail closed)", summary["gates"][0]["reason"])
                self.assertEqual(
                    summary["candidate_false_success_unknown_partition"],
                    [{"task_id": "t2", "trial": 1, "run_id": "run-1", "partition": partition}],
                )
                markdown = report.render_markdown(summary)
                self.assertIn("candidate false success on task `t2` trial 1", markdown)

    def test_partition_filter_sealed(self) -> None:
        recs, manifest = self.with_partitions(dev_false=True, sealed_false=False)
        summary = summarize(recs, manifest, partition="sealed")
        self.assertEqual(summary["partition_filter"], "sealed")
        self.assertEqual(summary["competitors"][CAND]["false_success"], 0)
        self.assertNotIn("t0", [row["task_id"] for row in summary["paired"]["tasks"]])

    def test_truncated_final_jsonl_line_is_tolerated(self) -> None:
        lines = SAMPLE_JSONL.read_text(encoding="utf-8").splitlines()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "killed.jsonl"
            path.write_text("\n".join(lines[:-1] + [lines[-1][:40]]) + "\n", encoding="utf-8")
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                records, _ = report.load_results([path])
            self.assertEqual(len(records), len(lines) - 1)
            self.assertIn("ignoring unparseable final line", stderr.getvalue())

    def test_malformed_middle_jsonl_line_fails(self) -> None:
        lines = SAMPLE_JSONL.read_text(encoding="utf-8").splitlines()
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "bad.jsonl"
            path.write_text("\n".join([lines[0], "{not json", *lines[1:]]) + "\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                report.load_results([path])
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr), contextlib.redirect_stdout(io.StringIO()):
                code = report.main(["--manifest", str(SAMPLE_MANIFEST), str(path)])
            self.assertEqual(code, 2)
            self.assertIn(":2: invalid JSON line", stderr.getvalue())

    def test_mode_specific_sentences(self) -> None:
        recs, manifest = paired_records([True], [True], trials=3)
        manifest["tasks"].append({**manifest["tasks"][0], "id": "blk", "expected_outcome": "blocked"})
        recs.append(record("blk", BASE, False, wrapper_outcome="passed_agent"))
        primary = report.render_markdown(summarize(recs, manifest))
        self.assertIn("not a model-versus-model comparison", primary)
        self.assertIn("today the Claude CLI wrapper", primary)
        for r in recs:
            r["comparison"] = "ablation"
        ablation = report.render_markdown(summarize(recs, manifest, ablation=True))
        self.assertNotIn("model-versus-model", ablation)
        self.assertNotIn("Claude CLI", ablation)
        self.assertIn("same underlying model", ablation)
        self.assertIn("A competitor without an escalation channel cannot pass", ablation)


if __name__ == "__main__":
    unittest.main()
