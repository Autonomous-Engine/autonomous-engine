package stage

import (
	"encoding/json"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
)

// ArtifactKindVerifier is the "kind" of the verdict's review_comments
// artifact.
const ArtifactKindVerifier = "verifier"

// ArtifactContent is the verdict artifact (M3-07 shape, unchanged).
// decision/branch/head_sha/reviewed_sha keep the shape every review_comments
// reader already parses (status-orchestrator and dashboard-backend read the
// latest artifact's branch and head_sha; an "escalate" decision is the head
// a human approves out of human_review); defects is the {critical, major,
// minor} object work_item_outcomes.review_defects reads (M4-05); the full
// verdict, with the per-criterion results and the defect list, is under
// verdict. TaskID (R14) names the subtask whose head the verification
// stage verified; empty for a verdict review-agent computed on pr.ready.
type ArtifactContent struct {
	Kind                          string                `json:"kind"`
	Decision                      string                `json:"decision"`
	Gate                          verifier.Decision     `json:"gate"`
	Feedback                      string                `json:"feedback"`
	Branch                        string                `json:"branch"`
	HeadSHA                       string                `json:"head_sha"`
	ReviewedSHA                   string                `json:"reviewed_sha"`
	Defects                       verifier.DefectCounts `json:"defects"`
	VerifierModelSharedWithAuthor bool                  `json:"verifier_model_shared_with_author"`
	VerifierVersion               string                `json:"verifier_version"`
	Verdict                       verifier.Verdict      `json:"verdict"`
	TaskID                        string                `json:"task_id,omitempty"`
}

// NewArtifactContent builds the verdict artifact's content. decision is the
// artifact's "decision": "approve" only when that approval is the final
// outcome, approve_pending_second_pass when the second pass decides,
// otherwise the gate action (request_changes, escalate).
func NewArtifactContent(v verifier.Verdict, g verifier.Decision, decision, branch, verifiedSHA, reviewedSHA, taskID string) ArtifactContent {
	return ArtifactContent{
		Kind:                          ArtifactKindVerifier,
		Decision:                      decision,
		Gate:                          g,
		Feedback:                      verifier.RenderFeedback(v, g),
		Branch:                        branch,
		HeadSHA:                       verifiedSHA,
		ReviewedSHA:                   reviewedSHA,
		Defects:                       verifier.CountDefects(v.Defects),
		VerifierModelSharedWithAuthor: v.ModelInfo.SharedWithAuthor,
		VerifierVersion:               verifier.Version,
		Verdict:                       v,
		TaskID:                        taskID,
	}
}

// NewArtifact wraps content as the review_comments artifact of workID.
// createdAt is set now so an artifact written later in the same transaction
// sorts after it (readers take the latest review_comments row).
func NewArtifact(workID uuid.UUID, agentID string, c ArtifactContent) *models.WorkItemArtifact {
	data, err := json.Marshal(c)
	if err != nil {
		data = []byte(`{}`)
	}
	return &models.WorkItemArtifact{
		WorkItemID: workID,
		Type:       models.ArtifactTypeReviewComments,
		Content:    data,
		AgentID:    agentID,
		CreatedAt:  time.Now().UTC(),
	}
}
