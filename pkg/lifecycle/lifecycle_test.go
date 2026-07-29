package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// discard keeps these tests off whatever zerolog's package-level logger
// happens to point at; they assert on behaviour, not on output.
func discard() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// journal records lifecycle events across goroutines so a test can assert the
// order they happened in.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.events)
}

func (j *journal) indexOf(t *testing.T, event string) int {
	t.Helper()
	i := slices.Index(j.all(), event)
	if i < 0 {
		t.Fatalf("event %q never happened; journal: %v", event, j.all())
	}
	return i
}

// stub is a Component whose behaviour each test dials in.
type stub struct {
	name string
	j    *journal

	serveErr  error         // returned immediately from Serve
	drain     time.Duration // time spent draining after the context is cancelled
	ignoreCtx bool          // never return, to exercise the timeout backstop
}

func (s *stub) Name() string { return s.name }

func (s *stub) Serve(ctx context.Context) error {
	s.j.add(s.name + " serving")
	if s.serveErr != nil {
		return s.serveErr
	}
	if s.ignoreCtx {
		<-make(chan struct{}) // Blocks forever; the test process exits eventually.
	}

	<-ctx.Done()
	s.j.add(s.name + " draining")
	if s.drain > 0 {
		time.Sleep(s.drain)
	}
	s.j.add(s.name + " stopped")
	return nil
}

// newSupervisor builds a supervisor with no signal handler, so tests drive
// shutdown purely through the context.
func newSupervisor(t *testing.T, cfg Config, components ...Component) *Supervisor {
	t.Helper()
	return New(cfg,
		WithLogger(discard()),
		WithSignals(),
		WithComponents(components...),
	)
}

// runUntilServing starts Run and waits until every component reports serving.
func runUntilServing(t *testing.T, sup *Supervisor, j *journal, count int) (
	cancel context.CancelFunc, done <-chan error,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(j.all()) < count {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("only %d of %d components started; journal: %v", len(j.all()), count, j.all())
		}
		time.Sleep(time.Millisecond)
	}
	return cancel, errCh
}

func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

// TestStopsInReverseRegistrationOrder is the guarantee the supervisor exists
// for: the component registered first is the last to stop, and it is still
// running while the one after it drains. An ops server registered ahead of an
// RPC server therefore keeps answering probes for the whole drain.
func TestStopsInReverseRegistrationOrder(t *testing.T) {
	j := &journal{}
	first := &stub{name: "ops", j: j}
	second := &stub{name: "rpc", j: j, drain: 50 * time.Millisecond}

	sup := newSupervisor(t, Config{}, first, second)
	cancel, done := runUntilServing(t, sup, j, 2)

	cancel()
	if err := waitFor(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// rpc must be fully stopped before ops even begins to drain.
	if rpcStopped, opsDraining := j.indexOf(t, "rpc stopped"), j.indexOf(t, "ops draining"); rpcStopped > opsDraining {
		t.Errorf("ops began draining before rpc had stopped; journal: %v", j.all())
	}
	if opsStopped := j.indexOf(t, "ops stopped"); opsStopped != len(j.all())-1 {
		t.Errorf("ops was not the last to stop; journal: %v", j.all())
	}
}

func TestBeforeShutdownRunsBeforeAnythingStops(t *testing.T) {
	j := &journal{}
	sup := New(Config{},
		WithLogger(discard()),
		WithSignals(),
		WithComponents(&stub{name: "ops", j: j}, &stub{name: "rpc", j: j}),
		BeforeShutdown(func() { j.add("readiness off") }),
	)

	cancel, done := runUntilServing(t, sup, j, 2)
	cancel()
	if err := waitFor(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Nothing may have started draining before the hook ran.
	hook := j.indexOf(t, "readiness off")
	for i, event := range j.all() {
		if strings.HasSuffix(event, " draining") && i < hook {
			t.Errorf("%q happened before the hook; journal: %v", event, j.all())
		}
	}
}

// TestComponentFailureStopsEverything covers the case where a component dies on
// its own rather than being asked to stop.
func TestComponentFailureStopsEverything(t *testing.T) {
	j := &journal{}
	boom := errors.New("bind: address already in use")
	sup := newSupervisor(t, Config{},
		&stub{name: "ops", j: j},
		&stub{name: "rpc", j: j, serveErr: boom},
	)

	// No cancellation needed: the failing component is what triggers shutdown.
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(t.Context()) }()

	err := waitFor(t, errCh)
	if !errors.Is(err, boom) {
		t.Errorf("Run = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "rpc:") {
		t.Errorf("error %q does not name the component that failed", err)
	}
	if !slices.Contains(j.all(), "ops stopped") {
		t.Errorf("the healthy component was not stopped; journal: %v", j.all())
	}
}

// TestCleanEarlyExitIsNotAnError covers a component that legitimately finishes,
// such as a one-shot job: everything else still shuts down, but Run reports no
// failure.
func TestCleanEarlyExitIsNotAnError(t *testing.T) {
	j := &journal{}
	sup := newSupervisor(t, Config{},
		&stub{name: "ops", j: j},
		&stub{name: "job", j: j}, // serveErr nil, returns as soon as ctx is done
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Run(ctx) }()

	// Cancelling is the only way the stub returns; it exits cleanly.
	deadline := time.Now().Add(5 * time.Second)
	for len(j.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()

	if err := waitFor(t, errCh); err != nil {
		t.Errorf("Run = %v, want nil for a clean exit", err)
	}
}

// TestShutdownTimeoutNamesStuckComponents guards the backstop: a component that
// ignores its context must not hang the process silently.
func TestShutdownTimeoutNamesStuckComponents(t *testing.T) {
	j := &journal{}
	sup := newSupervisor(t, Config{ShutdownTimeout: 100 * time.Millisecond},
		&stub{name: "ops", j: j},
		&stub{name: "wedged", j: j, ignoreCtx: true},
	)

	cancel, done := runUntilServing(t, sup, j, 2)
	cancel()

	err := waitFor(t, done)
	if err == nil {
		t.Fatal("Run = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not mention the timeout", err)
	}
	// Both are still running: wedged is stuck, and ops was never reached.
	for _, name := range []string{"wedged", "ops"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the still-running component %q", err, name)
		}
	}
}

// TestShutdownTimeoutCoversTheHooks pins where the shutdown clock starts.
//
// Config.ShutdownTimeout is documented as a bound on the whole ordered
// shutdown, and BeforeShutdown hooks run inside it. Started only once the hooks
// are done, the clock would hand the drain a full fresh budget however long they
// took, so the real sequence could run to hooks + timeout — past the
// orchestrator's grace period, which ends in SIGKILL and no drain at all.
//
// The numbers are chosen so only the bug can pass: a 250ms hook leaves 50ms of
// the 300ms budget for a component that needs 150ms to drain, so the backstop
// must fire. Start the clock after the hook and the same component drains
// comfortably inside a fresh 300ms and Run returns nil.
func TestShutdownTimeoutCoversTheHooks(t *testing.T) {
	j := &journal{}
	sup := New(Config{ShutdownTimeout: 300 * time.Millisecond},
		WithLogger(discard()),
		WithSignals(),
		WithComponents(&stub{name: "slow-drain", j: j, drain: 150 * time.Millisecond}),
		BeforeShutdown(func() { time.Sleep(250 * time.Millisecond) }),
	)

	cancel, done := runUntilServing(t, sup, j, 1)
	cancel()

	err := waitFor(t, done)
	if err == nil {
		t.Fatal("Run = nil: the hook's 250ms was not charged to the 300ms budget")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not mention the timeout", err)
	}
}

func TestRunWithoutComponents(t *testing.T) {
	sup := New(Config{}, WithLogger(discard()), WithSignals())
	if err := sup.Run(context.Background()); !errors.Is(err, ErrNoComponents) {
		t.Errorf("Run = %v, want %v", err, ErrNoComponents)
	}
}

func TestConfigDefaults(t *testing.T) {
	if got := (Config{}).withDefaults(); got.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", got.ShutdownTimeout, DefaultShutdownTimeout)
	}
	set := Config{ShutdownTimeout: time.Second}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults changed an explicit config: %+v != %+v", got, set)
	}
}
