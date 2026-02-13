package logging

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Capture collects slog records for test assertions.
// Use CaptureForTest to install it as the global handler.
type Capture struct {
	mu        sync.Mutex
	records   []slog.Record
	prev      *slog.Logger
	prevLevel slog.Level
}

// CaptureForTest installs a capturing handler as the global slog default
// and returns a Capture that can be queried for assertions.
// Call Restore() when done (typically via defer).
func CaptureForTest() *Capture {
	c := &Capture{
		prev:      slog.Default(),
		prevLevel: level.Level(),
	}
	handler := &captureHandler{capture: c}
	slog.SetDefault(slog.New(handler))
	SetLevel(slog.LevelDebug) // capture everything
	return c
}

// Restore reinstates the previous global logger and log level.
func (c *Capture) Restore() {
	slog.SetDefault(c.prev)
	level.Set(c.prevLevel)
}

// Records returns a copy of all captured records.
func (c *Capture) Records() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// Has returns true if any captured record matches the given level and
// contains msgSubstring in its message.
func (c *Capture) Has(level slog.Level, msgSubstring string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level == level && strings.Contains(r.Message, msgSubstring) {
			return true
		}
	}
	return false
}

// Count returns the number of captured records at the given level.
func (c *Capture) Count(level slog.Level) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.records {
		if r.Level == level {
			n++
		}
	}
	return n
}

// captureHandler is a slog.Handler that appends records to a Capture.
type captureHandler struct {
	capture *Capture
	attrs   []slog.Attr
	group   string
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.capture.mu.Lock()
	defer h.capture.mu.Unlock()
	h.capture.records = append(h.capture.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{
		capture: h.capture,
		attrs:   append(h.attrs, attrs...),
		group:   h.group,
	}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{
		capture: h.capture,
		attrs:   h.attrs,
		group:   name,
	}
}
