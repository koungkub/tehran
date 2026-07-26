package connectrpc

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
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

// captureHandler records what reaches the logger, including whether the context
// carried a span at that point.
type captureHandler struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(ctx context.Context, rec slog.Record) error {
	fields := map[string]string{"msg": rec.Message, "level": rec.Level.String()}
	rec.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		fields["trace_id"] = sc.TraceID().String()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, fields)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) find(msg string) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

// newTestServer builds a Server and a client that talks to it through httptest,
// which avoids binding a port. The Connect protocol works over HTTP/1.1, so no
// h2c is needed here.
func newTestServer(t *testing.T, module echoModule, opts ...Option) (
	*Server, *connect.Client[wrapperspb.StringValue, wrapperspb.StringValue], *captureHandler,
) {
	t.Helper()
	capture := &captureHandler{}
	opts = append([]Option{
		WithModules(module),
		WithLogger(slog.New(capture)),
	}, opts...)

	srv, err := New(Config{}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](
		ts.Client(), ts.URL+testProcedure,
	)
	return srv, client, capture
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

	mux, ok := srv.Handler().(*http.ServeMux)
	if !ok {
		t.Fatalf("Handler() is %T, want *http.ServeMux", srv.Handler())
	}
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
		WithLogger(slog.New(slog.DiscardHandler)),
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
	srv, err := New(Config{Host: "127.0.0.1", Port: 1}, WithLogger(slog.New(slog.DiscardHandler)))
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
	if got.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("MaxRequestBytes = %d, want %d", got.MaxRequestBytes, DefaultMaxRequestBytes)
	}
	if got.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if got.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", got.ShutdownTimeout, DefaultShutdownTimeout)
	}

	// An explicit value is never overridden.
	set := Config{MaxRequestBytes: 1, ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults changed an explicit config: %+v != %+v", got, set)
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
