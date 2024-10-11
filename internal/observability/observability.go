package observability

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"os"
)

type Metrics struct {
	Registry         *prometheus.Registry
	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	Updates          prometheus.Counter
	Connections      prometheus.Gauge
	StreamingUpdates prometheus.Counter
	DeliveryFailures prometheus.Counter
	Pending          prometheus.Gauge
	OutboxFailures   prometheus.Counter
	Evaluations      *prometheus.CounterVec
	Propagation      prometheus.Histogram
}

func New() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry(),
		Requests:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flagforge_http_requests_total", Help: "Requests by bounded route and status."}, []string{"route", "status"}),
		Duration:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "flagforge_http_request_duration_seconds", Help: "HTTP duration by route."}, []string{"route"}),
		Updates:          prometheus.NewCounter(prometheus.CounterOpts{Name: "flagforge_flag_updates_total", Help: "Committed flag configuration mutations."}),
		Connections:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "flagforge_sdk_connections", Help: "Active SSE connections."}),
		StreamingUpdates: prometheus.NewCounter(prometheus.CounterOpts{Name: "flagforge_streaming_updates_total", Help: "SSE notifications written."}),
		DeliveryFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "flagforge_streaming_delivery_failures_total", Help: "SSE write failures."}),
		Pending:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "flagforge_outbox_pending", Help: "Pending transactional outbox rows."}),
		OutboxFailures:   prometheus.NewCounter(prometheus.CounterOpts{Name: "flagforge_outbox_failures_total", Help: "Failed outbox batches."}),
		Evaluations:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flagforge_flag_evaluations_total", Help: "Best-effort reported SDK evaluations."}, []string{"reason"}),
		Propagation:      prometheus.NewHistogram(prometheus.HistogramOpts{Name: "flagforge_propagation_latency_seconds", Help: "Commit to gateway notification latency.", Buckets: prometheus.ExponentialBuckets(.001, 2, 16)}),
	}
	m.Registry.MustRegister(m.Requests, m.Duration, m.Updates, m.Connections, m.StreamingUpdates, m.DeliveryFailures, m.Pending, m.OutboxFailures, m.Evaluations, m.Propagation, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func Tracing(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	p := trace.NewTracerProvider(trace.WithBatcher(exporter), trace.WithResource(resource.NewWithAttributes("", attribute.String("service.name", "flagforge"))))
	otel.SetTracerProvider(p)
	return p.Shutdown, nil
}
