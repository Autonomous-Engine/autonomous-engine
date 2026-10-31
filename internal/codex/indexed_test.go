package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
)

func TestCodexIndexedTopicIsManaged(t *testing.T) {
	if bus.TopicCodexIndexed != "codex.indexed" {
		t.Fatalf("TopicCodexIndexed = %q", bus.TopicCodexIndexed)
	}
	for _, tp := range bus.AllTopics {
		if tp == bus.TopicCodexIndexed {
			return
		}
	}
	t.Fatal("codex.indexed is not in bus.AllTopics (no auto-creation)")
}

func TestIndexedEventWireShape(t *testing.T) {
	pid := uuid.MustParse("8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10")
	ev := IndexedEvent{
		ProductID: pid, Branch: "develop", ProductVersion: 4, VersionNumber: 91,
		SectionsChanged: []string{"module:a", "section:risk_zones"}, Trigger: TriggerIncrementalIndex,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"product_id", "branch", "product_version", "sections_changed", "trigger", "version_number"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("payload lacks %q: %s", k, raw)
		}
	}
	if fields["product_id"] != pid.String() || fields["product_version"] != float64(4) || fields["trigger"] != "incremental_index" {
		t.Errorf("payload = %s", raw)
	}
}

func TestDecodeIndexedEvent(t *testing.T) {
	pid := uuid.New()
	good := `{"product_id":"` + pid.String() + `","branch":"main","product_version":3,"version_number":10,"sections_changed":["legacy","module:b"],"trigger":"full_index_product","event_id":"e-1"}`
	ev, err := DecodeIndexedEvent([]byte(good))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.ProductID != pid || ev.Branch != "main" || ev.ProductVersion != 3 || ev.EventID != "e-1" {
		t.Errorf("decoded %+v", ev)
	}
	if got, want := ev.Modules(), []string{"b", "legacy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Modules() = %v, want %v", got, want)
	}
	for name, bad := range map[string]string{
		"not json":       `nope`,
		"no product":     `{"branch":"main","product_version":1}`,
		"nil product":    `{"product_id":"00000000-0000-0000-0000-000000000000","branch":"main","product_version":1}`,
		"no branch":      `{"product_id":"` + pid.String() + `","product_version":1}`,
		"no version":     `{"product_id":"` + pid.String() + `","branch":"main"}`,
		"version is str": `{"product_id":"` + pid.String() + `","branch":"main","product_version":"1"}`,
	} {
		if _, err := DecodeIndexedEvent([]byte(bad)); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
}

func TestIndexedOutboxEvent(t *testing.T) {
	pid := uuid.New()
	ev, err := indexedOutboxEvent(IndexedEvent{ProductID: pid, Branch: "main", ProductVersion: 2, VersionNumber: 7, SectionsChanged: []string{"module:x"}, Trigger: TriggerOnboardingApproval})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Topic != bus.TopicCodexIndexed || ev.AggregateType != "product" || ev.AggregateID != pid || ev.Key != pid.String() {
		t.Errorf("event addressing = %s %s/%s key %s", ev.Topic, ev.AggregateType, ev.AggregateID, ev.Key)
	}
	if ev.Headers[outbox.HeaderEventID] == "" || !strings.Contains(string(ev.Payload), `"event_id"`) {
		t.Errorf("event id not carried: headers=%v payload=%s", ev.Headers, ev.Payload)
	}
	back, err := DecodeIndexedEvent(ev.Payload)
	if err != nil || back.ProductVersion != 2 || back.EventID != ev.ID.String() {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}
