package main

// Outbox wiring for the webhook listener (M1-04e).
//
// A webhook is not replayable: once the listener has answered GitHub or Jira,
// the delivery is gone. Before the conversion the listener published straight
// to Kafka and, on failure, wrote the raw body to webhook_dead_letters for a
// background retry. Now every accepted event is enqueued in a short
// transaction of its own (there is no other row write to share it with) and
// the service's outbox dispatcher delivers it with the outbox's retries,
// backoff and dead-lettering. The dead-letter table remains the fallback for
// a failed enqueue and for unparseable bodies.
//
// Addressing (ruling in the M1-04e brief):
//
//	GitHub events   aggregate_type "repository", aggregate_id = repositories.id,
//	                or a stable name-derived id when the repository is not
//	                registered, so a repository's events stay ordered either way
//	Jira events     aggregate_type "jira_issue", a stable id derived from the key
//	dead-letter     aggregate_type "webhook_dead_letter", the dead-letter row id
//
// Topics, keys and payloads are exactly what was published directly before,
// plus the additive event_id.
//
// Event ids are derived from the delivery, not random: GitHub's
// X-GitHub-Delivery (+ topic + index within the delivery) and Jira's webhook
// timestamp (+ issue key + changelog index). A sender's redelivery of the same
// webhook therefore enqueues the same ids, and the outbox insert's
// ON CONFLICT (id) DO NOTHING turns it into a no-op instead of a duplicate
// event. A delivery without those fields falls back to random ids.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	aggregateTypeRepository = "repository"
	aggregateTypeJiraIssue  = "jira_issue"
	aggregateTypeDeadLetter = "webhook_dead_letter"
)

// webhookNamespace seeds the name-derived aggregate ids. It is arbitrary but
// must never change: changing it would split one repository's queued events
// across two aggregates.
var webhookNamespace = uuid.MustParse("6f1d7c1e-3a52-4f0b-9d1e-5b2a8c4e7f90")

// webhookDB is the database surface the handlers need: transactions for the
// outbox, pooled reads for repository lookups, and the dead-letter insert.
// *pgxpool.Pool satisfies it.
type webhookDB interface {
	workgraph.TxBeginner
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// syntheticRepositoryID is the aggregate for a GitHub repository that has no
// repositories row. It is derived from the lower-cased full name, so all of
// one repository's events still share an aggregate.
func syntheticRepositoryID(fullName string) uuid.UUID {
	return uuid.NewSHA1(webhookNamespace, []byte("github:"+strings.ToLower(strings.TrimSpace(fullName))))
}

// syntheticJiraIssueID is the aggregate for a Jira issue's change events.
func syntheticJiraIssueID(issueKey string) uuid.UUID {
	return uuid.NewSHA1(webhookNamespace, []byte("jira:"+strings.TrimSpace(issueKey)))
}

// repositoryAggregateSQL matches the forms a repository's github_url is
// stored in (the full name, or the https URL with or without .git), ignoring
// case, oldest row first so the choice is stable.
const repositoryAggregateSQL = `
	SELECT id FROM repositories
	WHERE lower(github_url) = lower($1)
	   OR lower(github_url) = lower('https://github.com/' || $1)
	   OR lower(github_url) = lower('https://github.com/' || $1 || '.git')
	ORDER BY created_at, id
	LIMIT 1`

// repositoryAggregateID resolves a GitHub repository once per delivery: the
// repositories.id when the repository is registered (registered=true), or a
// stable name-derived id otherwise. The same id is the outbox aggregate for
// every event of the delivery and the repo_id of an extract request.
func repositoryAggregateID(ctx context.Context, db webhookDB, fullName string) (uuid.UUID, bool) {
	var id uuid.UUID
	if err := db.QueryRow(ctx, repositoryAggregateSQL, fullName).Scan(&id); err == nil && id != uuid.Nil {
		return id, true
	}
	return syntheticRepositoryID(fullName), false
}

// githubEventID derives the event id for the index-th event of a GitHub
// delivery. ok is false when the delivery id is missing.
func githubEventID(delivery, topic string, index int) (uuid.UUID, bool) {
	delivery = strings.TrimSpace(delivery)
	if delivery == "" {
		return uuid.Nil, false
	}
	return uuid.NewSHA1(webhookNamespace, []byte(fmt.Sprintf("github-delivery:%s:%s:%d", delivery, topic, index))), true
}

// jiraEventID derives the event id for the index-th changelog item of a Jira
// webhook. ok is false when the webhook carries no timestamp.
func jiraEventID(timestamp int64, issueKey string, index int) (uuid.UUID, bool) {
	if timestamp == 0 || issueKey == "" {
		return uuid.Nil, false
	}
	return uuid.NewSHA1(webhookNamespace, []byte(fmt.Sprintf("jira-webhook:%d:%s:%d", timestamp, issueKey, index))), true
}

// newEventWithID is outbox.NewEvent with a caller-chosen event id, carried
// the way NewEvent adopts one: as the payload's event_id field (so the row id,
// the header and the payload agree). id == uuid.Nil, or a payload that is not
// a JSON object, gets a random id.
func newEventWithID(id uuid.UUID, aggregateType string, aggregateID uuid.UUID, topic, key string, payload any) (outbox.Event, error) {
	if id == uuid.Nil {
		return outbox.NewEvent(aggregateType, aggregateID, topic, key, payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return outbox.Event{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return outbox.NewEvent(aggregateType, aggregateID, topic, key, payload)
	}
	encoded, err := json.Marshal(id.String())
	if err != nil {
		return outbox.Event{}, err
	}
	fields[outbox.FieldEventID] = encoded
	return outbox.NewEvent(aggregateType, aggregateID, topic, key, fields)
}

// pushEvents builds the events a push produces: external-change.detected for
// main/master, and codex.extract.requested when watched (the branch is
// watched for commit-linked codex extraction, which implies repoAggregate is
// the registered repositories.id). registered says repoAggregate is the
// registered repositories.id (not a name-derived id): only then does the
// external change name it as repository_id. delivery is X-GitHub-Delivery.
func pushEvents(payload githubPushPayload, repoAggregate uuid.UUID, registered, watched bool, now, delivery string) ([]outbox.Event, error) {
	var evs []outbox.Event
	nextID := func(topic string) uuid.UUID {
		id, _ := githubEventID(delivery, topic, len(evs))
		return id
	}
	if payload.Ref == "refs/heads/main" || payload.Ref == "refs/heads/master" {
		isAgent := payload.HeadCommit != nil && strings.HasPrefix(payload.HeadCommit.Message, "[agent]")
		change := externalChangeEvent{
			Source:    "github",
			Branch:    strings.TrimPrefix(payload.Ref, "refs/heads/"),
			CommitSHA: payload.After,
			IsAgent:   isAgent,
			Timestamp: now,
		}
		if registered {
			change.RepositoryID = repoAggregate.String()
		}
		ev, err := newEventWithID(nextID(bus.TopicExternalChangeDetected), aggregateTypeRepository, repoAggregate, bus.TopicExternalChangeDetected, payload.After, change)
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	if watched {
		repoID := repoAggregate
		ev, err := newEventWithID(nextID(bus.TopicCodexExtractRequested), aggregateTypeRepository, repoID, bus.TopicCodexExtractRequested, repoID.String(), map[string]any{
			"repo_id":    repoID,
			"commit_sha": payload.After,
			"branch":     strings.TrimPrefix(payload.Ref, "refs/heads/"),
			"parent_sha": "", // webhook doesn't carry before SHA reliably for merges
			"pushed_by":  payload.Pusher.Name,
			"pushed_at":  now,
		})
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// enqueueEvents writes evs in one short transaction. An empty slice opens no
// transaction.
func enqueueEvents(ctx context.Context, db workgraph.TxBeginner, evs []outbox.Event) error {
	if len(evs) == 0 {
		return nil
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return outbox.EnqueueAll(ctx, tx, evs...)
	})
}

// deadLetter is one webhook_dead_letters row due for a retry.
type deadLetter struct {
	id         string
	source     string
	eventType  string
	payload    []byte
	retryCount int
	maxRetries int
}

const resolveDeadLetterSQL = `UPDATE webhook_dead_letters SET resolved = true WHERE id = $1`

// retryDeadLetter enqueues a dead letter's stored body and marks the row
// resolved in the same transaction, so a retry cannot be both delivered and
// left unresolved (which would deliver it again on the next sweep).
func retryDeadLetter(ctx context.Context, db workgraph.TxBeginner, e deadLetter) error {
	id, err := uuid.Parse(e.id)
	if err != nil {
		return err
	}
	ev, err := outbox.NewEvent(aggregateTypeDeadLetter, id, deadLetterTopic(e.source, e.eventType), e.id, json.RawMessage(e.payload))
	if err != nil {
		return err
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := outbox.Enqueue(ctx, tx, ev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, resolveDeadLetterSQL, e.id)
		return err
	})
}
