package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// logger writes structured JSON log lines to stderr (stdout stays reserved for
// resource payloads, per the kernel's logging discipline). Every line carries a
// timestamp, an event name, and the event's fields.
type logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newLogger() *logger {
	return &logger{enc: json.NewEncoder(os.Stderr)}
}

// event emits one structured log line. A nil fields map is fine.
func (l *logger) event(event string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	l.enc.Encode(rec)
}
