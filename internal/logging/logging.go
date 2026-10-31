package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type contextKey string

const correlationKey contextKey = "work_item_id"

// Logger is a structured JSON logger that tags every entry with a service name
// and an optional correlation ID extracted from context.
type Logger struct {
	service string
	output  *json.Encoder
}

// New creates a new Logger that writes JSON to stdout.
func New(service string) *Logger {
	return &Logger{
		service: service,
		output:  json.NewEncoder(os.Stdout),
	}
}

// logEntry is the JSON shape written for every log line.
type logEntry struct {
	Timestamp  string                 `json:"ts"`
	Level      string                 `json:"level"`
	Service    string                 `json:"service"`
	WorkItemID string                 `json:"work_item_id,omitempty"`
	Message    string                 `json:"msg"`
	Error      string                 `json:"error,omitempty"`
	Fields     map[string]interface{} `json:"fields,omitempty"`
}

// WithWorkItemID returns a new context that carries the given work-item ID.
// Downstream log calls will include it automatically.
func WithWorkItemID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// WorkItemIDFromCtx extracts the work-item ID stored by WithWorkItemID.
// Returns an empty string when none is set.
func WorkItemIDFromCtx(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey).(string); ok {
		return id
	}
	return ""
}

// Info emits an info-level log entry.
func (l *Logger) Info(ctx context.Context, msg string, fields ...map[string]interface{}) {
	l.log(ctx, "info", msg, "", fields)
}

// Warn emits a warn-level log entry.
func (l *Logger) Warn(ctx context.Context, msg string, fields ...map[string]interface{}) {
	l.log(ctx, "warn", msg, "", fields)
}

// Error emits an error-level log entry. err may be nil.
func (l *Logger) Error(ctx context.Context, msg string, err error, fields ...map[string]interface{}) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	l.log(ctx, "error", msg, errStr, fields)
}

func (l *Logger) log(ctx context.Context, level, msg, errStr string, fields []map[string]interface{}) {
	entry := logEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Level:      level,
		Service:    l.service,
		WorkItemID: WorkItemIDFromCtx(ctx),
		Message:    msg,
		Error:      errStr,
	}
	if len(fields) > 0 && fields[0] != nil {
		entry.Fields = fields[0]
	}
	// Encode is safe for concurrent use; errors are intentionally ignored
	// because we have nowhere useful to report them.
	if err := l.output.Encode(entry); err != nil {
		fmt.Fprintf(os.Stderr, "logging: encode: %v\n", err)
	}
}
