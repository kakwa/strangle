// Package applier contains the logic for applying to job postings.
package applier

import (
	"context"
	"log/slog"
	"time"
)

// Job is a job posting to apply to.
type Job struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Company string `json:"company"`
}

// Report is the outcome of a single application attempt.
type Report struct {
	JobID     string    `json:"job_id"`
	JobTitle  string    `json:"job_title"`
	Company   string    `json:"company"`
	AppliedAt time.Time `json:"applied_at"`
	Success   bool      `json:"success"`
	Message   string    `json:"message"`
}

// Applier applies to job postings.
type Applier interface {
	Apply(ctx context.Context, job Job) (Report, error)
}

// Stub is a placeholder Applier that pretends to submit applications.
// TODO: replace with a real implementation.
type Stub struct {
	logger *slog.Logger
}

// NewStub returns a Stub applier.
func NewStub(logger *slog.Logger) *Stub {
	return &Stub{logger: logger}
}

// Apply simulates applying to a job.
func (a *Stub) Apply(ctx context.Context, job Job) (Report, error) {
	select {
	case <-ctx.Done():
		return Report{}, ctx.Err()
	case <-time.After(100 * time.Millisecond): // simulate work
	}

	a.logger.Info("applied to job", "job", job.ID, "company", job.Company)
	return Report{
		JobID:     job.ID,
		JobTitle:  job.Title,
		Company:   job.Company,
		AppliedAt: time.Now().UTC(),
		Success:   true,
		Message:   "stub: application submitted",
	}, nil
}
