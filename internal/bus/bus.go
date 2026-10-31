package bus

import "context"

// Message represents a Kafka message with topic routing metadata and optional headers.
type Message struct {
	Topic   string
	Key     string
	Value   []byte
	Headers map[string]string
}

// Handler is a function that processes a received message.
// Returning a non-nil error causes the message to not be committed (redelivered).
type Handler func(ctx context.Context, msg *Message) error

// MessageBus defines the contract for publishing and subscribing to messages.
type MessageBus interface {
	// Publish sends a raw payload to the given topic with the given partition key.
	Publish(ctx context.Context, topic string, key string, payload []byte) error

	// PublishMessage sends a fully-constructed Message, including any headers.
	PublishMessage(ctx context.Context, msg *Message) error

	// Subscribe registers a handler for a single topic within a consumer group.
	Subscribe(topic string, group string, handler Handler) error

	// SubscribeMultiple registers a handler for multiple topics within a consumer group.
	SubscribeMultiple(topics []string, group string, handler Handler) error

	// Close shuts down all producers and consumers cleanly.
	Close() error
}
