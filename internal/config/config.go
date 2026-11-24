// Package config holds the runtime configuration for strangle,
// loaded from environment variables.
package config

import (
	"os"
	"time"
)

// Config holds all runtime configuration for strangle.
type Config struct {
	// ListenAddr is the address the HTTP server listens on.
	ListenAddr string
	// Interval is how often the applier runs over the job list.
	Interval time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		ListenAddr: env("STRANGLE_ADDR", ":8080"),
		Interval:   envDuration("STRANGLE_INTERVAL", 15*time.Minute),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
