// Prometheus-compatible scrape endpoint from the current in-process state of
// the master. No metrics library is needed: the exposition format is small and
// stable, and values are computed on demand from the store and worker manager.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

const contentType = "text/plain; version=0.0.4; charset=utf-8"

var processStart = time.Now().UTC()

// metricsHandler renders one scrape of the current process state. The endpoint
// is unauthenticated: it exposes aggregate counts only, never host names or
// playbook output.
func metricsHandler(st db.Store, workers WorkerReporter, logger *zap.Logger) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out exposition
		var stats *db.Stats
		var err error
		if st == nil {
			err = errors.New("no store configured")
		} else {
			stats, err = st.Stats(r.Context())
		}
		if err != nil {
			logger.Warn("metrics: cannot read store stats", zap.Error(err))
			out.metric("strangle_store_up", "gauge", "Whether the run store is reachable (1) or not (0).",
				[]sample{{value: 0}})
		} else {
			out.writeStats(stats)
		}

		out.writeWorkers(r.Context(), workers)
		out.writeRuntime()

		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, out.String())
	})
}

type label struct{ name, value string }

type sample struct {
	labels []label
	value  float64
}

type exposition struct{ strings.Builder }

func (e *exposition) metric(name, typ, help string, samples []sample) {
	fmt.Fprintf(e, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	for _, s := range samples {
		e.WriteString(name)
		if len(s.labels) > 0 {
			e.WriteByte('{')
			for i, l := range s.labels {
				if i > 0 {
					e.WriteByte(',')
				}
				fmt.Fprintf(e, "%s=%s", l.name, quote(l.value))
			}
			e.WriteByte('}')
		}
		e.WriteByte(' ')
		e.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
		e.WriteByte('\n')
	}
}

func (e *exposition) gauge(name, help string, value float64) {
	e.metric(name, "gauge", help, []sample{{value: value}})
}

func (e *exposition) writeStats(stats *db.Stats) {
	e.gauge("strangle_store_up", "Whether the run store is reachable (1) or not (0).", 1)
	e.metric("strangle_build_info", "gauge", "Build information (always 1).", []sample{{
		labels: []label{{"version", "dev"}}, value: 1,
	}})

	// Run counts are gauges, not counters: retention can purge old rows and
	// legitimately reduce them between scrapes.
	runs := stats.Runs
	e.gauge("strangle_runs", "Number of recorded runs.", float64(runs.Total))
	e.gauge("strangle_runs_succeeded", "Number of successful runs.", float64(runs.Succeeded))
	e.gauge("strangle_runs_failed", "Number of failed runs.", float64(runs.Failed))
	e.gauge("strangle_run_duration_ms_avg", "Average recorded run duration in milliseconds.", float64(runs.AvgDurationMS))
	e.gauge("strangle_last_run_timestamp_seconds", "Unix timestamp of the most recent run.", unixSeconds(runs.LastRunAt))

	hosts := stats.Hosts
	e.metric("strangle_hosts", "gauge", "Number of hosts by state.", []sample{
		{labels: []label{{"state", "total"}}, value: float64(hosts.Total)},
		{labels: []label{{"state", "in_inventory"}}, value: float64(hosts.InInventory)},
		{labels: []label{{"state", "active"}}, value: float64(hosts.Active)},
		{labels: []label{{"state", "disappeared"}}, value: float64(hosts.Disappeared)},
	})

	playbooks := append([]db.PlaybookStats(nil), stats.PerPlaybook...)
	sort.Slice(playbooks, func(i, j int) bool {
		if playbooks[i].Target != playbooks[j].Target {
			return playbooks[i].Target < playbooks[j].Target
		}
		return playbooks[i].Playbook < playbooks[j].Playbook
	})
	totalByPlaybook := make([]sample, 0, len(playbooks))
	failedByPlaybook := make([]sample, 0, len(playbooks))
	for _, pb := range playbooks {
		labels := []label{
			{"collection", pb.Target},
			{"playbook", pb.Playbook},
		}
		totalByPlaybook = append(totalByPlaybook, sample{labels: labels, value: float64(pb.Total)})
		failedByPlaybook = append(failedByPlaybook, sample{labels: labels, value: float64(pb.Failed)})
	}
	e.metric("strangle_playbook_runs", "gauge", "Recorded runs per collection and playbook.", totalByPlaybook)
	e.metric("strangle_playbook_failed_runs", "gauge", "Failed runs per collection and playbook.", failedByPlaybook)
}

func (e *exposition) writeWorkers(ctx context.Context, workers WorkerReporter) {
	var statuses []worker.WorkerStatus
	if workers != nil {
		statuses = workers.WorkerStatuses(ctx)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	alive, dead := 0, 0
	jobs, restarts := make([]sample, 0, len(statuses)), make([]sample, 0, len(statuses))
	for _, ws := range statuses {
		if ws.Alive {
			alive++
		} else {
			dead++
		}
		labels := []label{
			{"worker", strconv.Itoa(ws.ID)},
			{"collection", ws.Collection},
		}
		jobs = append(jobs, sample{labels: labels, value: float64(ws.Jobs)})
		restarts = append(restarts, sample{labels: labels, value: float64(ws.Restarts)})
	}
	e.metric("strangle_workers", "gauge", "Number of worker slots by state.", []sample{
		{labels: []label{{"state", "total"}}, value: float64(len(statuses))},
		{labels: []label{{"state", "alive"}}, value: float64(alive)},
		{labels: []label{{"state", "dead"}}, value: float64(dead)},
	})
	e.metric("strangle_worker_jobs", "gauge", "In-flight jobs assigned to each worker slot.", jobs)
	e.metric("strangle_worker_restarts_total", "counter", "Process restarts of each worker slot.", restarts)
}

func (e *exposition) writeRuntime() {
	e.gauge("process_start_time_seconds", "Start time of the process since unix epoch in seconds.",
		float64(processStart.Unix()))
	e.gauge("go_goroutines", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()))

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	e.gauge("go_memstats_alloc_bytes", "Number of bytes allocated and still in use.", float64(m.Alloc))
	e.gauge("go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and still in use.", float64(m.HeapAlloc))
	e.gauge("go_memstats_heap_sys_bytes", "Number of heap bytes obtained from system.", float64(m.HeapSys))
	e.gauge("go_memstats_sys_bytes", "Number of bytes obtained from system.", float64(m.Sys))
	e.gauge("go_memstats_gc_sys_bytes", "Number of bytes used for garbage collection system metadata.", float64(m.GCSys))
	e.gauge("go_memstats_gc_cpu_fraction", "The fraction of this program's available CPU time used by the GC.", m.GCCPUFraction)
}

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// quote escapes a label value per the Prometheus text exposition format.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
