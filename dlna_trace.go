package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Keep only recent control requests; never record cookies, media URLs or response bodies.
type dlnaTrace struct {
	mu     sync.Mutex
	file   string
	events []map[string]string
}

func (trace *dlnaTrace) record(request *http.Request, action string, fields map[string]string) {
	if trace == nil {
		return
	}
	event := map[string]string{
		"time": time.Now().Format(time.RFC3339Nano), "peer": request.RemoteAddr,
		"action": action, "method": request.Method,
	}
	for key, value := range fields {
		event[key] = value
	}
	for key, value := range event {
		if len(value) > 2048 {
			event[key] = strings.ToValidUTF8(value[:2048], "") + "…"
		}
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.events = append(trace.events, event)
	if len(trace.events) > 64 {
		trace.events = trace.events[len(trace.events)-64:]
	}
	data, err := json.MarshalIndent(trace.events, "", "  ")
	if err == nil {
		err = os.WriteFile(trace.file, data, 0600)
	}
	if err != nil {
		slog.Warn("Cannot save DLNA request diagnostics", "error", err)
	}
}

type dlnaTraceResponse struct {
	http.ResponseWriter
	status int
}

func (writer *dlnaTraceResponse) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}
