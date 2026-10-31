CREATE TABLE jira_ingested_tickets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jira_key TEXT NOT NULL UNIQUE,
    work_item_id UUID NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_jira_ingested_key ON jira_ingested_tickets(jira_key);

CREATE TABLE portal_submissions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tracking_id TEXT NOT NULL UNIQUE,
    work_item_id UUID REFERENCES work_items(id) ON DELETE SET NULL,
    title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT '',
    priority TEXT NOT NULL DEFAULT 'medium',
    contact_email TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'received',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_portal_submissions_tracking ON portal_submissions(tracking_id);
CREATE INDEX idx_portal_submissions_email ON portal_submissions(contact_email);
