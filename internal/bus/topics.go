package bus

// Kafka topic name constants used throughout the autonomous engine pipeline.
const (
	TopicIntake                 = "intake"
	TopicPRDReady               = "prd.ready"
	TopicResearchTasks          = "research.tasks"
	TopicResearchResults        = "research.results"
	TopicResearchAdditional     = "research.additional"
	TopicTechnicalBriefReady    = "technical-brief.ready"
	TopicImplementationTasks    = "implementation.tasks"
	TopicImplementationComplete = "implementation.complete"
	// TopicVerificationComplete (R14) is enqueued by the verification stage
	// (review-agent's second consumer group on implementation.complete) once
	// a subtask's verified head was judged by the independent verifier
	// before any PR exists. pr-coordinator opens the PR (or records the
	// pending change) only on an approve decision; status-orchestrator moves
	// the parent out of `verification`.
	TopicVerificationComplete   = "verification.complete"
	TopicPRReady                = "pr.ready"
	TopicPRGated                = "pr.gated"
	TopicReviewComplete         = "review.complete"
	TopicPRMerged               = "pr.merged"
	TopicQAReady                = "qa.ready"
	TopicQAResult               = "qa.result"
	TopicQABugs                 = "qa.bugs"
	TopicExternalChangeDetected = "external-change.detected"
	// TopicCodexUpdated is an index REQUEST: status-orchestrator after a
	// merge (post_merge), change-reconciler for an external change, the
	// webhook listener for a registered push, onboarding's run-analysis
	// trigger. Only knowledge-indexer consumes it. Since migration 060 the
	// indexer no longer announces its own completions here; see
	// TopicCodexIndexed.
	TopicCodexUpdated = "codex.updated"
	// TopicCodexIndexed (migration 060) announces one new codex version for a
	// product and branch. Every codex version writer enqueues it through the
	// outbox in the version row's transaction (codex.RecordVersionTx): the
	// knowledge-indexer (full and incremental index), onboarding approval and
	// the interaction consumers (answer promoted, answer revoked). Payload:
	// codex.IndexedEvent {product_id, branch, product_version,
	// version_number, sections_changed[], trigger}; sections_changed uses the
	// typed entries of internal/codex/sections.go. Aggregate "product", key
	// the product id, so one product's versions arrive in order. Consumers,
	// each in its own consumer group: status-orchestrator (group
	// status-orchestrator-codex) re-runs items refused on an empty codex whose
	// Re-index is pending, once the codex is grounded (codex coherence Part 3);
	// status-orchestrator's codex re-plan (group status-orchestrator-codex-replan)
	// re-plans pinned in-flight work (codex coherence Part 1).
	TopicCodexIndexed         = "codex.indexed"
	TopicWorkBlocked          = "work.blocked"
	TopicStateChanged         = "state.changed"
	TopicToolExecuted         = "tool.executed"
	TopicConsoleStream        = "console.stream"
	TopicChatMessage          = "chat.message"
	TopicChatResponse         = "chat.response"
	TopicChatProposalResolved = "chat.proposal.resolved"

	// User interaction surface — see
	TopicUserPromptRequested        = "user_prompt.requested"
	TopicUserPromptAnswered         = "user_prompt.answered"
	TopicUserPromptRevoked          = "user_prompt.revoked"
	TopicCodexUpdateFromInteraction = "codex.update.from_interaction"

	// TopicCodexGap is emitted when an agent falls back to a filesystem
	// tool (grep / list_files / read_file) AFTER a codex_* tool returned
	// zero results in the same tool loop. This is the explicit feedback
	// signal for "the codex didn't have an answer that grep did". Payload
	// carries a gap_id for deduplication. Consumer: status-orchestrator
	// (group status-orchestrator-codex, internal/codexgap) counts these
	// empty lookups per work item and run; a research or implementation
	// run with more than CODEX_GAP_THRESHOLD (default 5) is marked
	// ungrounded and the verification stage does not approve it
	// automatically (codex coherence Part 3). Planner-scope events
	// (reason "planner_missed_file") are ignored there.
	TopicCodexGap = "codex.gap"

	// Alerts from the internal alert service (metric threshold trips,
	// credential expiry). Consumed by dashboard-backend and rebroadcast
	// on the "alerts" WebSocket channel for the dashboard drift banner.
	TopicAlerts = "alerts"

	// Proactive onboarding (pre-fill). Published by the knowledge-indexer after
	// the PhasePrefill stage. Consumed by dashboard-backend to broadcast on the
	// "alerts" WebSocket channel so the wizard picks up progress + drift alerts.
	TopicOnboardingPrefillCompleted     = "onboarding.prefill.completed"
	TopicOnboardingPrefillDriftDetected = "onboarding.prefill.drift_detected"
	TopicOnboardingPrefillProgress      = "onboarding.prefill.progress"

	// Real-time indexing progress. Published by the knowledge-indexer at each
	// pipeline phase so the dashboard can show live activity. Consumed by
	// dashboard-backend and rebroadcast on the "indexing" WebSocket channel.
	TopicIndexingProgress = "indexing.progress"

	// Commit-linked codex: published by webhook-listener on watched-branch pushes.
	TopicCodexExtractRequested = "codex.extract.requested"

	// M4-06: pr-coordinator enqueues replay.complete for a replay (shadow)
	// work item that has no reviewable branch; status-orchestrator moves
	// the item to cancelled ("replay: evidence recorded"). A replay item
	// with a reviewable branch is terminated on its review.complete instead.
	TopicReplayComplete = "replay.complete"
)

// AllTopics lists every managed Kafka topic. Used for auto-creation at startup.
var AllTopics = []string{
	TopicIntake,
	TopicPRDReady,
	TopicResearchTasks,
	TopicResearchResults,
	TopicResearchAdditional,
	TopicTechnicalBriefReady,
	TopicImplementationTasks,
	TopicImplementationComplete,
	TopicVerificationComplete,
	TopicPRReady,
	TopicPRGated,
	TopicReviewComplete,
	TopicPRMerged,
	TopicQAReady,
	TopicQAResult,
	TopicQABugs,
	TopicExternalChangeDetected,
	TopicCodexUpdated,
	TopicCodexIndexed,
	TopicWorkBlocked,
	TopicStateChanged,
	TopicToolExecuted,
	TopicConsoleStream,
	TopicChatMessage,
	TopicChatResponse,
	TopicChatProposalResolved,
	TopicUserPromptRequested,
	TopicUserPromptAnswered,
	TopicUserPromptRevoked,
	TopicCodexUpdateFromInteraction,
	TopicCodexGap,
	TopicAlerts,
	TopicOnboardingPrefillCompleted,
	TopicOnboardingPrefillDriftDetected,
	TopicOnboardingPrefillProgress,
	TopicIndexingProgress,
	TopicCodexExtractRequested,
	TopicReplayComplete,
}
