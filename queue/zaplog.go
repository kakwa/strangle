package queue

import (
	"context"
	"log/slog"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// zapSlogHandler adapts a zap core to slog.Handler so the River client's
// *slog.Logger emits into strangle's zap logger instead of the standard
// library logging stack.
type zapSlogHandler struct {
	core   zapcore.Core
	groups []string
	// fields are the zap fields of the attributes captured by WithAttrs,
	// resolved (group prefix included) at the moment they were added.
	fields []zap.Field
}

func newZapSlogHandler(core zapcore.Core) *zapSlogHandler {
	return &zapSlogHandler{core: core}
}

func (h *zapSlogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.core.Enabled(zapcoreLevel(level))
}

func (h *zapSlogHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make([]zap.Field, 0, len(h.fields)+record.NumAttrs())
	fields = append(fields, h.fields...)
	record.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, toFields(h.groups, attr)...)
		return true
	})

	entry := zapcore.Entry{
		Level:   zapcoreLevel(record.Level),
		Time:    record.Time,
		Message: record.Message,
	}
	if ce := h.core.Check(entry, nil); ce != nil {
		ce.Write(fields...)
	}
	return nil
}

func (h *zapSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.fields = append(append([]zap.Field{}, h.fields...), toFields(h.groups, attrs...)...)
	return &clone
}

func (h *zapSlogHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

// zapcoreLevel maps a slog level onto the nearest zap level.
func zapcoreLevel(level slog.Level) zapcore.Level {
	switch {
	case level >= slog.LevelError:
		return zapcore.ErrorLevel
	case level >= slog.LevelWarn:
		return zapcore.WarnLevel
	case level >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}

// toFields converts slog attributes (and any nested group attributes) to zap
// fields, flattening group keys with dots.
func toFields(groups []string, attrs ...slog.Attr) []zap.Field {
	fields := make([]zap.Field, 0, len(attrs))
	for _, attr := range attrs {
		fields = append(fields, toField(groups, attr)...)
	}
	return fields
}

func toField(groups []string, attr slog.Attr) []zap.Field {
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(append(groups, attr.Key), ".")
	}

	if attr.Value.Kind() == slog.KindGroup {
		sub := append(append([]string{}, groups...), attr.Key)
		fields := []zap.Field{}
		for _, nested := range attr.Value.Group() {
			fields = append(fields, toFields(sub, nested)...)
		}
		return fields
	}

	switch attr.Value.Kind() {
	case slog.KindBool:
		return []zap.Field{zap.Bool(key, attr.Value.Bool())}
	case slog.KindDuration:
		return []zap.Field{zap.Duration(key, attr.Value.Duration())}
	case slog.KindFloat64:
		return []zap.Field{zap.Float64(key, attr.Value.Float64())}
	case slog.KindInt64:
		return []zap.Field{zap.Int64(key, attr.Value.Int64())}
	case slog.KindString:
		return []zap.Field{zap.String(key, attr.Value.String())}
	case slog.KindTime:
		return []zap.Field{zap.Time(key, attr.Value.Time())}
	case slog.KindUint64:
		return []zap.Field{zap.Uint64(key, attr.Value.Uint64())}
	default:
		return []zap.Field{zap.Any(key, attr.Value.Any())}
	}
}
