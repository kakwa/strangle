// Package logging builds strangle's zap loggers.
package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds a JSON logger on stderr at the given level.
func New(level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	switch level {
	case "debug":
		lvl = zap.DebugLevel
	case "info":
		lvl = zap.InfoLevel
	case "warn":
		lvl = zap.WarnLevel
	case "error":
		lvl = zap.ErrorLevel
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	conf := zap.NewProductionConfig()
	conf.Level = zap.NewAtomicLevelAt(lvl)
	conf.EncoderConfig.TimeKey = "ts"
	return conf.Build()
}
