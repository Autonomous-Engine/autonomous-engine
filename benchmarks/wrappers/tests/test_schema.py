from __future__ import annotations

import copy
import json
import unittest
from pathlib import Path

import helpers  # noqa: F401 - sys.path bootstrap

from wrappers import common
from wrappers.schema import SchemaError, assert_valid, load_schema, validate

TESTDATA = Path(__file__).resolve().parents[1] / "testdata"

VALID = {
    "schema_version": 1,
    "competitor": "claude-cli",
    "task_id": "sample-fix",
    "outcome": "passed_agent",
    "elapsed_s": 12.5,
    "head_sha": "0123456789abcdef0123456789abcdef01234567",
    "model_calls": 3,
    "tokens_in": 1110,
    "tokens_out": 42,
    "cost_usd": 0.1234,
    "versions": {"claude_cli": "2.1.267 (Claude Code)"},
    "raw_ref": "/tmp/bench-wrappers/claude-cli/sample-fix/x/wrapper.log",
    "details": {"anything": ["goes"]},
}


class SchemaTests(unittest.TestCase):
    def setUp(self) -> None:
        self.schema = load_schema()

    def test_valid_document(self) -> None:
        self.assertEqual(validate(VALID, self.schema), [])

    def test_nulls_mean_unknown(self) -> None:
        doc = dict(VALID, head_sha=None, model_calls=None, tokens_in=None, tokens_out=None, cost_usd=None)
        self.assertEqual(validate(doc, self.schema), [])

    def test_missing_required(self) -> None:
        doc = copy.deepcopy(VALID)
        del doc["raw_ref"]
        errors = validate(doc, self.schema)
        self.assertTrue(any("raw_ref" in e for e in errors), errors)

    def test_bad_outcome(self) -> None:
        doc = dict(VALID, outcome="passed")
        self.assertTrue(validate(doc, self.schema))

    def test_bad_sha(self) -> None:
        doc = dict(VALID, head_sha="abc")
        self.assertTrue(validate(doc, self.schema))

    def test_additional_property_rejected(self) -> None:
        doc = dict(VALID, extra=1)
        self.assertTrue(any("additional property" in e for e in validate(doc, self.schema)))

    def test_bool_is_not_integer(self) -> None:
        doc = dict(VALID, model_calls=True)
        self.assertTrue(validate(doc, self.schema))

    def test_negative_rejected(self) -> None:
        doc = dict(VALID, elapsed_s=-1)
        self.assertTrue(validate(doc, self.schema))

    def test_assert_valid_raises(self) -> None:
        with self.assertRaises(SchemaError):
            assert_valid(dict(VALID, competitor=""), self.schema)

    def test_result_to_dict_validates(self) -> None:
        for outcome in common.OUTCOMES:
            result = common.Result(competitor="x", task_id="t", outcome=outcome, elapsed_s=0.0, raw_ref="/x/log")
            self.assertEqual(validate(result.to_dict(), self.schema), [], outcome)
            self.assertEqual(result.exit_code, common.EXIT_FOR_OUTCOME[outcome])

    def test_sample_outputs_validate(self) -> None:
        samples = sorted(TESTDATA.glob("sample-output-*.json"))
        self.assertGreaterEqual(len(samples), 2)
        for path in samples:
            with self.subTest(sample=path.name):
                self.assertEqual(validate(json.loads(path.read_text(encoding="utf-8")), self.schema), [])


if __name__ == "__main__":
    unittest.main()
