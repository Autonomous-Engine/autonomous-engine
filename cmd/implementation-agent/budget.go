package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
)

// D4 execution budgets for one implementation attempt (M3-06):
//
//   - tokens: the work item's cumulative input+output tokens, capped at
//     WORK_ITEM_TOKEN_BUDGET (default 500,000) — the same ceiling and the
//     same count RunToolLoop enforces (tools.TokenBudget);
//   - active time: wall time since the attempt started, minus every
//     interval during which a user prompt this attempt raised was pending,
//     capped at IMPLEMENTATION_ATTEMPT_ACTIVE_MINUTES (default 60).
//
// Both are checked before every LLM call of every tool loop (the
// executor's iteration hook) and inside the repair loop before each check
// run and each repair. Exhaustion is sticky: once a cap is reached the
// attempt stops, its work is committed to the agent branch unpushed, and
// finalizeStopped records failed evidence and blocks the item with an
// escalation prompt (never partial work as success).
//

const (
	budgetKindTokens     = "tokens"
	budgetKindActiveTime = "active_time"

	// activeMinutesEnv overrides the active-time cap (minutes, > 0).
	activeMinutesEnv     = "IMPLEMENTATION_ATTEMPT_ACTIVE_MINUTES"
	defaultActiveMinutes = 60.0
	// budgetPersistEvery throttles detail.budget writes from the
	// iteration hook.
	budgetPersistEvery = 30 * time.Second
	// maxRecordedPauses bounds detail.budget.pauses.
	maxRecordedPauses = 20
)

var (
	budgetExhaustedTokens = metrics.NewLabeledCounter("ae_budget_exhausted_total", "Implementation attempts stopped because the work item's implementation budget was exhausted", "kind", budgetKindTokens)
	budgetExhaustedActive = metrics.NewLabeledCounter("ae_budget_exhausted_total", "Implementation attempts stopped because the work item's implementation budget was exhausted", "kind", budgetKindActiveTime)
)

// budgetExhaustedError is the error the iteration hook returns once a cap
// is reached; RunToolLoop returns it wrapped.
type budgetExhaustedError struct {
	kind   string
	detail string
}

func (e *budgetExhaustedError) Error() string {
	return "budget_exhausted: " + e.kind + " " + e.detail
}

// pauseRecord is one interval excluded from active time.
type pauseRecord struct {
	PromptID string    `json:"prompt_id"`
	Trigger  string    `json:"trigger,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to,omitempty"`
	Seconds  float64   `json:"seconds"`
}

// budgetState is implementation_runs.detail.budget.
type budgetState struct {
	TokenLimit         int64         `json:"token_limit"`
	TokensUsed         int64         `json:"tokens_used"`
	ActiveLimitSeconds float64       `json:"active_limit_seconds"`
	ActiveSeconds      float64       `json:"active_seconds"`
	PausedSeconds      float64       `json:"paused_seconds"`
	Paused             bool          `json:"paused"`
	Pauses             []pauseRecord `json:"pauses,omitempty"`
	Exhausted          string        `json:"exhausted,omitempty"`
	ExhaustedDetail    string        `json:"exhausted_detail,omitempty"`
	StartedAt          time.Time     `json:"started_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// attemptBudget tracks one attempt's D4 budgets. Every method is safe for
// concurrent use and a no-op (never exhausted) on a nil receiver.
type attemptBudget struct {
	now         func() time.Time
	tokenLimit  int64
	activeLimit time.Duration
	run         *controller.Run
	logger      *logging.Logger
	taskID      string

	mu          sync.Mutex
	started     time.Time
	paused      time.Duration
	pausedSince *time.Time
	pauses      []pauseRecord
	tokenBase   int64
	tokenLast   int64
	exhausted   *budgetExhaustedError
	lastPersist time.Time
}

// newAttemptBudget starts the attempt's clock now. now is time.Now in
// production; tests pass a fake clock.
func newAttemptBudget(now func() time.Time, run *controller.Run, logger *logging.Logger, taskID string) *attemptBudget {
	if now == nil {
		now = time.Now
	}
	return &attemptBudget{
		now: now, tokenLimit: tools.TokenBudget(), activeLimit: activeTimeLimit(),
		run: run, logger: logger, taskID: taskID, started: now(),
	}
}

// activeTimeLimit reads IMPLEMENTATION_ATTEMPT_ACTIVE_MINUTES (default 60).
func activeTimeLimit() time.Duration {
	m := defaultActiveMinutes
	if s := os.Getenv(activeMinutesEnv); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			m = v
		}
	}
	return time.Duration(m * float64(time.Minute))
}

// promptPending is the executor's prompt hook: a user prompt is pending
// from now, so the active clock pauses.
func (b *attemptBudget) promptPending(promptID uuid.UUID, trigger interactions.Trigger) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.pausedSince == nil {
		t := b.now()
		b.pausedSince = &t
		b.pauses = append(b.pauses, pauseRecord{PromptID: promptID.String(), Trigger: string(trigger), From: t})
	}
	b.mu.Unlock()
	b.info("budget: active clock paused while a user prompt is pending", map[string]interface{}{"prompt_id": promptID.String(), "trigger": string(trigger)})
	b.persist(context.Background(), true)
}

// activity marks the attempt active again (an LLM call or a check run is
// about to happen): a pending prompt's pause ends here. The agent does not
// wait in-process for an answer, so the pause covers exactly the time
// nothing ran because a prompt was pending.
func (b *attemptBudget) activity() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumeLocked()
}

func (b *attemptBudget) resumeLocked() {
	if b.pausedSince == nil {
		return
	}
	t := b.now()
	d := t.Sub(*b.pausedSince)
	if d < 0 {
		d = 0
	}
	b.paused += d
	if n := len(b.pauses); n > 0 && b.pauses[n-1].To.IsZero() {
		b.pauses[n-1].To = t
		b.pauses[n-1].Seconds = d.Seconds()
	}
	if len(b.pauses) > maxRecordedPauses {
		b.pauses = b.pauses[len(b.pauses)-maxRecordedPauses:]
	}
	b.pausedSince = nil
}

// activeLocked is the active time so far: wall time minus paused time
// (including a pause still in progress).
func (b *attemptBudget) activeLocked() (active, paused time.Duration) {
	now := b.now()
	paused = b.paused
	if b.pausedSince != nil {
		paused += now.Sub(*b.pausedSince)
	}
	active = now.Sub(b.started) - paused
	if active < 0 {
		active = 0
	}
	return active, paused
}

// observeTokens records a cumulative token count from a tool loop. With a
// database the count is the work item's total (monotonic); without one
// each loop restarts at zero, so a decrease starts a new segment.
func (b *attemptBudget) observeTokens(cumulative int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cumulative < b.tokenLast {
		b.tokenBase += b.tokenLast
	}
	b.tokenLast = cumulative
}

func (b *attemptBudget) tokensLocked() int64 { return b.tokenBase + b.tokenLast }

// check returns the exhaustion error once either cap is reached (sticky),
// nil otherwise.
func (b *attemptBudget) check() *budgetExhaustedError {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.exhausted != nil {
		e := b.exhausted
		b.mu.Unlock()
		return e
	}
	var e *budgetExhaustedError
	active, paused := b.activeLocked()
	if used := b.tokensLocked(); b.tokenLimit > 0 && used >= b.tokenLimit {
		e = &budgetExhaustedError{kind: budgetKindTokens, detail: fmt.Sprintf("%d of %d tokens used", used, b.tokenLimit)}
	} else if b.activeLimit > 0 && active >= b.activeLimit {
		e = &budgetExhaustedError{kind: budgetKindActiveTime, detail: fmt.Sprintf("%s of %s active (%s awaiting user input excluded)",
			active.Round(time.Second), b.activeLimit, paused.Round(time.Second))}
	}
	if e != nil {
		b.exhausted = e
	}
	b.mu.Unlock()
	if e != nil {
		b.markExhausted(e)
	}
	return e
}

// exhaust records an exhaustion observed elsewhere (RunToolLoop's own
// token check). The first exhaustion wins.
func (b *attemptBudget) exhaust(e *budgetExhaustedError) *budgetExhaustedError {
	if b == nil {
		return e
	}
	b.mu.Lock()
	if b.exhausted != nil {
		e = b.exhausted
		b.mu.Unlock()
		return e
	}
	b.exhausted = e
	b.mu.Unlock()
	b.markExhausted(e)
	return e
}

func (b *attemptBudget) markExhausted(e *budgetExhaustedError) {
	if e.kind == budgetKindTokens {
		budgetExhaustedTokens.Inc()
	} else {
		budgetExhaustedActive.Inc()
	}
	b.warn("budget exhausted — stopping the attempt", map[string]interface{}{"kind": e.kind, "detail": e.detail})
	b.persist(context.Background(), true)
}

// exhaustion returns the recorded exhaustion, nil when none.
func (b *attemptBudget) exhaustion() *budgetExhaustedError {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exhausted
}

// fromLoopError maps a tool loop's error to an exhaustion: the hook's own
// error, or RunToolLoop's token-budget error. nil when err is not one.
func (b *attemptBudget) fromLoopError(err error) *budgetExhaustedError {
	if err == nil {
		return nil
	}
	var be *budgetExhaustedError
	if errors.As(err, &be) {
		return b.exhaust(be)
	}
	if errors.Is(err, tools.ErrTokenBudgetExceeded) {
		return b.exhaust(&budgetExhaustedError{kind: budgetKindTokens, detail: err.Error()})
	}
	return nil
}

// iterationHook is the executor's IterationHook: the attempt is active,
// the work item's token count is current, and both caps are checked
// before the next LLM call.
func (b *attemptBudget) iterationHook(ctx context.Context, cumulativeTokens, _ int64) error {
	if b == nil {
		return nil
	}
	b.activity()
	b.observeTokens(cumulativeTokens)
	if e := b.check(); e != nil {
		return e
	}
	b.persist(ctx, false)
	return nil
}

// snapshot is the budget's state for detail.budget.
func (b *attemptBudget) snapshot() budgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	active, paused := b.activeLocked()
	s := budgetState{
		TokenLimit: b.tokenLimit, TokensUsed: b.tokensLocked(),
		ActiveLimitSeconds: b.activeLimit.Seconds(), ActiveSeconds: active.Seconds(), PausedSeconds: paused.Seconds(),
		Paused: b.pausedSince != nil, Pauses: append([]pauseRecord(nil), b.pauses...),
		StartedAt: b.started, UpdatedAt: b.now(),
	}
	if b.exhausted != nil {
		s.Exhausted, s.ExhaustedDetail = b.exhausted.kind, b.exhausted.detail
	}
	return s
}

// persist writes detail.budget (throttled unless force).
func (b *attemptBudget) persist(ctx context.Context, force bool) {
	if b == nil || b.run == nil {
		return
	}
	b.mu.Lock()
	now := b.now()
	due := force || b.lastPersist.IsZero() || now.Sub(b.lastPersist) >= budgetPersistEvery
	if due {
		b.lastPersist = now
	}
	b.mu.Unlock()
	if due {
		b.run.MergeDetail(ctx, "budget", b.snapshot())
	}
}

func (b *attemptBudget) info(msg string, fields map[string]interface{}) {
	if b.logger == nil {
		return
	}
	fields["task_id"] = b.taskID
	b.logger.Info(context.Background(), msg, fields)
}

func (b *attemptBudget) warn(msg string, fields map[string]interface{}) {
	if b.logger == nil {
		return
	}
	fields["task_id"] = b.taskID
	b.logger.Warn(context.Background(), msg, fields)
}
