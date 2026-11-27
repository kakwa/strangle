package logger

import "testing"

func TestNew(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		l, err := New(lvl)
		if err != nil {
			t.Fatalf("New(%q) error: %v", lvl, err)
		}
		if l == nil {
			t.Fatalf("New(%q) returned a nil logger", lvl)
		}
		l.Sync()
	}
}

func TestNewUnknownLevel(t *testing.T) {
	for _, lvl := range []string{"", "verbose", "INFO", "Info"} {
		if l, err := New(lvl); err == nil {
			t.Errorf("New(%q) = %v, want error", lvl, l)
		}
	}
}
