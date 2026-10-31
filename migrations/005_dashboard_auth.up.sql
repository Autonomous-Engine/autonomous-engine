-- 005_dashboard_auth.up.sql
-- Dashboard authentication: users, sessions, alert configuration and history.

CREATE TYPE user_role AS ENUM (
    'viewer',
    'operator',
    'admin'
);

-- Dashboard user accounts.
CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL DEFAULT '',
    role          user_role   NOT NULL DEFAULT 'viewer',
    password_hash TEXT        NOT NULL DEFAULT '',
    sso_id        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Active user sessions identified by opaque tokens.
CREATE TABLE sessions (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token      TEXT        NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Alert rule definitions (thresholds, channels).
CREATE TABLE alert_configs (
    id         UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT             NOT NULL DEFAULT '',
    alert_type TEXT             NOT NULL DEFAULT '',
    threshold  DOUBLE PRECISION NOT NULL DEFAULT 0,
    channels   TEXT[]           NOT NULL DEFAULT '{}',
    enabled    BOOLEAN          NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- Historical record of fired alerts.
CREATE TABLE alert_history (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_config_id UUID        NOT NULL REFERENCES alert_configs(id) ON DELETE CASCADE,
    message         TEXT        NOT NULL DEFAULT '',
    severity        TEXT        NOT NULL DEFAULT 'warning',
    acknowledged    BOOLEAN     NOT NULL DEFAULT false,
    fired_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_sessions_token   ON sessions(token);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);

CREATE INDEX idx_alert_history_fired_at      ON alert_history(fired_at);
CREATE INDEX idx_alert_history_acknowledged  ON alert_history(acknowledged);
