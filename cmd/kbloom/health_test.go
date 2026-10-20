package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Gizmosoft/kbloom/internal/bloom"
	"github.com/Gizmosoft/kbloom/internal/server"
)

func TestHealthDialAddr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":6380", "127.0.0.1:6380"},
		{"0.0.0.0:6380", "127.0.0.1:6380"},
		{"[::]:6380", "127.0.0.1:6380"},
		{"127.0.0.1:6380", "127.0.0.1:6380"},
		{"10.1.2.3:7000", "10.1.2.3:7000"},
	}
	for _, tc := range cases {
		got, err := healthDialAddr(tc.in)
		if err != nil {
			t.Fatalf("healthDialAddr(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("healthDialAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(bloom.NewStore(bloom.StoreConfig{}), server.Config{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})

	if err := ping(ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
}
