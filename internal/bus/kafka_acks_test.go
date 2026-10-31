package bus

import (
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// TestNewWriter_WaitsForBrokerAcknowledgement pins the one setting the
// transactional outbox depends on.
//
// kafka.Writer's zero RequiredAcks is kafka.RequireNone, under which the
// broker sends no produce response and WriteMessages returns nil as soon as
// the request reaches the socket. Publishing would then report success for a
// record the cluster never stored, and internal/outbox — which marks a row
// published only after the bus "acknowledges" it — would be recording an
// acknowledgement that never happened.
//
// This test fails if the writer construction ever loses the setting, which is
// easy to do: it is a struct literal, and an omitted field is silently the
// dangerous value.
func TestNewWriter_WaitsForBrokerAcknowledgement(t *testing.T) {
	w := newWriter([]string{"broker-a:9092", "broker-b:9092"}, "implementation.tasks")

	if w.RequiredAcks == kafka.RequireNone {
		t.Fatal("writer has RequiredAcks=RequireNone: publishes are fire-and-forget and the outbox would mark unstored messages as published")
	}
	if w.RequiredAcks != kafka.RequireAll {
		t.Errorf("RequiredAcks = %v, want kafka.RequireAll (%v)", w.RequiredAcks, kafka.RequireAll)
	}
}

// TestNewWriter_BoundsOneAckedPublish pins the two settings that bound an
// acked write. With RequireAll a publish waits on the broker, so an unbounded
// retry count or an implicit deadline turns a sick cluster into a wedged
// publisher.
//
// The exact values matter beyond this package, which is why they are pinned
// rather than merely required to be positive: MaxAttempts x WriteTimeout is
// the worst case for one WriteMessages call, and internal/outbox sizes its
// DefaultPublishTimeout (40s) above it so the writer's own retries can
// actually run and last_error records the broker's complaint instead of the
// dispatcher's generic timeout. Change either number here and that budget
// has to move with it.
func TestNewWriter_BoundsOneAckedPublish(t *testing.T) {
	w := newWriter([]string{"broker-a:9092"}, "implementation.tasks")

	if w.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want exactly 3; internal/outbox's DefaultPublishTimeout is sized against MaxAttempts x WriteTimeout", w.MaxAttempts)
	}
	if w.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %s, want exactly 10s; internal/outbox's DefaultPublishTimeout is sized against MaxAttempts x WriteTimeout", w.WriteTimeout)
	}
	if worst := time.Duration(w.MaxAttempts) * w.WriteTimeout; worst != 30*time.Second {
		t.Errorf("worst case for one publish = %s, want 30s; if this changes, raise outbox.DefaultPublishTimeout above it", worst)
	}
}

// TestNewWriter_KeepsRoutingAndTopicSettings guards the fields the rest of the
// bus relies on, so a future edit to the ack settings cannot quietly drop
// them: Hash balancing is the per-key partition affinity that gives ordering
// per work_id, and AllowAutoTopicCreation is the fallback for a topic
// createTopics did not manage to create.
func TestNewWriter_KeepsRoutingAndTopicSettings(t *testing.T) {
	w := newWriter([]string{"broker-a:9092"}, "implementation.tasks")

	if w.Topic != "implementation.tasks" {
		t.Errorf("Topic = %q, want %q", w.Topic, "implementation.tasks")
	}
	if _, ok := w.Balancer.(*kafka.Hash); !ok {
		t.Errorf("Balancer = %T, want *kafka.Hash so one key always lands on one partition", w.Balancer)
	}
	if !w.AllowAutoTopicCreation {
		t.Error("AllowAutoTopicCreation = false, want true as the fallback for an uncreated topic")
	}
}

// TestGetOrCreateWriter_UsesTheAckedWriter proves the production path — the
// one Publish and PublishMessage actually call — hands back the writer
// newWriter built, cached per topic. A test of newWriter alone would pass even
// if getOrCreateWriter still built its own literal.
func TestGetOrCreateWriter_UsesTheAckedWriter(t *testing.T) {
	kb := &KafkaBus{
		brokers: []string{"broker-a:9092"},
		writers: map[string]*kafka.Writer{},
	}

	w := kb.getOrCreateWriter("implementation.tasks")
	if w.RequiredAcks != kafka.RequireAll {
		t.Errorf("cached writer RequiredAcks = %v, want kafka.RequireAll", w.RequiredAcks)
	}
	if again := kb.getOrCreateWriter("implementation.tasks"); again != w {
		t.Error("getOrCreateWriter returned a second writer for the same topic; the per-topic cache is broken")
	}
}
