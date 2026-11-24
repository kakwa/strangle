package riverqueue

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// captureCore records every entry written through it so tests can assert on
// levels, messages and fields.
type captureCore struct {
	zapcore.LevelEnabler
	mu      sync.Mutex
	entries []capturedEntry
}

type capturedEntry struct {
	level   zapcore.Level
	message string
	fields  []zap.Field
}

func newCaptureCore(level zapcore.Level) *captureCore {
	return &captureCore{LevelEnabler: zapcore.LevelEnabler(level)}
}

func (c *captureCore) With(_ []zap.Field) zapcore.Core { return c }

func (c *captureCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *captureCore) Write(e zapcore.Entry, f []zap.Field) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, capturedEntry{level: e.Level, message: e.Message, fields: append([]zap.Field{}, f...)})
	return nil
}

func (c *captureCore) Sync() error { return nil }

func (c *captureCore) all() []capturedEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedEntry{}, c.entries...)
}

func (c *captureCore) last() (capturedEntry, bool) {
	entries := c.all()
	if len(entries) == 0 {
		return capturedEntry{}, false
	}
	return entries[len(entries)-1], true
}

// assertFields checks that got contains each want field exactly once.
func assertFields(t *testing.T, got, want []zap.Field) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("captured %d fields, want %d: %v", len(got), len(want), got)
	}
	for _, wf := range want {
		found := false
		for _, f := range got {
			if f.Equals(wf) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing field %s (%T)", wf.Key, wf)
		}
	}
}

func TestZapSlogLevels(t *testing.T) {
	cases := []struct {
		slog slog.Level
		zap  zapcore.Level
	}{
		{slog.LevelDebug, zapcore.DebugLevel},
		{slog.LevelInfo, zapcore.InfoLevel},
		{slog.LevelWarn, zapcore.WarnLevel},
		{slog.LevelError, zapcore.ErrorLevel},
	}
	for _, tc := range cases {
		core := newCaptureCore(zapcore.DebugLevel)
		logger := slog.New(newZapSlogHandler(core))
		logger.Log(context.Background(), tc.slog, "message "+tc.slog.String())

		entry, ok := core.last()
		if !ok {
			t.Fatalf("level %v: no entry captured", tc.slog)
		}
		if entry.level != tc.zap {
			t.Errorf("level %v: got zap level %v, want %v", tc.slog, entry.level, tc.zap)
		}
	}
}

func TestZapSlogEnabled(t *testing.T) {
	core := newCaptureCore(zapcore.WarnLevel)
	logger := slog.New(newZapSlogHandler(core))

	logger.Debug("dropped")
	logger.Info("dropped too")
	if n := len(core.all()); n != 0 {
		t.Fatalf("debug/info at warn threshold: %d entries, want 0", n)
	}
	logger.Warn("kept")
	logger.Error("kept too")
	if n := len(core.all()); n != 2 {
		t.Fatalf("warn/error at warn threshold: %d entries, want 2", n)
	}
}

func TestZapSlogAttrs(t *testing.T) {
	core := newCaptureCore(zapcore.DebugLevel)
	logger := slog.New(newZapSlogHandler(core))
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	logger.Info("all kinds",
		"b", true,
		"d", 1500*time.Millisecond,
		"f", 1.5,
		"i", int64(42),
		"s", "hello",
		"t", now,
		"u", uint64(7),
		"a", "any-value",
	)

	entry, ok := core.last()
	if !ok {
		t.Fatal("no entry captured")
	}
	assertFields(t, entry.fields, []zap.Field{
		zap.Bool("b", true),
		zap.Duration("d", 1500*time.Millisecond),
		zap.Float64("f", 1.5),
		zap.Int64("i", 42),
		zap.String("s", "hello"),
		zap.Time("t", now),
		zap.Uint64("u", 7),
		zap.Any("a", "any-value"),
	})
}

func TestZapSlogGroups(t *testing.T) {
	core := newCaptureCore(zapcore.DebugLevel)
	logger := slog.New(newZapSlogHandler(core))

	logger.Info("grouped",
		slog.Group("job",
			slog.String("kind", "playbook_run"),
			slog.Int("attempt", 2),
		),
	)

	entry, ok := core.last()
	if !ok {
		t.Fatal("no entry captured")
	}
	// slog.Int is a KindInt64, so the handler emits zap.Int64.
	assertFields(t, entry.fields, []zap.Field{
		zap.String("job.kind", "playbook_run"),
		zap.Int64("job.attempt", 2),
	})
}

func TestZapSlogWithGroupAndWithAttrs(t *testing.T) {
	core := newCaptureCore(zapcore.DebugLevel)
	handler := slog.Handler(newZapSlogHandler(core))

	handler = handler.WithGroup("outer").WithAttrs([]slog.Attr{slog.String("static", "yes")})
	logger := slog.New(handler)
	logger.Info("nested",
		slog.Group("inner", slog.String("leaf", "value")),
	)

	entry, ok := core.last()
	if !ok {
		t.Fatal("no entry captured")
	}
	// slog.WithAttrs places the attributes in the handler's current group,
	// so "static" lands under "outer" as well.
	assertFields(t, entry.fields, []zap.Field{
		zap.String("outer.static", "yes"),
		zap.String("outer.inner.leaf", "value"),
	})
}

// TestZapSlogHandlerFromLogger builds the handler the way Open does (from a
// zap logger's core) to make sure the wiring is what is actually shipped.
func TestZapSlogHandlerFromLogger(t *testing.T) {
	core := newCaptureCore(zapcore.DebugLevel)
	logger := zap.New(core)

	slogLogger := slog.New(newZapSlogHandler(logger.Core()))
	slogLogger.Warn("via zap logger")

	entry, ok := core.last()
	if !ok {
		t.Fatal("no entry captured")
	}
	if entry.level != zapcore.WarnLevel || entry.message != "via zap logger" {
		t.Errorf("entry = %+v", entry)
	}
}
