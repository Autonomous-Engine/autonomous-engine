package main

import (
	"context"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
)

// falseSuccessInterval is how often the false-success job runs ("nightly").
const falseSuccessInterval = 24 * time.Hour

// runFalseSuccessJob runs outcomes.MarkFalseSuccess once at start and then
// every interval until ctx ends (M4-06). The statement is idempotent, so
// several orchestrator replicas running it is harmless; a failure is
// logged and retried at the next tick.
func runFalseSuccessJob(ctx context.Context, db outcomes.Execer, interval time.Duration) {
	run := func() {
		n, err := outcomes.MarkFalseSuccess(ctx, db)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("status-orchestrator: false-success job: %v", err)
			}
			return
		}
		if n > 0 {
			log.Printf("status-orchestrator: false-success job: marked %d verified item(s) reverted within %d days", n, outcomes.FalseSuccessWindowDays)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
