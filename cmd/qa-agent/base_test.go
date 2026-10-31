package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

const (
	pinA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	pinB = "0123456789abcdef0123456789abcdef01234567"
)

type fakeBase struct {
	pins   []snapshot.Pin
	events []snapshot.Event
	err    error
}

func (f fakeBase) GetAll(context.Context, uuid.UUID) ([]snapshot.Pin, error) { return f.pins, f.err }
func (f fakeBase) Events(context.Context, uuid.UUID) ([]snapshot.Event, error) {
	return f.events, nil
}

func TestQABase_RecordsReceivedBaseAndNoDrift(t *testing.T) {
	repo := uuid.New()
	sha, rec := qaBase(context.Background(), fakeBase{pins: []snapshot.Pin{{RepositoryID: repo, BaseSHA: pinA}},
		events: []snapshot.Event{{Kind: snapshot.EventResolved, NewSHA: pinA}}}, uuid.New(), qaReadyMessage{BaseSHA: pinA})
	if sha != pinA || rec.Status != DriftNone || len(rec.Events) != 0 {
		t.Fatalf("sha=%s rec=%+v", sha, rec)
	}
}

func TestQABase_FallsBackToThePinAndReportsDriftEvents(t *testing.T) {
	repo := uuid.New()
	sha, rec := qaBase(context.Background(), fakeBase{
		pins: []snapshot.Pin{{RepositoryID: repo, BaseSHA: pinA}},
		events: []snapshot.Event{
			{Kind: snapshot.EventResolved, RepositoryID: repo, NewSHA: pinA},
			{Kind: snapshot.EventDriftDetected, RepositoryID: repo, OldSHA: pinA, NewSHA: pinB, Detail: "merges cleanly"},
		}}, uuid.New(), qaReadyMessage{})
	if sha != pinA {
		t.Fatalf("base = %s, want the pin", sha)
	}
	if rec.Status != DriftDetected || len(rec.Events) != 1 || rec.Events[0].Old != pinA || rec.Events[0].New != pinB {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestQABase_ReceivedDriftIsRecorded(t *testing.T) {
	d := &baseDrift{Old: pinA, New: pinB, Clean: true}
	_, rec := qaBase(context.Background(), fakeBase{pins: []snapshot.Pin{{BaseSHA: pinA}}}, uuid.New(), qaReadyMessage{BaseSHA: pinA, BaseDrift: d})
	if rec.Status != DriftDetected || rec.Received != d {
		t.Fatalf("rec = %+v", rec)
	}
	b, _ := json.Marshal(rec)
	if want := `"received":{"old":"` + pinA + `","new":"` + pinB + `","clean":true}`; !strings.Contains(string(b), want) {
		t.Fatalf("json %s lacks %s", b, want)
	}
}

func TestQABase_UnpinnedAndUnknown(t *testing.T) {
	sha, rec := qaBase(context.Background(), fakeBase{}, uuid.New(), qaReadyMessage{})
	if sha != "" || rec.Status != DriftUnpinned {
		t.Fatalf("legacy: sha=%s rec=%+v", sha, rec)
	}
	_, rec = qaBase(context.Background(), fakeBase{err: errors.New("db")}, uuid.New(), qaReadyMessage{BaseSHA: pinA})
	if rec.Status != DriftUnknown {
		t.Fatalf("read error: %+v", rec)
	}
}
