package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The implementation controller (M3-01/02). Each task attempt is a
// controller.Run persisted in implementation_runs: every executor callback
// is an event, and the run's state advances only when controller.Derive
// says the tool evidence warrants it. The operator-facing lifecycle lines
// ("controller: red_observed (go test ./... exit 1, call …)") are logged
// from the transitions; nothing logs a TDD step the tools did not show.

// startControllerRun creates this delivery's run: attempt N+1 for the task.
// A previous attempt that never reached a terminal state is logged with
// the state its tools last showed; the model conversation is not resumed
// (the implementation controller). It
// returns nil when the run cannot be recorded: the task proceeds without a
// controller rather than failing on telemetry.
func startControllerRun(ctx context.Context, store *controller.Store, logger *logging.Logger, workID, taskID uuid.UUID) *controller.Run {
	if store == nil {
		return nil
	}
	started, err := store.StartRun(ctx, workID, taskID.String())
	if err != nil {
		log.Printf("implementation-agent: controller: cannot record run for task_id=%s (continuing without one): %v", taskID, err)
		return nil
	}
	run := controller.NewRun(store, started, workID, taskID.String())
	run.Logf = log.Printf
	run.OnTransition = func(tr controller.Transition) {
		logger.Info(ctx, tr.LogLine(), map[string]interface{}{
			"task_id": taskID.String(), "run_id": run.ID.String(), "attempt": run.Attempt,
			"from": string(tr.From), "to": string(tr.To), "seq": tr.Seq,
		})
	}
	fields := map[string]interface{}{"task_id": taskID.String(), "run_id": started.ID.String(), "attempt": started.Attempt}
	if p := started.Previous; p != nil {
		fields["previous_run_id"], fields["previous_attempt"], fields["previous_state"] = p.ID, p.Attempt, string(p.State)
		logger.Info(ctx, fmt.Sprintf("controller: attempt %d starts; previous attempt %d ended in state %s", started.Attempt, p.Attempt, p.State), fields)
	} else {
		logger.Info(ctx, "controller: attempt 1 starts", fields)
	}
	return run
}

// newToolCallback is the executor callback: it records the call for the
// verify gate, feeds it to the controller, and streams it to the dashboard
// (tool.executed preview; console.stream with the run's phase after the
// call and run_command's exit code).
func newToolCallback(ctx context.Context, kb bus.MessageBus, workID, taskID uuid.UUID, run *controller.Run, budget *attemptBudget, turnToolCalls *[]strategy.ToolCall) tools.ToolCallback {
	// The executor runs read-only tool batches in parallel, so callbacks
	// can arrive concurrently; the append is serialised. (The verify gate
	// reads the slice only between tool loops.)
	var mu sync.Mutex
	return func(callID, toolName, arguments, result string, isError bool, meta tools.ResultMeta) {
		mu.Lock()
		*turnToolCalls = append(*turnToolCalls, strategy.ToolCall{Name: toolName, Args: arguments})
		mu.Unlock()
		// A tool ran: the attempt is active, so a pending prompt's pause
		// ends here (M3-06) — except for the request_user_input call that
		// raised the prompt.
		if toolName != "request_user_input" {
			budget.activity()
		}
		phase := run.Observe(ctx, controller.ToolEvent(callID, toolName, arguments, isError, meta.Command, meta.ExitCode))

		event := map[string]interface{}{
			"work_item_id":   workID.String(),
			"task_id":        taskID.String(),
			"tool":           toolName,
			"arguments":      arguments,
			"result_preview": truncate(result, 200),
			"is_error":       isError,
			"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		}
		payload, _ := json.Marshal(event)
		kb.Publish(ctx, bus.TopicToolExecuted, workID.String(), payload) //nolint:errcheck
		consoleEvent := map[string]interface{}{
			"work_item_id": workID.String(),
			"task_id":      taskID.String(),
			"agent_type":   "implementation-agent",
			"tool":         toolName,
			"arguments":    arguments,
			"result":       result,
			"is_error":     isError,
			"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
		}
		if phase != "" {
			consoleEvent["phase"] = string(phase)
		}
		if meta.ExitCode != nil {
			consoleEvent["exit_code"] = *meta.ExitCode
		}
		consolePayload, _ := json.Marshal(consoleEvent)
		kb.Publish(ctx, bus.TopicConsoleStream, workID.String(), consolePayload) //nolint:errcheck
	}
}

// outcomeRecorder records the run's terminal state inside the outcome
// transaction (outboxCompletionSink.Report).
type outcomeRecorder interface {
	recordOutcomeTx(ctx context.Context, tx pgx.Tx, o completionOutcome)
}

// outcomeHookFor returns the outcome hook for run, or nil without a run.
func outcomeHookFor(run *controller.Run, logger *logging.Logger) outcomeRecorder {
	if run == nil {
		return nil
	}
	return runOutcomeHook{run: run, logger: logger}
}

type runOutcomeHook struct {
	run    *controller.Run
	logger *logging.Logger
}

// recordOutcomeTx writes final_verified / blocked / failed (or a note that
// the run ended unverified) in the outcome's transaction, under a
// savepoint: a failed run write is rolled back alone and logged, and never
// costs the outcome itself (implementation.complete must still commit).
func (h runOutcomeHook) recordOutcomeTx(ctx context.Context, tx pgx.Tx, o completionOutcome) {
	outcome, note := finalOutcomeOf(o)
	sp, err := tx.Begin(ctx)
	if err != nil {
		log.Printf("implementation-agent: controller: run %s: savepoint for outcome: %v", h.run.ID, err)
		return
	}
	tr, err := h.run.FinishTx(ctx, sp, outcome, note)
	if err != nil {
		_ = sp.Rollback(ctx)
		log.Printf("implementation-agent: controller: run %s: record outcome %s: %v", h.run.ID, outcome, err)
		return
	}
	if err := sp.Commit(ctx); err != nil {
		log.Printf("implementation-agent: controller: run %s: release outcome savepoint: %v", h.run.ID, err)
		return
	}
	if tr != nil && h.logger != nil {
		h.logger.Info(ctx, tr.LogLine(), map[string]interface{}{
			"task_id": h.run.TaskID, "run_id": h.run.ID.String(), "attempt": h.run.Attempt,
			"from": string(tr.From), "to": string(tr.To), "seq": tr.Seq, "commits_with": "outcome",
		})
	}
}

// finalOutcomeOf maps an outcome to the controller's final event. Only a
// successful outcome whose evidence outcome is "verified" (D1: the only
// releasable outcome) yields FinalVerified.
func finalOutcomeOf(o completionOutcome) (controller.FinalOutcome, string) {
	ev := o.evidence
	if ev == nil && o.msg != nil {
		ev = o.msg.Evidence
	}
	switch {
	case o.blockReason != "":
		return controller.FinalBlocked, truncate(o.blockReason, 300)
	case o.msg != nil && o.msg.Success && ev != nil && ev.Outcome == evidence.OutcomeVerified:
		return controller.FinalVerified, ""
	case o.msg != nil && o.msg.Success:
		got := "none"
		if ev != nil {
			got = string(ev.Outcome)
		}
		return controller.FinalUnverified, "evidence outcome " + got
	case o.msg != nil:
		return controller.FinalFailed, truncate(o.msg.Reason, 300)
	}
	return controller.FinalFailed, "assignment " + string(o.assignment)
}

// runSummaryFields are the controller fields of the implementation summary
// artifact: the state generation ended in and the tool calls that proved
// red and green ("" when not observed).
func runSummaryFields(run *controller.Run) map[string]string {
	if run == nil {
		return map[string]string{}
	}
	d := run.Detail()
	out := map[string]string{
		"controller_run_id":  run.ID.String(),
		"controller_attempt": fmt.Sprintf("%d", run.Attempt),
		"controller_state":   string(run.State()),
		"red_call_id":        "",
		"green_call_id":      "",
	}
	if d.Red != nil {
		out["red_call_id"] = d.Red.CallID
	}
	if d.Green != nil {
		out["green_call_id"] = d.Green.CallID
	}
	return out
}
