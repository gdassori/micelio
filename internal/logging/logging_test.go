package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestInitText(t *testing.T) {
	Init("info", "text")
	logger := slog.Default()
	if logger == nil {
		t.Fatal("logger should not be nil after Init")
	}
}

func TestInitJSON(t *testing.T) {
	Init("debug", "json")
	logger := slog.Default()
	if logger == nil {
		t.Fatal("logger should not be nil after Init")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"  Error  ", slog.LevelError},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}
	for _, tt := range tests {
		parseLevel(tt.input)
		if level.Level() != tt.want {
			t.Errorf("parseLevel(%q): got %v, want %v", tt.input, level.Level(), tt.want)
		}
	}
}

func TestSetLevel(t *testing.T) {
	SetLevel(slog.LevelWarn)
	if level.Level() != slog.LevelWarn {
		t.Errorf("SetLevel(Warn): got %v", level.Level())
	}
	SetLevel(slog.LevelInfo)
}

func TestFor(t *testing.T) {
	logger := For("test-component")
	if logger == nil {
		t.Fatal("For() returned nil")
	}
	// Should be able to log without panicking
	logger.Info("test message", "key", "value")
}

func TestDynamicHandlerEnabled(t *testing.T) {
	SetLevel(slog.LevelWarn)
	defer SetLevel(slog.LevelInfo)

	h := &dynamicHandler{component: "test"}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug should not be enabled at warn level")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error should be enabled at warn level")
	}
}

func TestDynamicHandlerWithAttrsAndGroup(t *testing.T) {
	h := &dynamicHandler{component: "test"}

	h2 := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	if h2 != h {
		t.Error("WithAttrs should return same handler")
	}

	h3 := h.WithGroup("grp")
	if h3 != h {
		t.Error("WithGroup should return same handler")
	}
}

func TestCaptureForTest(t *testing.T) {
	c := CaptureForTest()
	defer c.Restore()

	slog.Info("hello")
	slog.Warn("warning message")
	slog.Debug("debug detail")

	records := c.Records()
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}

	if !c.Has(slog.LevelInfo, "hello") {
		t.Error("should have info 'hello'")
	}
	if !c.Has(slog.LevelWarn, "warning") {
		t.Error("should have warn 'warning'")
	}
	if c.Has(slog.LevelError, "hello") {
		t.Error("should not match error level")
	}
	if c.Has(slog.LevelInfo, "nonexistent") {
		t.Error("should not match nonexistent message")
	}

	if c.Count(slog.LevelInfo) != 1 {
		t.Errorf("expected 1 info, got %d", c.Count(slog.LevelInfo))
	}
	if c.Count(slog.LevelWarn) != 1 {
		t.Errorf("expected 1 warn, got %d", c.Count(slog.LevelWarn))
	}
	if c.Count(slog.LevelDebug) != 1 {
		t.Errorf("expected 1 debug, got %d", c.Count(slog.LevelDebug))
	}
	if c.Count(slog.LevelError) != 0 {
		t.Errorf("expected 0 error, got %d", c.Count(slog.LevelError))
	}
}

func TestCaptureRestore(t *testing.T) {
	prev := slog.Default()
	c := CaptureForTest()
	c.Restore()

	// After restore, default logger should be back to previous
	if slog.Default() != prev {
		t.Error("default logger not restored")
	}
}

func TestCaptureHandlerWithAttrsAndGroup(t *testing.T) {
	h := &captureHandler{capture: &Capture{}}

	h2 := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	ch2, ok := h2.(*captureHandler)
	if !ok {
		t.Fatal("WithAttrs should return *captureHandler")
	}
	if len(ch2.attrs) != 1 {
		t.Errorf("expected 1 attr, got %d", len(ch2.attrs))
	}

	h3 := h.WithGroup("mygroup")
	ch3, ok := h3.(*captureHandler)
	if !ok {
		t.Fatal("WithGroup should return *captureHandler")
	}
	if ch3.group != "mygroup" {
		t.Errorf("expected group 'mygroup', got %q", ch3.group)
	}
}

func TestCaptureHandlerEnabled(t *testing.T) {
	h := &captureHandler{capture: &Capture{}}
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("captureHandler should always be enabled")
	}
}

func TestForWithCapture(t *testing.T) {
	c := CaptureForTest()
	defer c.Restore()

	logger := For("mycomp")
	logger.Info("component log")

	if !c.Has(slog.LevelInfo, "component log") {
		t.Error("For() logger should use captured handler")
	}
}
