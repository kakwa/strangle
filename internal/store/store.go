// Package store keeps job application reports in memory.
// TODO: replace with a persistent store.
package store

import (
	"sync"

	"github.com/kakwa/strangle/internal/applier"
)

// Store is a concurrency-safe, bounded in-memory collection of reports.
type Store struct {
	mu       sync.RWMutex
	reports  []applier.Report
	capacity int
}

// New returns a Store keeping at most capacity reports.
func New(capacity int) *Store {
	if capacity <= 0 {
		capacity = 100
	}
	return &Store{capacity: capacity}
}

// Add appends a report, evicting the oldest one if the store is full.
func (s *Store) Add(r applier.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
	if len(s.reports) > s.capacity {
		s.reports = s.reports[len(s.reports)-s.capacity:]
	}
}

// List returns all reports, newest first.
func (s *Store) List() []applier.Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]applier.Report, 0, len(s.reports))
	for i := len(s.reports) - 1; i >= 0; i-- {
		out = append(out, s.reports[i])
	}
	return out
}
