package function

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Metrics are pushed over OTLP rather than scraped: Cloud Run scales to zero
// and throttles CPU between requests, so nothing can come and read a /metrics
// endpoint, and a background exporter would never get to run. Every handler
// therefore flushes before it returns.
//
// The exporter reads OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS
// itself; with no endpoint set, every instrument is a no-op.

const (
	serviceName = "gcrunner"

	// Cumulative counters arrive at Prometheus already at their current value.
	// Its created-timestamp ingestion writes the missing zero, but only when
	// the series' start time is recent. The SDK's default start time is the
	// instrument's creation, hours old on a long-lived instance; this flag
	// stamps each series with the time of its own first observation instead.
	perSeriesStartTimestampsEnv = "OTEL_GO_X_PER_SERIES_START_TIMESTAMPS"

	// One export attempt, no retries: a request waits for the flush, so an
	// unreachable collector must cost it one bounded round trip and nothing more.
	exportTimeout = 3 * time.Second
)

// Attribute keys shared by the instruments below.
const (
	attributeRepository  = "repo_full_name"
	attributeWorkflow    = "workflow_name"
	attributeStatus      = "status"
	attributeConclusion  = "conclusion"
	attributeZone        = "zone"
	attributeMachineType = "machine_type"
	attributeSpot        = "spot"
	attributeOutcome     = "outcome"
	attributeTask        = "task"
)

// Outcomes of one VM creation attempt in one zone.
const (
	outcomeCreated       = "created"
	outcomeQuota         = "quota"
	outcomeRetryable     = "retryable"
	outcomeFatal         = "fatal"
	outcomePermanent     = "permanent"
	outcomeAlreadyExists = "already_exists"
	outcomeUnresolved    = "unresolved"
)

// Outcomes of one Cloud Tasks attempt.
const (
	taskOutcomeOK        = "ok"
	taskOutcomeError     = "error"
	taskOutcomePermanent = "permanent"
)

type instruments struct {
	jobs          metric.Int64Counter
	queueDuration metric.Float64Histogram
	jobDuration   metric.Float64Histogram
	vmCreates     metric.Int64Counter
	tasks         metric.Int64Counter
	taskRetries   metric.Int64Histogram
}

type telemetry struct {
	provider *sdkmetric.MeterProvider
	// A one-slot semaphore rather than a mutex, so a request waiting its turn
	// to flush can give up when its own context ends.
	flushing chan struct{}
	instruments
}

var (
	telemetryMu   sync.Mutex
	telemetryInst *telemetry
)

// getTelemetry builds the instruments on first use. Set up lazily because the
// Cloud Run instance id comes from the metadata server, which the tests do
// not have; tests install their own instance instead.
func getTelemetry() *telemetry {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	if telemetryInst == nil {
		telemetryInst = newTelemetry(context.Background())
	}
	return telemetryInst
}

func newTelemetry(ctx context.Context) *telemetry {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		return &telemetry{instruments: newInstruments(noop.NewMeterProvider())}
	}
	if os.Getenv(perSeriesStartTimestampsEnv) == "" {
		os.Setenv(perSeriesStartTimestampsEnv, "true")
	}

	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithTimeout(exportTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
	if err != nil {
		log.Printf("ERROR: metrics exporter unavailable, metrics disabled: %v", err)
		return &telemetry{instruments: newInstruments(noop.NewMeterProvider())}
	}

	// A long interval, because the flush at the end of each request is what
	// actually exports. The reader only has to exist for ForceFlush to work.
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(newResource(ctx)),
	)
	return withProvider(provider)
}

func withProvider(provider *sdkmetric.MeterProvider) *telemetry {
	return &telemetry{provider: provider, flushing: make(chan struct{}, 1), instruments: newInstruments(provider)}
}

// newResource identifies this instance. Every Cloud Run instance must carry
// its own service.instance.id, or concurrent instances write the same series
// with values that disagree.
func newResource(ctx context.Context) *resource.Resource {
	attributes := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceNamespace(os.Getenv("GCP_PROJECT")),
	}
	if metadata.OnGCE() {
		if id, err := metadata.InstanceIDWithContext(ctx); err == nil {
			attributes = append(attributes, semconv.ServiceInstanceID(id))
		}
	}
	r, err := resource.New(ctx, resource.WithAttributes(attributes...), resource.WithFromEnv())
	if err != nil {
		log.Printf("Partial metrics resource: %v", err)
	}
	return r
}

func newInstruments(provider metric.MeterProvider) instruments {
	meter := provider.Meter(serviceName)
	jobs, _ := meter.Int64Counter("gcrunner.jobs",
		metric.WithUnit("{job}"),
		metric.WithDescription("workflow_job webhooks for gcrunner jobs, by status and conclusion"))
	queueDuration, _ := meter.Float64Histogram("gcrunner.queue.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time from a job being queued on GitHub to it starting on a runner"),
		metric.WithExplicitBucketBoundaries(15, 30, 60, 90, 120, 180, 300, 600, 1200, 1800, 3600))
	jobDuration, _ := meter.Float64Histogram("gcrunner.job.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time from a job starting on a runner to it completing"),
		metric.WithExplicitBucketBoundaries(30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200))
	vmCreates, _ := meter.Int64Counter("gcrunner.vm.creates",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("VM creation attempts, one per zone tried, by outcome"))
	tasks, _ := meter.Int64Counter("gcrunner.tasks",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Cloud Tasks attempts by outcome"))
	taskRetries, _ := meter.Int64Histogram("gcrunner.task.retries",
		metric.WithUnit("{retry}"),
		metric.WithDescription("How many times Cloud Tasks had already retried the task being handled"),
		metric.WithExplicitBucketBoundaries(0, 1, 2, 3, 5, 8, 12, 18, 25, 40))
	return instruments{
		jobs:          jobs,
		queueDuration: queueDuration,
		jobDuration:   jobDuration,
		vmCreates:     vmCreates,
		tasks:         tasks,
		taskRetries:   taskRetries,
	}
}

// recordJob counts a workflow_job webhook and, where the payload carries the
// timestamps, the time the job spent queued or running.
func recordJob(ctx context.Context, event WorkflowJobEvent) {
	t := getTelemetry()
	job := event.WorkflowJob
	attributes := []attribute.KeyValue{
		attribute.String(attributeRepository, event.Repository.FullName),
		attribute.String(attributeWorkflow, job.WorkflowName),
		attribute.String(attributeStatus, event.Action),
		attribute.String(attributeConclusion, job.Conclusion),
	}
	t.jobs.Add(ctx, 1, metric.WithAttributes(attributes...))

	timing := metric.WithAttributes(
		attribute.String(attributeRepository, event.Repository.FullName),
		attribute.String(attributeWorkflow, job.WorkflowName),
	)
	switch event.Action {
	case "in_progress":
		if !job.CreatedAt.IsZero() && !job.StartedAt.IsZero() {
			t.queueDuration.Record(ctx, job.StartedAt.Sub(job.CreatedAt).Seconds(), timing)
		}
	case "completed":
		if !job.StartedAt.IsZero() && !job.CompletedAt.IsZero() {
			t.jobDuration.Record(ctx, job.CompletedAt.Sub(job.StartedAt).Seconds(), timing)
		}
	}
}

// recordVMCreate counts one attempt to create a VM in one zone.
func recordVMCreate(ctx context.Context, zone, machineType string, spot bool, outcome string) {
	getTelemetry().vmCreates.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attributeZone, zone),
		attribute.String(attributeMachineType, machineType),
		attribute.Bool(attributeSpot, spot),
		attribute.String(attributeOutcome, outcome),
	))
}

// recordTask counts one Cloud Tasks attempt and how deep into its retries the
// task was. A queue full of high retry counts is the shape of an outage that
// otherwise looks like a quiet day: nothing fails, jobs just never start.
func recordTask(ctx context.Context, task, retryHeader, outcome string) {
	t := getTelemetry()
	retries, _ := strconv.ParseInt(retryHeader, 10, 64)
	t.tasks.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attributeTask, task),
		attribute.String(attributeOutcome, outcome),
	))
	t.taskRetries.Record(ctx, retries, metric.WithAttributes(attribute.String(attributeTask, task)))
}

// flushMetrics pushes everything recorded so far. Called at the end of each
// request that recorded something, because the instance may be frozen or
// gone before a periodic export would run. Flushes are serialised so two
// requests do not export the same cumulative values with disagreeing
// timestamps, and a request only waits for its turn while its own context
// lasts: what it recorded is still in the SDK, and the next flush carries it.
func flushMetrics(ctx context.Context) {
	t := getTelemetry()
	if t.provider == nil {
		return
	}
	select {
	case t.flushing <- struct{}{}:
	case <-ctx.Done():
		log.Printf("Skipping metrics flush, request over: %v", ctx.Err())
		return
	}
	defer func() { <-t.flushing }()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportTimeout)
	defer cancel()
	if err := t.provider.ForceFlush(ctx); err != nil {
		log.Printf("ERROR: metrics flush failed: %v", err)
	}
}

// ShutdownTelemetry flushes and stops the exporter. Cloud Run sends SIGTERM
// before it stops an instance; this is the last chance to push what the
// final requests recorded.
func ShutdownTelemetry(ctx context.Context) {
	t := getTelemetry()
	if t.provider == nil {
		return
	}
	if err := t.provider.Shutdown(ctx); err != nil {
		log.Printf("ERROR: metrics shutdown failed: %v", err)
	}
}
