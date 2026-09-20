package browser

import (
	"context"
	"errors"
	"github.com/looprig/host"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/looprig/sessionstore"
)

func TestStopClosesListenerWhenServeWasOvertaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{listener: ln, serveDone: make(chan error, 1), done: make(chan struct{})}
	s.serveDone <- net.ErrClosed
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("listener accepted after Stop")
	}
}

func TestPublicBindCancelledBeforeReadyClosesListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var bound net.Listener
	ln, err := listenReady(ctx, "127.0.0.1:0", func(_, addr string) (net.Listener, error) {
		var bindErr error
		bound, bindErr = net.Listen("tcp", addr)
		cancel()
		return bound, bindErr
	})
	if ln != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled bind = (%v, %v)", ln, err)
	}
	if bound == nil {
		t.Fatal("test did not bind")
	}
	if conn, err := net.DialTimeout("tcp", bound.Addr().String(), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("cancelled startup retained public listener")
	}
}

func TestStopRetriesHostBeforeClosingStorage(t *testing.T) {
	var order []string
	calls := 0
	s := &Server{done: make(chan struct{}),
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			calls++
			if calls == 1 {
				return host.DrainReport{}, errors.New("publication failed")
			}
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	if err := s.Stop(context.Background()); err == nil {
		t.Fatal("failed Host Stop reported success")
	}
	if got := strings.Join(order, ","); got != "factory,host" {
		t.Fatalf("early cleanup = %s", got)
	}
	select {
	case <-s.Done():
		t.Fatal("Done closed before Host recovered")
	default:
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "factory,host,host,storage" {
		t.Fatalf("retry cleanup = %s", got)
	}
}

func TestStopQuiescesAndObservesBeforeFactoryAndHost(t *testing.T) {
	var order []string
	p := &pendingPages{shards: 1, pages: map[int][]sessionstore.DispositionDueCommandPage{0: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}}}}
	s := &Server{done: make(chan struct{}), shutdownPolicy: ShutdownPolicy{QuiesceTimeout: time.Second, SettlementTimeout: time.Second},
		quiesceFactory: func(context.Context) error { order = append(order, "quiesce"); return nil },
		pendingReader:  p, pendingTenant: "local",
		waitPending: func(context.Context) error {
			order = append(order, "pending")
			p.pages[0] = nil
			p.reads = nil
			return nil
		},
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "quiesce,pending,factory,host,storage" {
		t.Fatalf("shutdown order = %s", got)
	}
}

func TestStopReportsUnsettledCommandAndStillDrainsHost(t *testing.T) {
	var order []string
	p := &pendingPages{shards: 1, pages: map[int][]sessionstore.DispositionDueCommandPage{0: {{Commands: []sessionstore.DispositionInboxEntry{pendingFor("local")}}}}}
	s := &Server{done: make(chan struct{}), shutdownPolicy: ShutdownPolicy{QuiesceTimeout: time.Second, SettlementTimeout: time.Second},
		quiesceFactory: func(context.Context) error { order = append(order, "quiesce"); return nil },
		pendingReader:  p, pendingTenant: "local",
		waitPending: func(context.Context) error { return context.DeadlineExceeded },
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	err := s.Stop(context.Background())
	var pending *PendingCommandsError
	if !errors.As(err, &pending) || pending.Count != 1 {
		t.Fatalf("Stop = %v, want one last-observed pending command", err)
	}
	if got := strings.Join(order, ","); got != "quiesce,factory,host,storage" {
		t.Fatalf("shutdown after settlement timeout = %s", got)
	}
	if p.pages[0][0].Commands[0].Record.State != sessionstore.InboxStatePending {
		t.Fatal("shutdown altered pending command")
	}
}

func TestQuiesceFailureStillStopsFactoryAndHost(t *testing.T) {
	want := errors.New("quiesce failed")
	var order []string
	s := &Server{done: make(chan struct{}), shutdownPolicy: ShutdownPolicy{QuiesceTimeout: time.Second},
		quiesceFactory: func(context.Context) error { order = append(order, "quiesce"); return want },
		stopFactory:    func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	if err := s.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Stop = %v", err)
	}
	if got := strings.Join(order, ","); got != "quiesce,factory,host,storage" {
		t.Fatalf("shutdown after Quiesce error = %s", got)
	}
}

func TestDrainFailuresRetainStorageOwner(t *testing.T) {
	closed := false
	s := &Server{done: make(chan struct{}), stopHost: func(context.Context) (host.DrainReport, error) {
		return host.DrainReport{Failures: []host.DrainFailure{{Step: host.StepCloseStore, Err: errors.New("provider busy")}}}, nil
	}, closeStorage: func(context.Context) error { closed = true; return nil }}
	err := s.Stop(context.Background())
	var incomplete *DrainIncompleteError
	if !errors.As(err, &incomplete) || closed {
		t.Fatalf("Stop = %v, provider closed = %t", err, closed)
	}
	select {
	case <-s.Done():
		t.Fatal("Done closed despite incomplete Host drain")
	default:
	}
}

func TestUnexpectedServeFailureStillTerminatesOwner(t *testing.T) {
	want := errors.New("serve failed")
	s := &Server{serveDone: make(chan error, 1), done: make(chan struct{})}
	s.serveDone <- want
	if err := s.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Stop = %v", err)
	}
	if err := s.Wait(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Wait = %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done left open after cleanup")
	}
}

func TestUnexpectedServeFailureAutomaticallyStopsOwner(t *testing.T) {
	want := errors.New("listener failed")
	closed := make(chan struct{})
	s := &Server{serveDone: make(chan error, 1), done: make(chan struct{}),
		closeStorage: func(context.Context) error { close(closed); return nil }}
	go s.runServe(func(net.Listener) error { return want })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("automatic Wait = %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("unexpected Serve exit left storage open")
	}
}

func TestUnexpectedPublicFailureReportsWhileHostCleanupRetries(t *testing.T) {
	want := errors.New("public listener failed")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	closed := false
	calls := 0
	s := &Server{serveDone: make(chan error, 1), done: make(chan struct{}), failureDone: make(chan struct{}),
		stopHost: func(context.Context) (host.DrainReport, error) {
			calls++
			if calls == 1 {
				close(firstEntered)
				<-releaseFirst
				return host.DrainReport{}, errors.New("drain publication failed")
			}
			return host.DrainReport{}, nil
		}, closeStorage: func(context.Context) error { closed = true; return nil }}
	go s.runServe(func(net.Listener) error { return want })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("Wait did not report listener failure: %v", err)
	}
	<-firstEntered
	if closed {
		t.Fatal("storage closed during failed Host Stop")
	}
	select {
	case <-s.Done():
		t.Fatal("Done closed during failed Host Stop")
	default:
	}
	close(releaseFirst)
	if err := s.Stop(context.Background()); err == nil {
		t.Fatal("first cleanup attempt reported success")
	}
	if err := s.Stop(context.Background()); err != nil && !errors.Is(err, want) {
		t.Fatalf("retry Stop = %v", err)
	}
	if !closed || calls != 2 {
		t.Fatalf("retry closed=%t calls=%d", closed, calls)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done open after successful retry")
	}
	if err := s.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("terminal Wait lost listener failure: %v", err)
	}
}

func TestHostListenerBroadcastTriggersBrowserShutdown(t *testing.T) {
	hostDone := make(chan struct{})
	want := errors.New("Host listener failed")
	s := &Server{done: make(chan struct{}), failureDone: make(chan struct{}),
		hostListenerDone: hostDone, hostListenerError: func() error { return want }}
	go s.watchHost()
	close(hostDone)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("Wait = %v", err)
	}
	if err := s.Stop(ctx); err != nil && !errors.Is(err, want) {
		t.Fatalf("Stop = %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("host listener failure left owner active")
	}
}
