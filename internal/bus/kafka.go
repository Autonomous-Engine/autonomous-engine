package bus

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	numPartitions     = 6
	replicationFactor = 1
	minBytes          = 1
	maxBytes          = 10e6
	batchTimeout      = 10 * time.Millisecond
	commitInterval    = 1 * time.Second

	// requiredAcks is how many in-sync replicas must have written a record
	// before the broker answers the produce request.
	//
	// kafka-go's zero value is RequireNone, which means the broker sends no
	// produce response at all: WriteMessages returns nil as soon as the
	// request is on the socket, so a broker-side failure — no leader, a full
	// disk, an under-replicated partition, a rejected record — is invisible
	// to the publisher. Every caller of Publish/PublishMessage was therefore
	// fire-and-forget. The transactional outbox (internal/outbox) cannot be
	// more reliable than the ack it waits for: with RequireNone it would mark
	// a row published for a message the cluster never stored, which is silent
	// loss rather than the at-least-once delivery it promises.
	//
	// RequireAll waits for every in-sync replica. The compose cluster runs a
	// single broker at replication factor 1, so "all" is one replica today
	// and the cost is one round trip; on a replicated cluster it is the
	// setting that makes a publish survive the loss of a broker.
	requiredAcks = kafka.RequireAll

	// writerMaxAttempts bounds kafka-go's own retries inside one
	// WriteMessages call. kafka-go defaults to 10, which under an acked write
	// can keep a single publish in flight for minutes; 3 rides out a leader
	// election while letting a genuinely unreachable broker surface as an
	// error the caller can act on.
	writerMaxAttempts = 3

	// writeTimeout bounds one produce round trip. It is kafka-go's own
	// default, pinned here so an acked write has an explicit deadline rather
	// than an implicit one.
	writeTimeout = 10 * time.Second
)

// KafkaBus implements MessageBus backed by Apache Kafka via segmentio/kafka-go.
type KafkaBus struct {
	brokers []string

	// writers is a per-topic cache of kafka.Writer instances, created lazily.
	writersMu sync.Mutex
	writers   map[string]*kafka.Writer

	// subscribers tracks cancel functions and a WaitGroup for clean shutdown.
	subscribersMu     sync.Mutex
	subscriberCancels []context.CancelFunc
	subscribersWg     sync.WaitGroup
}

// NewKafkaBus creates a KafkaBus, connects to the cluster, and auto-creates all
// topics defined in AllTopics (6 partitions, replication factor 1). If a topic
// already exists the error is logged and ignored.
func NewKafkaBus(brokers []string) (*KafkaBus, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("bus: at least one broker address is required")
	}

	kb := &KafkaBus{
		brokers: brokers,
		writers: make(map[string]*kafka.Writer),
	}

	if err := kb.createTopics(); err != nil {
		return nil, fmt.Errorf("bus: topic auto-creation failed: %w", err)
	}

	return kb, nil
}

// createTopics dials the controller and creates all managed topics.
func (kb *KafkaBus) createTopics() error {
	conn, err := kafka.Dial("tcp", kb.brokers[0])
	if err != nil {
		return fmt.Errorf("dial %s: %w", kb.brokers[0], err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("get controller: %w", err)
	}

	controllerAddr := fmt.Sprintf("%s:%d", controller.Host, controller.Port)
	controllerConn, err := kafka.Dial("tcp", controllerAddr)
	if err != nil {
		return fmt.Errorf("dial controller %s: %w", controllerAddr, err)
	}
	defer controllerConn.Close()

	topicConfigs := make([]kafka.TopicConfig, 0, len(AllTopics))
	for _, t := range AllTopics {
		topicConfigs = append(topicConfigs, kafka.TopicConfig{
			Topic:             t,
			NumPartitions:     numPartitions,
			ReplicationFactor: replicationFactor,
		})
	}

	err = controllerConn.CreateTopics(topicConfigs...)
	if err != nil {
		// kafka-go returns an error that wraps per-topic errors; topics that
		// already exist produce a TopicAlreadyExists error code which we treat
		// as non-fatal.
		log.Printf("bus: CreateTopics returned (may include already-exists): %v", err)
	}

	return nil
}

// getOrCreateWriter returns the cached kafka.Writer for topic, creating one if
// it does not yet exist. Safe for concurrent use.
func (kb *KafkaBus) getOrCreateWriter(topic string) *kafka.Writer {
	kb.writersMu.Lock()
	defer kb.writersMu.Unlock()

	if w, ok := kb.writers[topic]; ok {
		return w
	}

	w := newWriter(kb.brokers, topic)
	kb.writers[topic] = w
	return w
}

// newWriter builds the per-topic writer.
//
// It is a function rather than an inline struct literal so the
// acknowledgement settings can be pinned by a unit test without a live
// broker. RequiredAcks is the difference between a publish that is durable
// and one that has merely left the process, and a struct literal quietly
// falling back to the zero value (RequireNone) is precisely how that
// guarantee went missing.
func newWriter(brokers []string, topic string) *kafka.Writer {
	return &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kafka.Hash{},
		BatchTimeout: batchTimeout,
		// Wait for the cluster to acknowledge the write; without this a
		// failed publish is indistinguishable from a successful one.
		RequiredAcks: requiredAcks,
		MaxAttempts:  writerMaxAttempts,
		WriteTimeout: writeTimeout,
		// Allow the writer to create topics automatically as a fallback.
		AllowAutoTopicCreation: true,
	}
}

// Publish sends payload to topic, routing by key so that the same key always
// lands on the same partition (ordering guarantee per work_id).
//
// It blocks until the cluster acknowledges the write (see requiredAcks): a nil
// return means the record is stored, an error means it is not. That costs one
// broker round trip per batch compared with the previous fire-and-forget
// behaviour, which is the price of being able to tell the two apart.
func (kb *KafkaBus) Publish(ctx context.Context, topic string, key string, payload []byte) error {
	w := kb.getOrCreateWriter(topic)
	return w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: payload,
	})
}

// PublishMessage sends a Message, forwarding any headers alongside the payload.
func (kb *KafkaBus) PublishMessage(ctx context.Context, msg *Message) error {
	w := kb.getOrCreateWriter(msg.Topic)

	headers := make([]kafka.Header, 0, len(msg.Headers))
	for k, v := range msg.Headers {
		headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
	}

	return w.WriteMessages(ctx, kafka.Message{
		Key:     []byte(msg.Key),
		Value:   msg.Value,
		Headers: headers,
	})
}

// Subscribe registers handler for a single topic within the given consumer group.
func (kb *KafkaBus) Subscribe(topic string, group string, handler Handler) error {
	return kb.SubscribeMultiple([]string{topic}, group, handler)
}

// SubscribeMultiple registers handler for multiple topics within the given consumer
// group. A goroutine is spawned that reads messages in a loop; on handler error
// the message is not committed so it will be redelivered.
func (kb *KafkaBus) SubscribeMultiple(topics []string, group string, handler Handler) error {
	ctx, cancel := context.WithCancel(context.Background())

	kb.subscribersMu.Lock()
	kb.subscriberCancels = append(kb.subscriberCancels, cancel)
	kb.subscribersWg.Add(1)
	kb.subscribersMu.Unlock()

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:           kb.brokers,
		GroupID:           group,
		GroupTopics:       topics,
		MinBytes:          minBytes,
		MaxBytes:          maxBytes,
		CommitInterval:    commitInterval,
		StartOffset:       kafka.LastOffset,
		SessionTimeout:    10 * time.Second,
		HeartbeatInterval: 3 * time.Second,
	})

	go func() {
		defer kb.subscribersWg.Done()
		defer func() {
			if err := reader.Close(); err != nil {
				log.Printf("bus: reader close error (group=%s topics=%v): %v", group, topics, err)
			}
		}()

		for {
			km, err := reader.FetchMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					// Context cancelled — clean shutdown.
					return
				}
				log.Printf("bus: FetchMessage error (group=%s topics=%v): %v", group, topics, err)
				continue
			}

			headers := make(map[string]string, len(km.Headers))
			for _, h := range km.Headers {
				headers[h.Key] = string(h.Value)
			}

			msg := &Message{
				Topic:   km.Topic,
				Key:     string(km.Key),
				Value:   km.Value,
				Headers: headers,
			}
			// M1-05: expose the event id to the handler (header first,
			// then the payload field) and count messages without one.
			resolveEventID(group, msg)

			if err := handler(ctx, msg); err != nil {
				log.Printf("bus: handler error (group=%s topic=%s key=%s): %v — skipping commit",
					group, msg.Topic, msg.Key, err)
				// Do not commit; message will be redelivered.
				continue
			}

			if err := reader.CommitMessages(ctx, km); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("bus: CommitMessages error (group=%s topic=%s): %v", group, msg.Topic, err)
			}
		}
	}()

	return nil
}

// HealthCheck returns a function suitable for health.Checker.AddCheck that
// verifies the Kafka cluster is reachable. It performs a short-timeout dial
// to the first configured broker and asks for the controller — a successful
// round-trip confirms the cluster is forming a quorum and reachable over TCP.
//
// Agents should wire this into their /health endpoint so a partitioned or
// down Kafka cluster turns the probe red instead of leaving /health green
// while the agent silently stalls on fetch.
func (kb *KafkaBus) HealthCheck() func() error {
	return KafkaHealthCheck(kb.brokers)
}

// KafkaHealthCheck is a package-level variant for processes that hold only
// the brokers list (e.g. credential-monitor may fail to init KafkaBus but
// still want a health signal).
func KafkaHealthCheck(brokers []string) func() error {
	return func() error {
		if len(brokers) == 0 {
			return fmt.Errorf("no kafka brokers configured")
		}
		dialer := &kafka.Dialer{Timeout: 3 * time.Second}
		conn, err := dialer.Dial("tcp", brokers[0])
		if err != nil {
			return fmt.Errorf("dial %s: %w", brokers[0], err)
		}
		defer conn.Close()
		if _, err := conn.Controller(); err != nil {
			return fmt.Errorf("controller lookup: %w", err)
		}
		return nil
	}
}

// Close cancels all subscriber contexts, waits for their goroutines to finish,
// then closes every cached writer.
func (kb *KafkaBus) Close() error {
	// Cancel all subscriber goroutines.
	kb.subscribersMu.Lock()
	for _, cancel := range kb.subscriberCancels {
		cancel()
	}
	kb.subscribersMu.Unlock()

	// Wait for all goroutines to exit.
	kb.subscribersWg.Wait()

	// Close all writers.
	kb.writersMu.Lock()
	defer kb.writersMu.Unlock()

	var firstErr error
	for topic, w := range kb.writers {
		if err := w.Close(); err != nil {
			log.Printf("bus: writer close error (topic=%s): %v", topic, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}
