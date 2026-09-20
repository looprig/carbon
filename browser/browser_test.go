package browser

import (
	"context"
	"errors"
	"github.com/looprig/host"
	"net"
	"strings"
	"testing"
	"time"
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
