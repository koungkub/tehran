package telemetry

import (
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// Keys of the fields correlationHook adds, plus the one zerolog's own caller hook
// adds when LogConfig.Caller is set.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
	// CallerKey names the field zerolog's caller hook writes. It duplicates
	// zerolog.CallerFieldName, which is a var rather than a constant, and is here
	// so a consumer parsing records has one place to read every key this package
	// puts on them. TestCallerKeyMatchesZerolog fails if the two diverge.
	CallerKey = "caller"
)

// correlationHook adds to every record the active span's identifiers, so a log
// line can be joined to the trace it happened in.
//
// The span is read from the event's context, so it is only found when the caller
// attaches one with Event.Ctx. A record logged without one is still emitted; it
// just carries no trace_id.
//
// The call site is not this hook's business: zerolog resolves that itself, from
// the call site's own stack depth, which is both cheaper and less fragile than
// anything a hook can do — see NewLogger.
type correlationHook struct{}

var _ zerolog.Hook = correlationHook{}

func (correlationHook) Run(e *zerolog.Event, _ zerolog.Level, _ string) {
	if spanCtx := trace.SpanContextFromContext(e.GetCtx()); spanCtx.IsValid() {
		e.Str(TraceIDKey, spanCtx.TraceID().String()).
			Str(SpanIDKey, spanCtx.SpanID().String())
	}
}
