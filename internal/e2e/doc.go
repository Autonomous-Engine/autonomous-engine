// Package e2e holds end-to-end fixtures that drive the running Docker stack
// through Kafka and observe the outcome in PostgreSQL and on the bus. It has
// no production code; every test is env-gated (TEST_POSTGRES_DSN and
// TEST_KAFKA_BROKERS) and skips cleanly in an ordinary `go test ./...` run.
//
// The fixtures deliberately target the live `autonomous_engine` database
// because that is the database the services under test read: a message
// published to the real Kafka is only observable through the rows the real
// consumers write. Every row a fixture creates is named with the
// fixturePrefix and deleted again at the end of the test (and any leftover
// from a crashed run is swept at the start).
//
// # Preconditions
//
// The stack must be quiet while the fixture runs. Two assertions depend on
// it:
//
//   - The legacy-fallback counter (ae_evidence_compat_legacy_fallback_total)
//     on status-orchestrator, pr-coordinator and implementation-planner is
//     asserted to move by exactly +1 for a message without evidence in the
//     legacy window and by 0 otherwise. A real pre-evidence producer
//     completing a task during the run moves it too and fails the case.
//   - Exactly one pending_pr_changes row per fixture parent is expected.
//     Nothing else should write rows for fixture work items, but a second
//     pr-coordinator replica or a replayed topic would.
//
// The consumers must have been started with the EVIDENCE_COMPAT_WINDOW named
// in E2E_COMPAT_WINDOW and the HUMAN_REVIEW_REQUIRED named in
// E2E_HUMAN_REVIEW_REQUIRED; the fixture cannot read the containers' env and
// trusts these two variables to pick the applicable cases and expectations.
// Fixture repositories have no clone, so any qa.ready the orchestrator emits
// for a fixture parent is answered by qa-agent blocking the item ("Cannot run
// QA: no repository available"); the fixture tolerates and records that.
//
// # Side effect of the synthetic pr.merged — dev stacks only
//
// The QA leg publishes a synthetic pr.merged for each reviewed parent.
// Besides the transition under test, status-orchestrator unconditionally
// publishes codex.updated{trigger: post_merge, since: HEAD~1} for it, and
// knowledge-indexer answers that with a post-merge pull and an incremental
// index on the default clone path. The fixture omits repository_id so no
// github-service pull is requested, but on a stack whose default clone is a
// populated repository the incremental index writes live codex and Qdrant
// rows that carry no fixture prefix and that the sweep cannot remove. Run
// the fixture only against a development stack whose codex you are willing
// to re-index.
package e2e
