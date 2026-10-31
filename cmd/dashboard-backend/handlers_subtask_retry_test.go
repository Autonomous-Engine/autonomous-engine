package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// A dashboard retry of a blocked subtask re-publishes implementation.tasks.
// cmd/implementation-agent decodes that message with the planner's field
// names (work_id = the parent work item, repository_id, product_id,
// description, files_to_change), so the dashboard must publish exactly that
// shape; a message with repo_id/task keys or work_id = the subtask's own id
// is silently unusable by the agent.
func TestBuildSubtaskRetryMessage(t *testing.T) {
	subtaskID := uuid.New()
	parentID := uuid.New()
	productID := uuid.New()
	repoID := uuid.New()

	cases := []struct {
		name string
		item *models.WorkItem
		want subtaskRetryMessage
	}{
		{
			name: "full subtask: work_id is the parent, ids are forwarded",
			item: &models.WorkItem{
				ID:           subtaskID,
				Type:         models.WorkItemTypeSubtask,
				ParentID:     &parentID,
				ProductID:    &productID,
				RepositoryID: &repoID,
				Title:        "Add retry",
				Description:  "Implement the retry path",
			},
			want: subtaskRetryMessage{
				TaskID:        subtaskID.String(),
				WorkID:        parentID.String(),
				ProductID:     productID.String(),
				RepositoryID:  repoID.String(),
				Title:         "Add retry",
				Description:   "Implement the retry path",
				FilesToChange: []string{},
				Retry:         true,
			},
		},
		{
			name: "no parent: work_id falls back to the subtask's own id",
			item: &models.WorkItem{
				ID:          subtaskID,
				Type:        models.WorkItemTypeSubtask,
				Title:       "Orphan",
				Description: "No parent, product, or repository",
			},
			want: subtaskRetryMessage{
				TaskID:        subtaskID.String(),
				WorkID:        subtaskID.String(),
				Title:         "Orphan",
				Description:   "No parent, product, or repository",
				FilesToChange: []string{},
				Retry:         true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSubtaskRetryMessage(tc.item)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildSubtaskRetryMessage =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// TestSubtaskRetryMessageWireShape pins the JSON keys to the ones
// cmd/implementation-agent's implementationTaskMessage decodes, and rejects
// the keys the old blocked-prompt retry used.
func TestSubtaskRetryMessageWireShape(t *testing.T) {
	subtaskID := uuid.New()
	parentID := uuid.New()
	productID := uuid.New()
	repoID := uuid.New()
	msg := buildSubtaskRetryMessage(&models.WorkItem{
		ID:           subtaskID,
		Type:         models.WorkItemTypeSubtask,
		ParentID:     &parentID,
		ProductID:    &productID,
		RepositoryID: &repoID,
		Title:        "t",
		Description:  "d",
	})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}

	wantString := map[string]string{
		"task_id":       subtaskID.String(),
		"work_id":       parentID.String(),
		"product_id":    productID.String(),
		"repository_id": repoID.String(),
		"title":         "t",
		"description":   "d",
	}
	for key, want := range wantString {
		field, ok := wire[key]
		if !ok {
			t.Fatalf("wire message lacks %q: %s", key, raw)
		}
		var got string
		if err := json.Unmarshal(field, &got); err != nil || got != want {
			t.Fatalf("%s = %s, want %q", key, field, want)
		}
	}
	if got := string(wire["files_to_change"]); got != "[]" {
		t.Fatalf("files_to_change = %s, want [] (not null: the agent ranges over it)", got)
	}
	for _, stale := range []string{"repo_id", "task", "timestamp"} {
		if _, ok := wire[stale]; ok {
			t.Fatalf("wire message still carries the unusable key %q: %s", stale, raw)
		}
	}

	// A subtask without a repository omits repository_id, as the planner does.
	noRepo := buildSubtaskRetryMessage(&models.WorkItem{ID: subtaskID, ParentID: &parentID})
	raw, _ = json.Marshal(noRepo)
	wire = map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &wire)
	if _, ok := wire["repository_id"]; ok {
		t.Fatalf("repository_id should be omitted when the subtask has none: %s", raw)
	}
}
