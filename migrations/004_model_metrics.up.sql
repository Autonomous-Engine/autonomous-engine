-- 004_model_metrics.up.sql
-- Model capability profiles, inference logs, and pipeline metrics.

CREATE TYPE operating_mode AS ENUM (
    'constrained',
    'standard',
    'advanced'
);

-- Capability profiles for LLM model variants.
CREATE TABLE model_profiles (
    id                       UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    provider                 TEXT           NOT NULL DEFAULT '',
    model_id                 TEXT           NOT NULL DEFAULT '',
    effective_context_window INT            NOT NULL DEFAULT 0,
    instruction_following    INT            NOT NULL DEFAULT 0,
    code_generation          INT            NOT NULL DEFAULT 0,
    code_understanding       INT            NOT NULL DEFAULT 0,
    reasoning                INT            NOT NULL DEFAULT 0,
    synthesis                INT            NOT NULL DEFAULT 0,
    avg_inference_speed      DOUBLE PRECISION NOT NULL DEFAULT 0,
    concurrent_capacity      INT            NOT NULL DEFAULT 1,
    operating_mode           operating_mode NOT NULL DEFAULT 'standard',
    evaluated_at             TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

-- Per-request inference log for cost tracking and debugging.
CREATE TABLE inference_logs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_provider TEXT        NOT NULL DEFAULT '',
    model_id       TEXT        NOT NULL DEFAULT '',
    intent_type    TEXT        NOT NULL DEFAULT '',
    input_tokens   INT         NOT NULL DEFAULT 0,
    output_tokens  INT         NOT NULL DEFAULT 0,
    latency_ms     BIGINT      NOT NULL DEFAULT 0,
    success        BOOLEAN     NOT NULL DEFAULT true,
    error_message  TEXT        NOT NULL DEFAULT '',
    agent_id       UUID,
    work_item_id   UUID        REFERENCES work_items(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Arbitrary numeric metrics emitted during pipeline execution.
CREATE TABLE pipeline_metrics (
    id           UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    work_item_id UUID             REFERENCES work_items(id) ON DELETE CASCADE,
    metric_type  TEXT             NOT NULL DEFAULT '',
    value        DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at  TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_inference_logs_created_at   ON inference_logs(created_at);
CREATE INDEX idx_inference_logs_agent_id     ON inference_logs(agent_id);
CREATE INDEX idx_inference_logs_intent_type  ON inference_logs(intent_type);

CREATE INDEX idx_pipeline_metrics_metric_type  ON pipeline_metrics(metric_type);
CREATE INDEX idx_pipeline_metrics_recorded_at  ON pipeline_metrics(recorded_at);
