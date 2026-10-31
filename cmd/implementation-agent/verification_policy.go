package main

import (
	"context"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/google/uuid"
)

// policyTestCommand applies the repository verification policy's
// test_command override (M3-03) to the detected command for the generation
// phase, so the model's prompt and the pre-commit test run use the same
// command the final gate will run. Any failure to resolve the policy keeps
// the detected command; finalization resolves the policy again, fail
// closed, before the gate (applyVerificationPolicy).
func policyTestCommand(ctx context.Context, logger *logging.Logger, src verificationPolicySource, repositoryID, detected string) string {
	repoID, err := uuid.Parse(strings.TrimSpace(repositoryID))
	if err != nil || src == nil {
		return detected
	}
	p, _, loadErr := src.LoadVerificationPolicy(ctx, repoID)
	if loadErr != nil {
		if logger != nil {
			logger.Warn(ctx, "verification policy lookup failed — using the detected test command for generation", map[string]interface{}{
				"repository_id": repositoryID,
				"err":           loadErr.Error(),
			})
		}
		return detected
	}
	cmd := p.EffectiveTestCommand(detected)
	if cmd != strings.TrimSpace(detected) && logger != nil {
		logger.Info(ctx, "verification policy overrides the detected test command", map[string]interface{}{
			"repository_id":     repositoryID,
			"detected_command":  detected,
			"effective_command": cmd,
			"policy_source":     string(p.Source),
		})
	}
	return cmd
}
