package browser

import (
	"context"
	"errors"
	"net"
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
