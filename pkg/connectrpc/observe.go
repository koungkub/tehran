package connectrpc

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// accountedKey carries a flag saying this request has been accounted for, so the
// surrounding middleware knows not to report it as unserved.
type accountedKey struct{}

// markAccounted records that this request needs no report from the middleware,
// either because an interceptor already logged it as an RPC or because it is an
// endpoint deliberately kept off the logs.
func markAccounted(ctx context.Context) {
	if accounted, ok := ctx.Value(accountedKey{}).(*atomic.Bool); ok {
		accounted.Store(true)
	}
}

// accountedFor marks everything h serves as accounted for, without logging it.
//
// Health and reflection are mounted without the handler options that carry the
// interceptors, so nothing downstream ever reports them. That is deliberate — an
// orchestrator's health check runs every few seconds and is not worth a log line
// each — but it means the rejection middleware would otherwise accuse every one
// of them of never being served.
func accountedFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markAccounted(r.Context())
		h.ServeHTTP(w, r)
	})
}

// newRejectionLogger reports requests that reached the server but never became
// an RPC, which is the one class of failure no interceptor can see.
//
// Connect reads and decodes the request message before it runs the interceptor
// chain, so anything that fails earlier — a body cut off by ReadTimeout above
// all, but also an unroutable path or an undecodable message — is invisible to
// both the logging and the tracing interceptor. A slow-body client would
// otherwise pin streams while leaving no trace at all.
//
// It deliberately does not wrap the ResponseWriter, so it cannot report a status
// code: connect requires the ResponseWriter to implement http.Flusher and checks
// with a direct type assertion rather than through http.ResponseController, so a
// wrapper that did not reimplement Flush would break every streaming RPC. The
// path, peer and duration are enough to see the traffic.
func newRejectionLogger(next http.Handler, log *zerolog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var accounted atomic.Bool
		ctx := context.WithValue(r.Context(), accountedKey{}, &accounted)

		start := time.Now()
		next.ServeHTTP(w, r.WithContext(ctx))
		if accounted.Load() {
			return // Already on the rpc log line, with a code and a trace id.
		}

		log.Warn().Ctx(ctx).
			Str("procedure", r.URL.Path).
			Str("peer", r.RemoteAddr).
			Dur("duration", time.Since(start)).
			Str("proto", r.Proto).
			Str("method", r.Method).
			Msg("rpc rejected")
	})
}
