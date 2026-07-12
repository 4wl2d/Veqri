package observability

import (
	"context"
	"sync/atomic"
)

// Tracer is intentionally compatible with OpenTelemetry's start/end model
// without forcing domain packages to depend on one telemetry SDK.
type Tracer interface {
	Start(ctx context.Context, name string, attributes ...Attribute) (context.Context, Span)
}

type Span interface {
	SetAttributes(attributes ...Attribute)
	End(err error)
}

type Attribute struct {
	Key   string
	Value any
}

type NoopTracer struct{}
type noopSpan struct{}

func (NoopTracer) Start(ctx context.Context, _ string, _ ...Attribute) (context.Context, Span) {
	return ctx, noopSpan{}
}
func (noopSpan) SetAttributes(...Attribute) {}
func (noopSpan) End(error)                  {}

type Counters struct {
	EventsIngested     atomic.Uint64
	TasksStarted       atomic.Uint64
	TasksCompleted     atomic.Uint64
	TasksFailed        atomic.Uint64
	ApprovalsRequested atomic.Uint64
	ToolInvocations    atomic.Uint64
	Deliveries         atomic.Uint64
}
