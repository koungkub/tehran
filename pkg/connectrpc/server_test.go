package connectrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"github.com/rs/zerolog"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/koungkub/tehran/pkg/lifecycle"
)

// The server is meant to be driven by a supervisor. Asserting that here rather
// than in the package proper keeps the production code free of the dependency.
var _ lifecycle.Component = (*Server)(nil)

const (
	testService   = "test.v1.EchoService"
	testProcedure = "/test.v1.EchoService/Echo"
)

type (
	echoRequest  = connect.Request[wrapperspb.StringValue]
	echoResponse = connect.Response[wrapperspb.StringValue]
)

// echoModule is a Module built without codegen: connect.NewUnaryHandler accepts
// any proto message, so a well-known wrapper type stands in for a generated
// request and response.
type echoModule struct {
	fn func(context.Context, *echoRequest) (*echoResponse, error)
}

func (m echoModule) Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
	mux.Handle(testProcedure, connect.NewUnaryHandler(testProcedure, m.fn, opts...))
	return []string{testService}
}

func echo(_ context.Context, req *echoRequest) (*echoResponse, error) {
	return connect.NewResponse(wrapperspb.String(req.Msg.GetValue())), nil
}

// capture records what reaches the logger, as one flat map of strings per line.
//
// It is the logger's writer, so it sees exactly the JSON a real deployment would
// — which is also why the trace id has to be put on the record by a hook rather
// than read from a context here: by the time a line reaches a writer there is no
// context left to read.
type capture struct {
	mu      sync.Mutex
	records []map[string]string
}

func (c *capture) Write(p []byte) (int, error) {
	decoded := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber() // Keep durations and counts as they were written.
	if err := dec.Decode(&decoded); err != nil {
		return len(p), nil // Not a record this test cares about.
	}
	fields := make(map[string]string, len(decoded))
	for k, v := range decoded {
		fields[k] = fmt.Sprint(v)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, fields)
	return len(p), nil
}

// logger wires the capture up the way telemetry.NewLogger does: the trace id
// comes from a hook reading the event's context, which is what makes these tests
// fail if an interceptor stops passing one.
func (c *capture) logger() *zerolog.Logger {
	l := zerolog.New(c).Level(zerolog.TraceLevel).Hook(zerolog.HookFunc(
		func(e *zerolog.Event, _ zerolog.Level, _ string) {
			if sc := trace.SpanContextFromContext(e.GetCtx()); sc.IsValid() {
				e.Str("trace_id", sc.TraceID().String())
			}
		}))
	return &l
}

func (c *capture) find(msg string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r["message"] == msg {
			return r
		}
	}
	return nil
}

// waitFor polls for a log line. The interceptors log after the handler returns,
// so a client that gave up on its own — a cancellation — can be back before the
// line exists.
func (c *capture) waitFor(t *testing.T, msg string) map[string]string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if rec := c.find(msg); rec != nil {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q log line was emitted", msg)
	return nil
}

// discard is a logger for the tests that assert on behaviour rather than output.
func discard() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// newTestServer builds a Server and a client that talks to it through httptest,
// which avoids binding a port. The Connect protocol works over HTTP/1.1, so no
// h2c is needed here.
func newTestServer(t *testing.T, module echoModule, opts ...Option) (
	*Server, *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], *capture,
) {
	t.Helper()
	return newConfiguredTestServer(t, Config{}, module, opts...)
}

func newConfiguredTestServer(t *testing.T, cfg Config, module echoModule, opts ...Option) (
	*Server, *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], *capture,
) {
	t.Helper()
	recorder := &capture{}
	opts = append([]Option{
		WithModules(module),
		WithLogger(recorder.logger()),
	}, opts...)

	srv, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		ts.Client(), ts.URL+testProcedure,
	)
	return srv, client, recorder
}

func TestServerRoundTrip(t *testing.T) {
	_, client, capture := newTestServer(t, echoModule{fn: echo})

	res, err := client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String("tehran")))
	if err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	if got := res.Msg.GetValue(); got != "tehran" {
		t.Errorf("echo = %q, want %q", got, "tehran")
	}

	rec := capture.find("rpc")
	if rec == nil {
		t.Fatal("no rpc log line was emitted")
	}
	if rec["procedure"] != testProcedure {
		t.Errorf("procedure = %q, want %q", rec["procedure"], testProcedure)
	}
	if rec["code"] != "ok" {
		t.Errorf("code = %q, want %q", rec["code"], "ok")
	}
	if rec["duration"] == "" {
		t.Error("rpc line has no duration")
	}
}

// TestRPCLogLineSeesSpanContext pins the interceptor order. Connect makes the
// first interceptor outermost, so the otel interceptor has to precede the
// logging one — otherwise the span is created downstream and the context the
// logger receives has none, leaving every rpc line uncorrelated.
func TestRPCLogLineSeesSpanContext(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, client, capture := newTestServer(t, echoModule{fn: echo}, WithTracerProvider(tp))

	if _, err := client.CallUnary(
		context.Background(), connect.NewRequest(wrapperspb.String("tehran")),
	); err != nil {
		t.Fatalf("CallUnary: %v", err)
	}

	rec := capture.find("rpc")
	if rec == nil {
		t.Fatal("no rpc log line was emitted")
	}
	if rec["trace_id"] == "" {
		t.Error("the context reaching the logger carried no span; " +
			"the otel interceptor must be registered before the logging one")
	}
}

func TestServerRecoversPanic(t *testing.T) {
	panicking := echoModule{fn: func(context.Context, *echoRequest) (*echoResponse, error) {
		panic("boom")
	}}
	_, client, capture := newTestServer(t, panicking)

	_, err := client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String("x")))
	if err == nil {
		t.Fatal("CallUnary succeeded, want an error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want %v", got, connect.CodeInternal)
	}
	// The panic value must not leak to the caller.
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q leaks the panic value", err)
	}
	if rec := capture.find("rpc panic"); rec == nil {
		t.Error("the panic was not logged")
	} else if rec["stack"] == "" {
		t.Error("the panic log line has no stack")
	}
}

func TestServerMountsHealthAndReflection(t *testing.T) {
	srv, _, _ := newTestServer(t, echoModule{fn: echo})

	// The bare mux, not Handler(): that is wrapped in the rejection-reporting
	// middleware, which routes nothing itself.
	mux := srv.mux
	for _, path := range []string{
		testProcedure,
		"/grpc.health.v1.Health/Check",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("%s is not routed", path)
		}
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	srv, err := New(
		Config{Host: "127.0.0.1", ShutdownTimeout: 2 * time.Second},
		WithModules(echoModule{fn: echo}),
		WithLogger(discard()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// Give Serve a moment to bind before asking it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

func TestServeReportsBindFailure(t *testing.T) {
	// Port 1 is privileged, so binding it fails for a normal test process.
	srv, err := New(Config{Host: "127.0.0.1", Port: 1}, WithLogger(discard()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Skip("able to bind port 1, so this process is privileged")
	} else if !strings.Contains(err.Error(), "rpc server") {
		t.Errorf("error %q is not wrapped with the server name", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"MaxRequestBytes", got.MaxRequestBytes, DefaultMaxRequestBytes},
		{"ReadHeaderTimeout", got.ReadHeaderTimeout, DefaultReadHeaderTimeout},
		{"ShutdownTimeout", got.ShutdownTimeout, DefaultShutdownTimeout},
		{"IdleTimeout", got.IdleTimeout, DefaultIdleTimeout},
		{"KeepaliveInterval", got.KeepaliveInterval, DefaultKeepaliveInterval},
		{"WriteByteTimeout", got.WriteByteTimeout, DefaultWriteByteTimeout},
		{"RequestTimeout", got.RequestTimeout, DefaultRequestTimeout},
		{"MaxConcurrentStreams", got.MaxConcurrentStreams, DefaultMaxConcurrentStreams},
		// ReadTimeout is the one field with no default: it is unsafe for
		// client-streaming RPCs, so a service has to ask for it.
		{"ReadTimeout", got.ReadTimeout, time.Duration(0)},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// The drain has to be able to outlast a request, or an ordinary SIGTERM cuts
	// one off: Shutdown waits for connections to go idle and never cancels a
	// handler's context. Each value is defensible alone; only the pair is wrong.
	// Body read and handler run in sequence, so the two timeouts add up.
	if worst := got.ReadTimeout + got.RequestTimeout; worst > got.ShutdownTimeout {
		t.Errorf("ReadTimeout + RequestTimeout (%v) > ShutdownTimeout (%v): "+
			"a request outliving the drain is cut off and Serve returns an error",
			worst, got.ShutdownTimeout)
	}

	// An explicit value is never overridden. Every field is set, so a new field
	// that withDefaults clobbers regardless fails here.
	set := Config{
		MaxRequestBytes:      1,
		ReadHeaderTimeout:    time.Second,
		ShutdownTimeout:      time.Second,
		IdleTimeout:          time.Second,
		KeepaliveInterval:    time.Second,
		WriteByteTimeout:     time.Second,
		RequestTimeout:       time.Second,
		MaxConcurrentStreams: 1,
		ReadTimeout:          time.Second,
	}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults changed an explicit config: %+v != %+v", got, set)
	}
}

// TestProgressBasedTimeoutsAreWired checks the settings reach the http.Server,
// and that the two total-duration timeouts stay unset. Asserting the wiring is
// as far as this goes: whether a PING actually goes out after
// KeepaliveInterval is the standard library's contract, not this package's.
func TestProgressBasedTimeoutsAreWired(t *testing.T) {
	cfg := Config{
		ReadHeaderTimeout:    2 * time.Second,
		IdleTimeout:          3 * time.Second,
		KeepaliveInterval:    4 * time.Second,
		WriteByteTimeout:     5 * time.Second,
		MaxConcurrentStreams: 7,
	}
	srv, err := New(cfg, WithLogger(discard()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if srv.http.ReadHeaderTimeout != cfg.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.http.ReadHeaderTimeout, cfg.ReadHeaderTimeout)
	}
	if srv.http.IdleTimeout != cfg.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.http.IdleTimeout, cfg.IdleTimeout)
	}
	// WriteTimeout bounds the whole ServeHTTP lifetime, a streaming response
	// included, and there is no setting that turns it on. It must stay at zero.
	if srv.http.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: it would abort a streaming response", srv.http.WriteTimeout)
	}
	// ReadTimeout is opt-in, so an unset Config must leave it off.
	if srv.http.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 when unconfigured", srv.http.ReadTimeout)
	}

	h2 := srv.http.HTTP2
	if h2 == nil {
		t.Fatal("HTTP2 is nil, so there is no keepalive or stalled-write detection")
	}
	if h2.SendPingTimeout != cfg.KeepaliveInterval {
		t.Errorf("SendPingTimeout = %v, want %v", h2.SendPingTimeout, cfg.KeepaliveInterval)
	}
	if h2.WriteByteTimeout != cfg.WriteByteTimeout {
		t.Errorf("WriteByteTimeout = %v, want %v", h2.WriteByteTimeout, cfg.WriteByteTimeout)
	}
	if h2.MaxConcurrentStreams != cfg.MaxConcurrentStreams {
		t.Errorf("MaxConcurrentStreams = %d, want %d", h2.MaxConcurrentStreams, cfg.MaxConcurrentStreams)
	}
}

// TestUnroutedRequestIsReported covers the blind spot no interceptor can see.
// Connect decodes the request message before the interceptor chain runs, so
// anything failing earlier — a body cut off by ReadTimeout above all, and an
// unroutable path here — reaches neither the logging nor the tracing
// interceptor, and only the middleware can account for it.
func TestUnroutedRequestIsReported(t *testing.T) {
	srv, _, capture := newTestServer(t, echoModule{fn: echo})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	res, err := ts.Client().Post(ts.URL+"/no.such.Service/Method", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	rec := capture.waitFor(t, "rpc rejected")
	if rec["procedure"] != "/no.such.Service/Method" {
		t.Errorf("procedure = %q, want the requested path", rec["procedure"])
	}
	if rec["peer"] == "" {
		t.Error("the rejection line has no peer, so the source is untraceable")
	}
	if rec["duration"] == "" {
		t.Error("the rejection line has no duration, so a slow body is invisible")
	}
}

// TestServedRPCIsNotDoubleLogged keeps the middleware from doubling every log
// line: an RPC that reached a handler is reported once, by the interceptor,
// which is the line that carries the code and the trace id.
func TestServedRPCIsNotDoubleLogged(t *testing.T) {
	_, client, capture := newTestServer(t, echoModule{fn: echo})

	if _, err := client.CallUnary(
		context.Background(), connect.NewRequest(wrapperspb.String("tehran")),
	); err != nil {
		t.Fatalf("CallUnary: %v", err)
	}

	if rec := capture.find("rpc"); rec == nil {
		t.Fatal("no rpc log line was emitted")
	}
	if rec := capture.find("rpc rejected"); rec != nil {
		t.Errorf("a served RPC was also reported as rejected: %v", rec)
	}
}

// TestFailedRPCIsNotDoubleLogged is the same guarantee for the error path: the
// interceptor reports a failure with its code, so the middleware must stay
// quiet. A panic is used because it is the furthest an RPC can go wrong while
// still having reached a handler.
func TestFailedRPCIsNotDoubleLogged(t *testing.T) {
	panicking := echoModule{fn: func(context.Context, *echoRequest) (*echoResponse, error) {
		panic("boom")
	}}
	_, client, capture := newTestServer(t, panicking)

	if _, err := client.CallUnary(
		context.Background(), connect.NewRequest(wrapperspb.String("x")),
	); err == nil {
		t.Fatal("CallUnary succeeded, want an error")
	}
	if rec := capture.find("rpc rejected"); rec != nil {
		t.Errorf("a failed RPC was also reported as rejected: %v", rec)
	}
}

// TestHealthAndReflectionAreNotReportedAsRejected guards against the rejection
// middleware crying wolf. Those two are mounted without the handler options that
// carry the interceptors, so nothing logs them — deliberately, since an
// orchestrator's health check runs every few seconds. Without accountedFor the
// middleware would report every single one at warn level, which is worse than
// the blind spot it was added to close.
func TestHealthAndReflectionAreNotReportedAsRejected(t *testing.T) {
	srv, _, capture := newTestServer(t, echoModule{fn: echo})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	for _, path := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		res, err := ts.Client().Post(ts.URL+path, "application/grpc", nil)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		_ = res.Body.Close()
	}

	// Nothing to wait for: a rejection is logged before ServeHTTP returns, so by
	// the time the last response is read any line would already exist.
	if rec := capture.find("rpc rejected"); rec != nil {
		t.Errorf("an infrastructure endpoint was reported as rejected: %v", rec)
	}
}

// TestReadTimeoutIsOptInAndWired covers the one setting a service has to ask for.
// Nothing else bounds the body-read phase — Connect decodes the request before
// the interceptor chain runs, so RequestTimeout has not started; the HTTP/2 idle
// timer stops when a stream opens; WriteByteTimeout has nothing to write; and a
// peer answering PINGs defeats KeepaliveInterval. Left off, a slow-body client
// pins a stream indefinitely, so a service that serves no client-streaming RPC
// wants this on.
func TestReadTimeoutIsOptInAndWired(t *testing.T) {
	off, err := New(Config{}, WithLogger(discard()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if off.http.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0: withDefaults must not switch it on", off.http.ReadTimeout)
	}

	// ReadHeaderTimeout has to come down with it: its 10s default is above this,
	// and New refuses that pair. See TestReadHeaderTimeoutMustFitReadTimeout.
	on, err := New(
		Config{ReadTimeout: 3 * time.Second, ReadHeaderTimeout: time.Second},
		WithLogger(discard()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if on.http.ReadTimeout != 3*time.Second {
		t.Errorf("ReadTimeout = %v, want 3s", on.http.ReadTimeout)
	}
}

// TestReadHeaderTimeoutMustFitReadTimeout pins the check that caught this
// package's own test writing an incoherent pair.
//
// net/http arms the header phase with ReadHeaderTimeout and swaps to the
// ReadTimeout deadline only once the headers are read. Set the header budget
// higher and the header phase can outlive the whole-request budget, so the body
// phase begins on a deadline that has already passed — a failure that looks like
// the client's fault and is not. Switching ReadTimeout on while leaving
// ReadHeaderTimeout at its 10s default is the ordinary way to arrive there, so
// the pair is refused rather than quietly reordered.
func TestReadHeaderTimeoutMustFitReadTimeout(t *testing.T) {
	if _, err := New(Config{ReadTimeout: 3 * time.Second}, WithLogger(discard())); err == nil {
		t.Error("New accepted the 10s default header timeout under a 3s read timeout")
	}
	// Equal is fine: the header phase may use the whole budget as long as it does
	// not outlast it.
	if _, err := New(
		Config{ReadTimeout: 5 * time.Second, ReadHeaderTimeout: 5 * time.Second},
		WithLogger(discard()),
	); err != nil {
		t.Errorf("New rejected an equal pair: %v", err)
	}
	// With ReadTimeout off there is no whole-request budget to outlive, so the
	// header timeout stands alone and any value is coherent.
	if _, err := New(
		Config{ReadHeaderTimeout: time.Hour},
		WithLogger(discard()),
	); err != nil {
		t.Errorf("New rejected a header timeout with no read timeout to exceed: %v", err)
	}
}

// TestSetServingFlipsGRPCHealth covers the drain signal a gRPC-native load
// balancer watches. ops' /readyz answers an orchestrator over HTTP and does not
// reach this: the health service starts at SERVING and nothing else moves it, so
// without SetServing a client watching grpc.health.v1.Health keeps routing here
// for the whole drain.
func TestSetServingFlipsGRPCHealth(t *testing.T) {
	srv, err := New(Config{}, WithModules(&echoModule{}), WithLogger(discard()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	http := httptest.NewServer(srv.Handler())
	t.Cleanup(http.Close)

	check := func() grpchealth.Status {
		t.Helper()
		client := grpchealth.NewClient(http.Client(), http.URL, connect.WithGRPC())
		res, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: testService})
		if err != nil {
			t.Fatalf("health check: %v", err)
		}
		return res.Status
	}

	if got := check(); got != grpchealth.StatusServing {
		t.Fatalf("status = %v, want %v before a drain", got, grpchealth.StatusServing)
	}
	srv.SetServing(false)
	if got := check(); got != grpchealth.StatusNotServing {
		t.Errorf("status = %v, want %v once draining", got, grpchealth.StatusNotServing)
	}
	srv.SetServing(true)
	if got := check(); got != grpchealth.StatusServing {
		t.Errorf("status = %v, want %v once serving again", got, grpchealth.StatusServing)
	}
}

// deadlineReporter answers with how many milliseconds its context has left, so
// a test can tell which deadline is actually in force.
func deadlineReporter(ctx context.Context, _ *echoRequest) (*echoResponse, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		return connect.NewResponse(wrapperspb.String("none")), nil
	}
	ms := strconv.FormatInt(time.Until(dl).Milliseconds(), 10)
	return connect.NewResponse(wrapperspb.String(ms)), nil
}

func callDeadlineMillis(
	ctx context.Context,
	t *testing.T,
	client *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue],
) string {
	t.Helper()
	res, err := client.CallUnary(ctx, connect.NewRequest(wrapperspb.String("x")))
	if err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	return res.Msg.GetValue()
}

// TestRequestTimeoutAppliesWhenClientSendsNone is the gap none of the
// connection-level timeouts cover: a client that asks for no deadline at all.
func TestRequestTimeoutAppliesWhenClientSendsNone(t *testing.T) {
	_, client, _ := newConfiguredTestServer(t,
		Config{RequestTimeout: 25 * time.Second}, echoModule{fn: deadlineReporter})

	got := callDeadlineMillis(context.Background(), t, client)
	if got == "none" {
		t.Fatal("the handler ran with no deadline; a hung handler would occupy its stream forever")
	}
	ms, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("handler reported %q: %v", got, err)
	}
	if ms < 20_000 || ms > 25_000 {
		t.Errorf("deadline in %dms, want roughly the 25s RequestTimeout", ms)
	}
}

// TestClientDeadlineWinsOverRequestTimeout pins a deliberate choice: a deadline
// the caller sent is honoured as-is, in either direction. Connect has already
// turned Connect-Timeout-Ms into a context deadline by the time the interceptor
// runs, and overriding it would ignore what the caller actually asked for — the
// same thing grpc-go does. RequestTimeout is only a backstop for callers that
// send nothing, so a client may legitimately ask for longer than it.
func TestClientDeadlineWinsOverRequestTimeout(t *testing.T) {
	_, client, _ := newConfiguredTestServer(t,
		Config{RequestTimeout: 25 * time.Second}, echoModule{fn: deadlineReporter})

	t.Run("shorter than RequestTimeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ms, err := strconv.Atoi(callDeadlineMillis(ctx, t, client))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if ms > 5_000 {
			t.Errorf("deadline in %dms, want the client's 2s, not the server's 25s", ms)
		}
	})

	t.Run("longer than RequestTimeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		ms, err := strconv.Atoi(callDeadlineMillis(ctx, t, client))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if ms < 60_000 {
			t.Errorf("deadline in %dms, want the client's 90s to be honoured", ms)
		}
	})
}

// TestRequestTimeoutEndsAHangingHandler is the end-to-end version: a handler
// that never finishes on its own returns a real deadline-exceeded code to the
// caller instead of holding the stream.
func TestRequestTimeoutEndsAHangingHandler(t *testing.T) {
	hang := echoModule{fn: func(ctx context.Context, _ *echoRequest) (*echoResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	_, client, capture := newConfiguredTestServer(t,
		Config{RequestTimeout: 50 * time.Millisecond}, hang)

	_, err := client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.String("x")))
	if err == nil {
		t.Fatal("CallUnary succeeded, want a deadline error")
	}
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Errorf("code = %v, want %v", got, connect.CodeDeadlineExceeded)
	}
	if rec := capture.find("rpc"); rec == nil {
		t.Error("the timed-out rpc was not logged")
	} else if rec["code"] != connect.CodeDeadlineExceeded.String() {
		// The timeout interceptor sits inside the logging one, so the log line
		// reports the deadline rather than missing it.
		t.Errorf("logged code = %q, want %q", rec["code"], connect.CodeDeadlineExceeded)
	}
}

// TestClientCancellationIsLoggedAsCanceled covers the same classification gap
// on the path that happens constantly in production: a caller that gives up
// mid-RPC. connect.CodeOf alone would report "unknown" for both this and a
// timeout, hiding two ordinary events among genuine failures.
func TestClientCancellationIsLoggedAsCanceled(t *testing.T) {
	hang := echoModule{fn: func(ctx context.Context, _ *echoRequest) (*echoResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	_, client, capture := newTestServer(t, hang)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := client.CallUnary(ctx, connect.NewRequest(wrapperspb.String("x"))); err == nil {
		t.Fatal("CallUnary succeeded, want a cancellation error")
	}

	if rec := capture.waitFor(t, "rpc"); rec["code"] != connect.CodeCanceled.String() {
		t.Errorf("logged code = %q, want %q", rec["code"], connect.CodeCanceled)
	}
}

// TestZeroConfigStillBoundsAHandler is the safe-by-default guarantee: a service
// that configures nothing at all still gets the backstop, because a unary
// handler with no deadline is exactly what it exists to prevent. There is
// deliberately no way to switch it off — a non-positive value means "default".
func TestZeroConfigStillBoundsAHandler(t *testing.T) {
	_, client, _ := newTestServer(t, echoModule{fn: deadlineReporter})

	got := callDeadlineMillis(context.Background(), t, client)
	if got == "none" {
		t.Fatal("the zero Config left the handler with no deadline")
	}
	ms, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("handler reported %q: %v", got, err)
	}
	if want := DefaultRequestTimeout.Milliseconds(); ms > want || ms < want-5_000 {
		t.Errorf("deadline in %dms, want roughly DefaultRequestTimeout (%dms)", ms, want)
	}
}

// TestTimeoutInterceptorLeavesStreamsAlone is the invariant that keeps the
// backstop compatible with streaming: a stream is meant to run long, so no
// total-duration deadline may be imposed on one.
func TestTimeoutInterceptorLeavesStreamsAlone(t *testing.T) {
	var sawDeadline bool
	wrapped := newTimeoutInterceptor(time.Millisecond).WrapStreamingHandler(
		func(ctx context.Context, _ connect.StreamingHandlerConn) error {
			_, sawDeadline = ctx.Deadline()
			return nil
		},
	)
	if err := wrapped(context.Background(), nil); err != nil {
		t.Fatalf("streaming handler: %v", err)
	}
	if sawDeadline {
		t.Error("a deadline was imposed on a streaming handler; it would abort a healthy stream")
	}
}

func TestNewWithoutOptionsIsUsable(t *testing.T) {
	// The zero Config and no options at all must still produce a server: that is
	// what makes the library usable before telemetry is wired up.
	srv, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.Handler() == nil {
		t.Error("Handler() is nil")
	}
}
