package db

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UUIDv7 embeds the Unix epoch in milliseconds in its first 48 bits, so byte
// order is chronological order. The library also keeps IDs monotonic within
// one millisecond.
func TestAnsibleRunUUIDv7(t *testing.T) {
	var previous uuid.UUID
	for i := 0; i < 100; i++ {
		run := AnsibleRun{}
		if err := run.BeforeCreate(&gorm.DB{}); err != nil {
			t.Fatalf("BeforeCreate: %v", err)
		}
		if run.ID.Version() != 7 {
			t.Fatalf("id %s: version = %d, want 7", run.ID, run.ID.Version())
		}
		if previous != uuid.Nil && bytes.Compare(run.ID[:], previous[:]) <= 0 {
			t.Fatalf("id %s is not after previous %s", run.ID, previous)
		}
		previous = run.ID
	}

	fixed := uuid.New()
	run := AnsibleRun{ID: fixed}
	if err := run.BeforeCreate(&gorm.DB{}); err != nil {
		t.Fatalf("BeforeCreate: %v", err)
	}
	if run.ID != fixed {
		t.Fatalf("BeforeCreate replaced explicit id %s with %s", fixed, run.ID)
	}
}
