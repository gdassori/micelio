package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

var level = new(slog.LevelVar) // supports runtime changes via SetLevel

// Init configures the global slog logger. Call once at startup.
// levelStr: "debug", "info", "warn", "error" (default: "info").
// format: "text" or "json" (default: "text").
func Init(levelStr, format string) {
	parseLevel(levelStr)

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

// For returns a logger tagged with the given component name.
// The returned logger dynamically delegates to slog.Default(), so runtime
// changes to the global default (e.g., via CaptureForTest) take effect
// immediately — even for package-level logger variables.
func For(component string) *slog.Logger {
	return slog.New(&dynamicHandler{component: component})
}

// SetLevel changes the log level at runtime. Useful in tests.
func SetLevel(l slog.Level) {
	level.Set(l)
}

func parseLevel(s string) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn", "warning":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
}

// dynamicHandler delegates each log call to slog.Default().Handler(),
// prepending a "component" attribute. This ensures that package-level loggers
// created via For() respect runtime changes to the default logger.
type dynamicHandler struct {
	component string
}

func (h *dynamicHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return slog.Default().Handler().Enabled(ctx, l)
}

func (h *dynamicHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.String("component", h.component))
	return slog.Default().Handler().Handle(ctx, r)
}

func (h *dynamicHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *dynamicHandler) WithGroup(name string) slog.Handler {
	return h
}
