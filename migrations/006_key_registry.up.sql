CREATE TYPE credential_scope AS ENUM ('global', 'integration', 'agent_type', 'agent_instance');
CREATE TYPE credential_category AS ENUM ('llm', 'github', 'jira', 'system', 'dashboard');
CREATE TYPE audit_action AS ENUM ('created', 'updated', 'deleted', 'read', 'rotated');
CREATE TYPE config_value_type AS ENUM ('string', 'int', 'bool', 'json');

CREATE TABLE credential_registry (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope credential_scope NOT NULL DEFAULT 'global',
    scope_target TEXT NOT NULL DEFAULT '',
    key_name TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    category credential_category NOT NULL,
    is_secret BOOLEAN NOT NULL DEFAULT true,
    description TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scope, scope_target, key_name, category)
);

CREATE INDEX idx_credential_registry_category ON credential_registry(category, key_name);
CREATE INDEX idx_credential_registry_scope ON credential_registry(scope, scope_target);

CREATE TABLE credential_audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credential_id UUID REFERENCES credential_registry(id) ON DELETE SET NULL,
    action audit_action NOT NULL,
    actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_type TEXT NOT NULL DEFAULT 'system',
    ip_address TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}',
    timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_credential_audit_timestamp ON credential_audit_log(timestamp);
CREATE INDEX idx_credential_audit_credential ON credential_audit_log(credential_id);
CREATE INDEX idx_credential_audit_actor ON credential_audit_log(actor_id);

CREATE TABLE system_config (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope credential_scope NOT NULL DEFAULT 'global',
    scope_target TEXT NOT NULL DEFAULT '',
    key_name TEXT NOT NULL,
    value TEXT NOT NULL DEFAULT '',
    value_type config_value_type NOT NULL DEFAULT 'string',
    category TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scope, scope_target, key_name)
);

CREATE INDEX idx_system_config_category ON system_config(category);

CREATE OR REPLACE FUNCTION notify_config_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('config_changed', TG_TABLE_NAME || ':' || COALESCE(NEW.id::text, OLD.id::text));
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER credential_registry_notify
    AFTER INSERT OR UPDATE OR DELETE ON credential_registry
    FOR EACH ROW EXECUTE FUNCTION notify_config_change();

CREATE TRIGGER system_config_notify
    AFTER INSERT OR UPDATE OR DELETE ON system_config
    FOR EACH ROW EXECUTE FUNCTION notify_config_change();
