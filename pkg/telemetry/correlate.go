package telemetry

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"

	"go.opentelemetry.io/otel/trace"
)

// Keys of the attributes CorrelationHandler adds.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
	// CallerKey matches zerolog's own field name for the same thing.
	CallerKey = "caller"
)

// CorrelationHandler wraps a slog.Handler and adds to every record the two
// things needed to place a log line — the active span's identifiers, so it can
// be joined to the trace it happened in, and the call site that produced it.
//
// Both come from parts of the record that a back-end handler is free to ignore:
// the context and Record.PC. zerolog's slog handler ignores the PC entirely, so
// without this the call site would be lost.
//
// The span is read from the context, so it is only found when the caller passes
// one: use the *Context methods (logger.InfoContext) or logger.LogAttrs, not
// logger.Info.
type CorrelationHandler struct {
	inner slog.Handler
	// qualifiers records the WithGroup and WithAttrs calls made after the first
	// WithGroup. They cannot be pushed down into inner, because an open group
	// would swallow the correlation attributes too. Handle replays them around
	// the record's own attributes instead, which keeps trace_id and span_id at
	// the top level where a log backend expects to find them.
	qualifiers []qualifier
}

// qualifier is either a group (group != "") or a set of attributes.
type qualifier struct {
	group string
	attrs []slog.Attr
}

var _ slog.Handler = (*CorrelationHandler)(nil)

// NewCorrelationHandler wraps inner. It must be the outermost handler for the
// correlation attributes to stay at the top level of each record.
func NewCorrelationHandler(inner slog.Handler) *CorrelationHandler {
	return &CorrelationHandler{inner: inner}
}

// Enabled defers entirely to the wrapped handler.
func (h *CorrelationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle stamps the call site and the active span's identifiers onto the record
// and passes it on.
func (h *CorrelationHandler) Handle(ctx context.Context, rec slog.Record) error {
	spanCtx := trace.SpanContextFromContext(ctx)
	caller, hasCaller := callerOf(rec.PC)
	if !spanCtx.IsValid() && !hasCaller && len(h.qualifiers) == 0 {
		return h.inner.Handle(ctx, rec) // Nothing to add and nothing deferred.
	}

	attrs := make([]slog.Attr, 0, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	// Rebuild the deferred qualifiers from the inside out: a group nests
	// everything accumulated so far, a set of attributes prefixes it.
	for _, q := range slices.Backward(h.qualifiers) {
		if q.group != "" {
			if len(attrs) == 0 {
				continue // slog elides empty groups.
			}
			attrs = []slog.Attr{{Key: q.group, Value: slog.GroupValue(attrs...)}}
			continue
		}
		attrs = append(slices.Clone(q.attrs), attrs...)
	}

	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	if hasCaller {
		out.AddAttrs(slog.String(CallerKey, caller))
	}
	if spanCtx.IsValid() {
		out.AddAttrs(
			slog.String(TraceIDKey, spanCtx.TraceID().String()),
			slog.String(SpanIDKey, spanCtx.SpanID().String()),
		)
	}
	out.AddAttrs(attrs...)
	return h.inner.Handle(ctx, out)
}

// callerOf renders the program counter slog captured at the log call site as
// dir/file:line. Resolving it here rather than letting the back end do it is
// what keeps the frame pointing at the caller instead of at this handler.
func callerOf(pc uintptr) (string, bool) {
	if pc == 0 {
		return "", false
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	if frame.File == "" {
		return "", false
	}
	dir, file := filepath.Split(frame.File)
	return filepath.Join(filepath.Base(dir), file) + ":" + strconv.Itoa(frame.Line), true
}

// WithAttrs returns a handler that still correlates. Attributes added before any
// group go straight to the wrapped handler; later ones are deferred to Handle.
func (h *CorrelationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// With no group open these are top-level, so inner can own them; that keeps
	// the fast path in Handle available for the common case.
	if len(h.qualifiers) == 0 {
		return &CorrelationHandler{inner: h.inner.WithAttrs(slices.Clone(attrs))}
	}
	return &CorrelationHandler{
		inner:      h.inner,
		qualifiers: append(slices.Clone(h.qualifiers), qualifier{attrs: slices.Clone(attrs)}),
	}
}

// WithGroup records the group rather than opening it on the wrapped handler, so
// that Handle can keep the correlation attributes outside it.
func (h *CorrelationHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &CorrelationHandler{
		inner:      h.inner,
		qualifiers: append(slices.Clone(h.qualifiers), qualifier{group: name}),
	}
}
