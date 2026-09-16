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
)

// Metrics holds the runner's Prometheus collectors. A nil *Metrics is valid
// and all methods are no-ops, so callers do not need to guard.
type Metrics struct {
	registry *prometheus.Registry

	SandboxCreateTotal    *prometheus.CounterVec
	SandboxCreateDuration prometheus.Histogram
	SandboxRunning        prometheus.Gauge
	SandboxFailedTotal    *prometheus.CounterVec
	ExecTotal             *prometheus.CounterVec
	ExecDuration          prometheus.Histogram
	ExecFailedTotal       *prometheus.CounterVec
	PolicyDeniedTotal     prometheus.Counter
	NetworkDeniedTotal    prometheus.Counter
	RunnerCapacity        prometheus.Gauge
	RunnerSandboxes       prometheus.Gauge
}

// New creates and registers the metric set.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

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
		Help: "Total network denials (reserved for v0.3).",
	})
	m.RunnerCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_runner_capacity",
		Help: "Runner capacity in CPU millis (0 if unknown).",
	})
	m.RunnerSandboxes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vora_runner_sandboxes",
		Help: "Number of sandboxes managed by this runner.",
	})

	reg.MustRegister(
		m.SandboxCreateTotal, m.SandboxCreateDuration, m.SandboxRunning,
		m.SandboxFailedTotal, m.ExecTotal, m.ExecDuration, m.ExecFailedTotal,
		m.PolicyDeniedTotal, m.NetworkDeniedTotal, m.RunnerCapacity, m.RunnerSandboxes,
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
