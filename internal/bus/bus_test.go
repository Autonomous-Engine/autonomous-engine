package bus_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
)

// TestKafkaIntegration is an integration test that requires a live Kafka broker.
// Set TEST_KAFKA_BROKERS to a comma-separated list of broker addresses to run it,
// e.g.: TEST_KAFKA_BROKERS=localhost:9092 go test ./internal/bus/...
func TestKafkaIntegration(t *testing.T) {
	brokerEnv := os.Getenv("TEST_KAFKA_BROKERS")
	if brokerEnv == "" {
		t.Skip("TEST_KAFKA_BROKERS not set — skipping Kafka integration test")
	}

	brokers := strings.Split(brokerEnv, ",")

	kb, err := bus.NewKafkaBus(brokers)
	if err != nil {
		t.Fatalf("NewKafkaBus: %v", err)
	}
	defer func() {
		if err := kb.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	received := make(chan *bus.Message, 1)

	err = kb.Subscribe(bus.TopicIntake, "test-group-integration", func(ctx context.Context, msg *bus.Message) error {
		received <- msg
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give the consumer a moment to join the group and be assigned partitions.
	time.Sleep(2 * time.Second)

	wantKey := "work-test-001"
	wantValue := []byte(`{"work_id":"work-test-001","title":"integration test"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := kb.Publish(ctx, bus.TopicIntake, wantKey, wantValue); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case msg := <-received:
		if msg.Key != wantKey {
			t.Errorf("got key %q, want %q", msg.Key, wantKey)
		}
		if string(msg.Value) != string(wantValue) {
			t.Errorf("got value %q, want %q", msg.Value, wantValue)
		}
		t.Logf("received message: topic=%s key=%s value=%s", msg.Topic, msg.Key, msg.Value)

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for message after 10 seconds")
	}
}

// TestPublishMessageWithHeaders verifies that PublishMessage forwards headers correctly.
// Also requires TEST_KAFKA_BROKERS.
func TestPublishMessageWithHeaders(t *testing.T) {
	brokerEnv := os.Getenv("TEST_KAFKA_BROKERS")
	if brokerEnv == "" {
		t.Skip("TEST_KAFKA_BROKERS not set — skipping Kafka integration test")
	}

	brokers := strings.Split(brokerEnv, ",")

	kb, err := bus.NewKafkaBus(brokers)
	if err != nil {
		t.Fatalf("NewKafkaBus: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	received := make(chan *bus.Message, 1)

	err = kb.Subscribe(bus.TopicPRDReady, "test-group-headers", func(ctx context.Context, msg *bus.Message) error {
		received <- msg
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	time.Sleep(2 * time.Second)

	outMsg := &bus.Message{
		Topic: bus.TopicPRDReady,
		Key:   "work-hdr-001",
		Value: []byte(`{"work_id":"work-hdr-001"}`),
		Headers: map[string]string{
			"source":  "test",
			"version": "1",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := kb.PublishMessage(ctx, outMsg); err != nil {
		t.Fatalf("PublishMessage: %v", err)
	}

	select {
	case msg := <-received:
		if msg.Key != outMsg.Key {
			t.Errorf("got key %q, want %q", msg.Key, outMsg.Key)
		}
		if msg.Headers["source"] != "test" {
			t.Errorf("header source: got %q, want %q", msg.Headers["source"], "test")
		}
		t.Logf("received message with headers: %v", msg.Headers)

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for message after 10 seconds")
	}
}
