package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

type errgroupGroup = errgroup.Group

// SIGTERM during a tool call: the call already running finishes and its
// response is delivered, and whatever carries the traffic (the tsnet node)
// is torn down only after that.
func TestShutdownDrainsInFlightRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inFlight := make(chan struct{})
	var responded, closedEarly atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		time.Sleep(300 * time.Millisecond) // a tool call in progress
		_, _ = w.Write([]byte("done"))
		responded.Store(true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	var g errgroup.Group
	logger := slog.New(slog.DiscardHandler)
	serveHTTP(ctx, &g, newServer(h, "test", logger), ln, "test", logger, func() {
		if !responded.Load() {
			closedEarly.Store(true)
		}
	})

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/", "application/json", nil)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-inFlight
	cancel() // SIGTERM arrives mid-call

	r := <-got
	if r.err != nil || r.body != "done" {
		t.Errorf("in-flight request: body=%q err=%v, want it answered", r.body, r.err)
	}
	if err := g.Wait(); err != nil {
		t.Errorf("group: %v", err)
	}
	if closedEarly.Load() {
		t.Error("transport torn down before the in-flight response was written")
	}

	// And nothing new is accepted once shutdown has begun.
	if _, err := http.Post("http://"+ln.Addr().String()+"/", "application/json", nil); err == nil {
		t.Error("new request accepted after shutdown")
	}
}
