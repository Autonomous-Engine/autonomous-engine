#!/usr/bin/env bash
# smoke-proactive-onboarding.sh
# End-to-end smoke test for the proactive onboarding feature. Requires the
# stack running and a product with at least one cloned repo present.
#
# Usage: ./scripts/smoke-proactive-onboarding.sh <product_id>
#
# Asserts:
#   1. The stack is up and the onboarding service responds on port 3321
#   2. Triggering a reindex produces candidate rows for the given product
#   3. Every one of the 15 canonical question_keys has a row (no silent skips)
#   4. The /api/onboarding/candidates endpoint returns them as JSON
#   5. Reviewing a candidate flips its user_status in the DB
#
# Fail Honestly: any missing assertion exits non-zero with a real message.

set -euo pipefail

PRODUCT_ID="${1:-26b55430-f5cf-46c7-b655-295f073bce05}"
DASHBOARD="${DASHBOARD_URL:-http://localhost:8080}"
ONBOARDING="${ONBOARDING_URL:-http://localhost:3321}"
PG_EXEC="docker compose exec -T postgres psql -U ae_user -d autonomous_engine"

MARK() { printf "\n=== %s ===\n" "$*"; }
FAIL() { printf "\nFAIL: %s\n" "$*"; exit 1; }

MARK "1. Stack health"
curl -sf "$ONBOARDING/health" >/dev/null || FAIL "onboarding service not reachable at $ONBOARDING"

MARK "2. Login to dashboard (admin)"
TOKEN=$(curl -s -X POST "$DASHBOARD/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@localhost","password":"bf5223f7b8f4e8d9"}' \
  | grep -Eo '"token":"[^"]+"' | cut -d'"' -f4)
test -n "$TOKEN" || FAIL "login did not return a token"

MARK "3. Trigger reindex for $PRODUCT_ID"
curl -sf -X POST "$DASHBOARD/api/admin/products/reindex?product_id=$PRODUCT_ID" \
  -H "Authorization: Bearer $TOKEN" >/dev/null

MARK "4. Wait up to 60s for indexing + prefill to complete"
for i in $(seq 1 12); do
  COUNT=$($PG_EXEC -tAc "SELECT COUNT(*) FROM onboarding_candidates WHERE product_id='$PRODUCT_ID'" | tr -d ' \r\n')
  if [ "${COUNT:-0}" -ge 15 ]; then
    echo "candidates count = $COUNT (after $((i*5))s)"
    break
  fi
  sleep 5
done
test "${COUNT:-0}" -ge 15 || FAIL "expected 15 candidates, got $COUNT after 60s"

MARK "5. Verify every canonical question_key has a row"
EXPECTED="anti_patterns architecture_overview autonomous_scope branching_strategy critical_constraints cross_repo_imports deployment_process domain_knowledge patterns_discovered pr_process review_triggers risk_zones tech_stack test_conventions testing_philosophy"
for KEY in $EXPECTED; do
  ROW=$($PG_EXEC -tAc "SELECT user_status FROM onboarding_candidates WHERE product_id='$PRODUCT_ID' AND question_key='$KEY'" | tr -d ' \r\n')
  test -n "$ROW" || FAIL "missing row for question_key=$KEY"
done
echo "all 15 question_keys present"

MARK "6. Fetch candidates via /api/onboarding/candidates"
BODY=$(curl -sf "$ONBOARDING/api/onboarding/candidates?product_id=$PRODUCT_ID")
echo "$BODY" | grep -q '"candidates":' || FAIL "endpoint did not return candidates array"

MARK "7. Review one candidate (tech_stack → confirmed)"
curl -sf -X POST "$ONBOARDING/api/onboarding/candidates/review" \
  -H "Content-Type: application/json" \
  -d "{\"product_id\":\"$PRODUCT_ID\",\"question_key\":\"tech_stack\",\"status\":\"confirmed\"}" >/dev/null

STATUS=$($PG_EXEC -tAc "SELECT user_status FROM onboarding_candidates WHERE product_id='$PRODUCT_ID' AND question_key='tech_stack'" | tr -d ' \r\n')
test "$STATUS" = "confirmed" || FAIL "review did not flip user_status; got '$STATUS'"
echo "tech_stack user_status: confirmed"

MARK "8. Regenerate endpoint"
curl -sf -X POST "$ONBOARDING/api/onboarding/candidates/regenerate?product_id=$PRODUCT_ID" >/dev/null
echo "regenerate accepted"

MARK "ALL ASSERTIONS PASSED"
