CREATE TABLE codex_blobs (
  hash          TEXT PRIMARY KEY,
  section       TEXT NOT NULL,
  extractor     TEXT NOT NULL,
  payload       JSONB NOT NULL,
  confidence    NUMERIC(3,2) NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX codex_blobs_section_idx   ON codex_blobs(section);
CREATE INDEX codex_blobs_extractor_idx ON codex_blobs(extractor);

CREATE TABLE codex_snapshots (
  repo_id       UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  commit_sha    TEXT NOT NULL,
  branch        TEXT NOT NULL,
  parent_sha    TEXT,
  blob_hashes   TEXT[] NOT NULL,
  extracted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (repo_id, commit_sha)
);
CREATE INDEX codex_snapshots_branch_idx ON codex_snapshots(repo_id, branch);

CREATE TABLE codex_watched_branches (
  repo_id     UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  branch      TEXT NOT NULL,
  source      TEXT NOT NULL CHECK (source IN ('auto','user')),
  added_by    TEXT,
  added_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  enabled     BOOLEAN NOT NULL DEFAULT TRUE,
  PRIMARY KEY (repo_id, branch)
);

CREATE TABLE codex_git_strategy (
  repo_id             UUID PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
  pattern             TEXT NOT NULL,
  main_branch         TEXT NOT NULL,
  long_lived_branches TEXT[],
  merge_strategy      TEXT NOT NULL,
  pr_required         BOOLEAN NOT NULL,
  evidence            JSONB NOT NULL,
  extracted_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE codex_contract_edges (
  hash               TEXT PRIMARY KEY,
  contract_type      TEXT NOT NULL,
  contract_name      TEXT NOT NULL,
  producer_repo_id   UUID NOT NULL,
  producer_file      TEXT NOT NULL,
  producer_line      INT,
  consumer_repo_id   UUID NOT NULL,
  consumer_file      TEXT NOT NULL,
  consumer_line      INT,
  confidence         NUMERIC(3,2) NOT NULL,
  extracted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX codex_contract_edges_producer_idx ON codex_contract_edges(producer_repo_id, contract_name);
CREATE INDEX codex_contract_edges_consumer_idx ON codex_contract_edges(consumer_repo_id);

CREATE TABLE codex_product_topology (
  product_id     UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  repo_id        UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  role           TEXT NOT NULL,
  confidence     NUMERIC(3,2) NOT NULL,
  evidence       JSONB NOT NULL,
  extracted_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (product_id, repo_id, role)
);

CREATE TABLE codex_extraction_failure (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo_id         UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  commit_sha      TEXT NOT NULL,
  extractor       TEXT NOT NULL,
  error_message   TEXT NOT NULL,
  attempts        INT NOT NULL DEFAULT 1,
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX codex_extraction_failure_repo_idx ON codex_extraction_failure(repo_id, commit_sha);

ALTER TABLE products ADD COLUMN trusted_autoindex BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE codex_modules       ADD COLUMN source_commit_sha TEXT;
ALTER TABLE codex_modules       ADD COLUMN blob_hash         TEXT REFERENCES codex_blobs(hash);
ALTER TABLE codex_patterns      ADD COLUMN source_commit_sha TEXT;
ALTER TABLE codex_patterns      ADD COLUMN blob_hash         TEXT REFERENCES codex_blobs(hash);
ALTER TABLE codex_anti_patterns ADD COLUMN source_commit_sha TEXT;
ALTER TABLE codex_anti_patterns ADD COLUMN blob_hash         TEXT REFERENCES codex_blobs(hash);
ALTER TABLE codex_sections      ADD COLUMN source_commit_sha TEXT;
ALTER TABLE codex_sections      ADD COLUMN blob_hash         TEXT REFERENCES codex_blobs(hash);
