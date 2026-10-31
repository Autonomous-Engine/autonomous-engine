#!/usr/bin/env python3
"""Offline unit tests for benchmarks/verify-fixtures.py (no network).

    python -m unittest benchmarks/test_verify_fixtures.py
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
import unittest
from pathlib import Path
from unittest import mock

VERIFIER = Path(__file__).resolve().parent / "verify-fixtures.py"
_spec = importlib.util.spec_from_file_location("verify_fixtures", VERIFIER)
vf = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(vf)


def git(repo: Path, *args: str) -> str:
    return subprocess.check_output(
        ["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@example.com", *args], text=True
    ).strip()


class RepoUrlCanonicalisation(unittest.TestCase):
    def test_canonical_strips_slash_and_git_suffix(self) -> None:
        self.assertEqual(vf.canonical_repo_url("https://github.com/joho/godotenv.git"), "https://github.com/joho/godotenv")
        self.assertEqual(vf.canonical_repo_url("https://github.com/joho/godotenv/"), "https://github.com/joho/godotenv")
        self.assertEqual(vf.canonical_repo_url(" https://github.com/joho/godotenv.git/ "), "https://github.com/joho/godotenv")
        self.assertEqual(vf.canonical_repo_url("https://github.com/joho/godotenv"), "https://github.com/joho/godotenv")

    def test_pairing_ignores_case_slash_and_git_suffix(self) -> None:
        canonical = "https://github.com/joho/godotenv"
        for variant in ("https://github.com/joho/godotenv.git", "https://github.com/joho/godotenv/",
                        "https://GitHub.com/Joho/GoDotEnv", "https://github.com/joho/godotenv.git/"):
            self.assertTrue(vf.same_repo(variant, canonical), variant)
        self.assertFalse(vf.same_repo("https://github.com/joho/godotenv", "https://github.com/joho/godotenv-fork"))
        self.assertFalse(vf.same_repo("https://github.com/joho/godotenv", "https://github.com/hashicorp/go-version"))

    def test_non_canonical_repo_is_rejected_by_validate(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-canon-") as tmp:
            task_dir = Path(tmp) / "example-task"
            (task_dir / "hidden-tests").mkdir(parents=True)
            (task_dir / "prompt.md").write_text("# Task\n", encoding="utf-8")
            (task_dir / "hidden-tests" / "bench_x_test.go").write_text("package x\n", encoding="utf-8")
            fixture = {
                "id": "example-task",
                "repo": "https://github.com/joho/godotenv.git",
                "base_sha": "0" * 40,
                "language": "go",
                "task_class": "historical_defect",
                "difficulty": "easy",
                "size_stratum": "small",
                "test_command": "go test ./...",
                "partition": "sealed",
                "expected_outcome": "pass",
                "fail_mode": "assert",
                "reference": {"steps": [{"commit": "1" * 40, "paths": ["x.go"]}]},
            }
            (task_dir / "fixture.json").write_text(json.dumps(fixture), encoding="utf-8")
            with self.assertRaises(vf.FixtureError) as ctx:
                vf.validate_fixture(task_dir, {})
            self.assertIn("not canonical", str(ctx.exception))


def _task(tmp: str, **overrides) -> Path:
    task_dir = Path(tmp) / "example-task"
    (task_dir / "hidden-tests").mkdir(parents=True)
    (task_dir / "prompt.md").write_text("# Task\n", encoding="utf-8")
    (task_dir / "hidden-tests" / "bench_x_test.go").write_text("package x\n", encoding="utf-8")
    fixture = {
        "id": "example-task", "repo": "https://github.com/joho/godotenv", "base_sha": "0" * 40,
        "language": "go", "task_class": "historical_defect", "difficulty": "easy", "size_stratum": "small",
        "test_command": "go test ./...", "partition": "sealed", "expected_outcome": "pass", "fail_mode": "assert",
        "reference": {"steps": [{"commit": "1" * 40, "paths": ["x.go"]}]},
    }
    fixture.update(overrides)
    fixture = {k: v for k, v in fixture.items() if v is not None}
    (task_dir / "fixture.json").write_text(json.dumps(fixture), encoding="utf-8")
    return task_dir


class ReferenceRecipe(unittest.TestCase):
    """The corpus ships no upstream code: the reference is a recipe, never a patch file."""

    def test_shipped_reference_directory_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-ref-") as tmp:
            task_dir = _task(tmp)
            (task_dir / "reference").mkdir()
            (task_dir / "reference" / "fix.patch").write_text("diff --git a/x b/x\n", encoding="utf-8")
            with self.assertRaises(vf.FixtureError) as ctx:
                vf.validate_fixture(task_dir, {})
            self.assertIn("ships no upstream code", str(ctx.exception))

    def test_missing_recipe_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-ref-") as tmp:
            with self.assertRaises(vf.FixtureError) as ctx:
                vf.validate_fixture(_task(tmp, reference=None), {})
            self.assertIn("reference.steps", str(ctx.exception))

    def test_bad_commit_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-ref-") as tmp:
            bad = {"steps": [{"commit": "abc", "paths": ["x.go"]}]}
            with self.assertRaises(vf.FixtureError) as ctx:
                vf.validate_fixture(_task(tmp, reference=bad), {})
            self.assertIn("40-hex", str(ctx.exception))

    def test_drop_hunks_removes_only_matching_hunks(self) -> None:
        diff = (
            "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n"
            "@@ -1 +1 @@\n-old\n+new\n"
            "@@ -9,0 +9,2 @@\n+func TestDropMe(t *testing.T) {\n+}\n"
            "diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n"
            "@@ -1,0 +1 @@\n+func TestDropMe2() {}\n"
        )
        out = vf.drop_hunks(diff, ["TestDropMe"])
        self.assertIn("+new", out)
        self.assertNotIn("TestDropMe", out)
        self.assertNotIn("b.go", out)
        self.assertEqual(vf.drop_hunks(diff, []), diff)

    def test_reference_patch_renders_from_upstream_commits(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-render-") as tmp:
            up = Path(tmp) / "upstream"
            subprocess.run(["git", "init", "-q", str(up)], check=True)
            git(up, "config", "uploadpack.allowAnySHA1InWant", "true")
            (up / "a.txt").write_text("one\n", encoding="utf-8")
            (up / "b.txt").write_text("keep\n", encoding="utf-8")
            git(up, "add", ".")
            git(up, "commit", "-qm", "base")
            base = git(up, "rev-parse", "HEAD")
            (up / "a.txt").write_text("one\ntwo\n", encoding="utf-8")
            (up / "b.txt").write_text("changed\n", encoding="utf-8")
            git(up, "commit", "-qam", "fix")
            fix = git(up, "rev-parse", "HEAD")
            fixture = {"id": "render-task", "repo": up.as_uri(), "base_sha": base,
                       "reference": {"steps": [{"commit": fix, "paths": ["a.txt"]}]}}
            env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
            patch = vf.reference_patch(fixture, Path(tmp) / "work", env).decode()
            self.assertIn("+two", patch)
            self.assertNotIn("b.txt", patch)


class CrossFixtureLeakPairing(unittest.TestCase):
    """The leak check must pair `.../repo.git` with `.../repo` (offline: local repo)."""

    def test_git_suffix_variant_still_pairs_and_detects_leak(self) -> None:
        with tempfile.TemporaryDirectory(prefix="vf-leak-") as tmp:
            base = Path(tmp)
            clone = base / "clone"
            clone.mkdir()
            subprocess.run(["git", "init", "-q", str(clone)], check=True)
            (clone / "README.md").write_text("line one\nline two\n", encoding="utf-8")
            git(clone, "add", "README.md")
            git(clone, "commit", "-qm", "base that already contains the other fixture's fix")
            sha = git(clone, "rev-parse", "HEAD")

            # "Other" fixture whose reference patch adds `line two` (already present in this base).
            other_patch = (
                "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n"
                "@@ -1 +1,2 @@\n line one\n+line two\n"
            ).encode("utf-8")
            this = {"id": "this-fixture", "repo": "https://github.com/example/repo", "base_sha": sha}
            other = {"id": "other-fixture", "repo": "https://github.com/example/repo.git", "_patch": other_patch}
            env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
            with self.assertRaises(vf.FixtureError) as ctx:
                vf.cross_fixture_leak_check(this, [other], clone, base, env)
            self.assertIn("cross-fixture leak", str(ctx.exception))

            # Same content but a different repository never pairs.
            unrelated = dict(other, repo="https://github.com/example/another")
            self.assertEqual(vf.cross_fixture_leak_check(this, [unrelated], clone, base, env), "clean")

            # A written exception downgrades the finding to a note.
            excepted = dict(this, reverse_apply_exceptions={"other-fixture": "content is not an answer"})
            self.assertEqual(
                vf.cross_fixture_leak_check(excepted, [other], clone, base, env),
                "excepted:other-fixture(README.md)",
            )


def _fx(lang: str, cls: str, *, partition: str = "sealed", difficulty: str = "easy", size: str = "small") -> dict:
    return {
        "language": lang,
        "task_class": cls,
        "partition": partition,
        "difficulty": difficulty,
        "size_stratum": size,
        "expected_outcome": "blocked" if cls == "blocked_or_underspecified" else "pass",
    }


SMALL_TARGETS = {
    "languages": {
        "go": {
            "min_total": 4,
            "min_per_class": {"historical_defect": 2, "blocked_or_underspecified": 1},
            "min_blocked": 1,
            "min_sealed_fraction": 0.5,
            "min_per_difficulty": {"hard": 1},
            "min_non_small": 1,
        }
    },
    "overall": {"min_total": 4},
}


def _good_go_corpus() -> list[dict]:
    return [
        _fx("go", "historical_defect", difficulty="hard"),
        _fx("go", "historical_defect", size="medium"),
        _fx("go", "small_feature", partition="development"),
        _fx("go", "blocked_or_underspecified"),
    ]


class CorpusTargets(unittest.TestCase):
    def test_shipped_targets_file_is_valid(self) -> None:
        targets = vf.load_targets()
        self.assertIn("go", targets["languages"])

    def test_meeting_targets_reports_no_problems(self) -> None:
        self.assertEqual(vf.check_targets(_good_go_corpus(), SMALL_TARGETS), [])

    def test_each_target_is_enforced_per_language(self) -> None:
        corpus = [
            _fx("go", "historical_defect", partition="development"),
            _fx("go", "small_feature", partition="development"),
            _fx("go", "small_feature"),
        ]
        problems = "\n".join(vf.check_targets(corpus, SMALL_TARGETS))
        for needle in (
            "go total: 3 (minimum 4)",
            "go historical_defect: 1 (minimum 2)",
            "go blocked_or_underspecified: 0 (minimum 1)",
            "go blocked: 0 (minimum 1)",
            "go sealed fraction: 1/3",
            "go difficulty hard: 0 (minimum 1)",
            "go medium/large size: 0 (minimum 1)",
            "overall total: 3 (minimum 4)",
        ):
            self.assertIn(needle, problems)

    def test_language_without_targets_fails(self) -> None:
        corpus = _good_go_corpus() + [_fx("typescript", "small_feature")]
        problems = vf.check_targets(corpus, SMALL_TARGETS)
        self.assertEqual(len(problems), 1)
        self.assertIn("typescript", problems[0])

    def test_targeted_language_with_no_fixtures_fails(self) -> None:
        targets = {"languages": dict(SMALL_TARGETS["languages"], typescript={"min_total": 1}), "overall": {}}
        problems = vf.check_targets(_good_go_corpus(), targets)
        self.assertEqual(problems, ["typescript total: 0 (minimum 1)"])

    def test_overall_targets_span_languages(self) -> None:
        targets = {
            "languages": {"go": {}, "typescript": {}},
            "overall": {"min_per_class": {"small_feature": 2}, "min_sealed_fraction": 0.9},
        }
        corpus = [_fx("go", "small_feature"), _fx("typescript", "small_feature", partition="development")]
        problems = vf.check_targets(corpus, targets)
        self.assertEqual(len(problems), 1)
        self.assertIn("overall sealed fraction: 1/2", problems[0])

    def test_invalid_targets_file_is_rejected(self) -> None:
        bad_files = (
            {"languages": {}},
            {"languages": {"go": {"min_total": -1}}},
            {"languages": {"go": {"min_per_class": {"bug": 1}}}},
            {"languages": {"go": {"min_sealed_fraction": 1.5}}},
            {"languages": {"go": {"max_total": 3}}},
        )
        with tempfile.TemporaryDirectory(prefix="vf-targets-") as tmp:
            path = Path(tmp) / "corpus-targets.json"
            for data in bad_files:
                path.write_text(json.dumps(data), encoding="utf-8")
                with self.assertRaises(vf.FixtureError, msg=json.dumps(data)):
                    vf.load_targets(path)

    def test_only_skips_corpus_check_and_says_so(self) -> None:
        task_id = sorted(p.name for p in vf.TASKS_DIR.iterdir() if p.is_dir())[0]

        def fake_verify_one(*_args, **_kwargs):
            return {"baseline": "PASS", "before": "FAIL(ok)", "after": "PASS(ok)", "fail_mode": "assert(ok)"}

        def must_not_run(*_args, **_kwargs):
            raise AssertionError("corpus targets were checked under --only")

        out = io.StringIO()
        with mock.patch.object(vf, "verify_one", fake_verify_one), \
                mock.patch.object(vf, "check_targets", must_not_run), \
                mock.patch.object(sys, "argv", ["verify-fixtures.py", "--only", task_id]), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = vf.main()
        self.assertEqual(code, 0, out.getvalue())
        self.assertIn("corpus targets: SKIPPED", out.getvalue())


class SharedTimeLimit(unittest.TestCase):
    def test_verifier_uses_runner_time_limit(self) -> None:
        self.assertEqual(vf.RUNNER.time_limit_seconds(900), 840)
        self.assertEqual(vf.RUNNER.time_limit_seconds(40), vf.RUNNER.MIN_TIME_LIMIT_S)


if __name__ == "__main__":
    unittest.main()
