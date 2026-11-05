// Package telemetry exposes Prometheus metrics for the runner.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/vorash/vora/internal/version"
)

// Metrics holds the runner's Prometheus collectors. A nil *Metrics is valid
// and all methods are no-ops, so callers do not need to guard.
type Metrics struct {
	registry *prometheus.Registry

	BuildInfo             prometheus.Gauge
	SandboxCreateTotal    *prometheus.CounterVec
	SandboxCreateDuration prometheus.Histogram
	SandboxRunning        prometheus.Gauge
	SandboxFailedTotal    *prometheus.CounterVec
	ExecTotal             *prometheus.CounterVec
	ExecDuration          prometheus.Histogram
	ExecFailedTotal       *prometheus.CounterVec
	PolicyDeniedTotal     prometheus.Counter
	NetworkDeniedTotal    prometheus.Counter
	NetworkDecisionsTotal *prometheus.CounterVec
	AuditWriteFailedTotal prometheus.Counter
	DiskQuotaUnenforced   prometheus.Counter
	DiskQuotaEnforced     prometheus.Gauge
	RunnerCapacity        prometheus.Gauge
	RunnerSandboxes       prometheus.Gauge

	// Pool metrics observed per run. The pool label is "<tenant>/<name>", so
	// a series survives a delete and recreate; start is warm or cold.
	PoolRunsTotal     *prometheus.CounterVec
	PoolRunDuration   *prometheus.HistogramVec
	PoolStartup       *prometheus.HistogramVec
	PoolResetDuration *prometheus.HistogramVec
}

// PoolSample is one pool's state at scrape time, for the metrics that derive
// from the pool's own books rather than from a run.
type PoolSample struct {
	Pool        string
	States      map[string]int    // starting, idle, busy, resetting, retiring
	Retired     map[string]uint64 // by cause
	Exhausted   uint64
	StartFailed uint64
}

var (
	poolSandboxesDesc   = prometheus.NewDesc("vora_pool_sandboxes", "Pool members by state.", []string{"pool", "state"}, nil)
	poolRetiredDesc     = prometheus.NewDesc("vora_pool_retired_total", "Pool members destroyed, by cause.", []string{"pool", "cause"}, nil)
	poolExhaustedDesc   = prometheus.NewDesc("vora_pool_exhausted_total", "Pool runs refused because no member was free within the borrow timeout.", []string{"pool"}, nil)
	poolStartFailedDesc = prometheus.NewDesc("vora_pool_start_failed_total", "Pool member starts that failed.", []string{"pool"}, nil)
)

// poolCollector reads every pool's books at scrape time, so the gauges and
// counters below never drift from what the pool reports.
type poolCollector struct{ source func() []PoolSample }

func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolSandboxesDesc
	ch <- poolRetiredDesc
	ch <- poolExhaustedDesc
	ch <- poolStartFailedDesc
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	for _, p := range c.source() {
		for state, n := range p.States {
			ch <- prometheus.MustNewConstMetric(poolSandboxesDesc, prometheus.GaugeValue, float64(n), p.Pool, state)
		}
		for cause, n := range p.Retired {
			ch <- prometheus.MustNewConstMetric(poolRetiredDesc, prometheus.CounterValue, float64(n), p.Pool, cause)
		}
		ch <- prometheus.MustNewConstMetric(poolExhaustedDesc, prometheus.CounterValue, float64(p.Exhausted), p.Pool)
		ch <- prometheus.MustNewConstMetric(poolStartFailedDesc, prometheus.CounterValue, float64(p.StartFailed), p.Pool)
	}
}

// RegisterPoolSource exports the pool state metrics from source. Call it once
// per Metrics.
func (m *Metrics) RegisterPoolSource(source func() []PoolSample) {
	if m == nil {
		return
	}
	m.registry.MustRegister(poolCollector{source: source})
}

// Gather is the registry's Gather, for tests that read the exported values.
func (m *Metrics) Gather() ([]*dto.MetricFamily, error) { return m.registry.Gather() }

// New creates and registers the metric set.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	m.BuildInfo = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "vora_build_info",
		Help:        "Build information for the running Vora runner.",
		ConstLabels: prometheus.Labels{"version": version.String()},
	})
	m.BuildInfo.Set(1)
	m.SandboxCreateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_sandbox_create_total",
		Help: "Total sandbox create requests by status.",
	}, []string{"status"})
	m.SandboxCreateDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "vora_sandbox_create_duration_seconds",
		Help:    "Time to provision and start a sandbox.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	})
	m.SandboxRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_sandbox_running",
		Help: "Number of currently running sandboxes.",
	})
	m.SandboxFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_sandbox_failed_total",
		Help: "Total sandbox start failures by reason.",
	}, []string{"reason"})
	m.ExecTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_exec_total",
		Help: "Total executions by status.",
	}, []string{"status"})
	m.ExecDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "vora_exec_duration_seconds",
		Help:    "Execution duration.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 300},
	})
	m.ExecFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_exec_failed_total",
		Help: "Total failed executions by reason.",
	}, []string{"reason"})
	m.PolicyDeniedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vora_policy_denied_total",
		Help: "Total policy denials (reserved for v0.3).",
	})
	m.NetworkDeniedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vora_network_denied_total",
		Help: "Total denied network policy decisions.",
	})
	m.NetworkDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_network_decisions_total",
		Help: "Total network policy decisions by outcome and reason.",
	}, []string{"decision", "reason"})
	m.AuditWriteFailedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vora_audit_write_failed_total",
		Help: "Total audit records that could not be persisted.",
	})
	m.DiskQuotaUnenforced = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vora_disk_quota_unenforced_total",
		Help: "Total sandboxes created with a disk limit the host could not enforce.",
	})
	m.DiskQuotaEnforced = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_disk_quota_enforced",
		Help: "1 when the host enforces disk limits with project quotas, 0 otherwise.",
	})
	m.RunnerCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_runner_capacity",
		Help: "Runner capacity in CPU millis (0 if unknown).",
	})
	m.RunnerSandboxes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_runner_sandboxes",
		Help: "Number of sandboxes managed by this runner.",
	})
	m.PoolRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vora_pool_runs_total",
		Help: "Pool runs that reached a sandbox, by start (warm, cold) and exit reason.",
	}, []string{"pool", "start", "reason"})
	m.PoolRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vora_pool_run_duration_seconds",
		Help:    "Pool run duration from exec start to exit, excluding the reset.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 300},
	}, []string{"pool", "start"})
	m.PoolStartup = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vora_pool_startup_seconds",
		Help:    "What a pool run waited before its program ran: borrow wait, plus the cold start when cold.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"pool", "start"})
	m.PoolResetDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vora_pool_reset_duration_seconds",
		Help:    "Pool member reset duration by result (ok, failed).",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10},
	}, []string{"pool", "result"})

	reg.MustRegister(
		m.BuildInfo, m.SandboxCreateTotal, m.SandboxCreateDuration, m.SandboxRunning,
		m.SandboxFailedTotal, m.ExecTotal, m.ExecDuration, m.ExecFailedTotal,
		m.PolicyDeniedTotal, m.NetworkDeniedTotal, m.NetworkDecisionsTotal,
		m.AuditWriteFailedTotal, m.DiskQuotaUnenforced, m.DiskQuotaEnforced,
		m.RunnerCapacity, m.RunnerSandboxes,
		m.PoolRunsTotal, m.PoolRunDuration, m.PoolStartup, m.PoolResetDuration,
	)
	return m
}

// Handler returns the Prometheus scrape handler.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Serve starts an HTTP server exposing /metrics. It returns when ctx is done.
func (m *Metrics) Serve(ctx context.Context, addr string, logger *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(done)
	}()

	if logger != nil {
		logger.Info("metrics server listening", "addr", addr)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}
