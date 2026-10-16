package server_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kartikeyhebbar/kbloom/internal/bloom"
	"github.com/kartikeyhebbar/kbloom/internal/server"
)

// startServer listens on a random port and stops the server when the test ends.
func startServer(t *testing.T, store *bloom.Store, cfg server.Config) string {
	t.Helper()
	if store == nil {
		store = bloom.NewStore(bloom.StoreConfig{MaxBits: 1 << 20, MaxBytes: 1 << 20})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := server.New(store, cfg, logger)
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("server did not shut down")
		}
	})
	return ln.Addr().String()
}

// client is one RESP connection used by a test.
type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

// dial opens a client connection and closes it when the test ends.
func dial(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// cmd writes one request and requires the next bytes to match want exactly.
func (c *client) cmd(req, want string) {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write([]byte(req)); err != nil {
		c.t.Fatal(err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c.r, buf); err != nil {
		c.t.Fatalf("read reply to %q: %v", req, err)
	}
	if string(buf) != want {
		c.t.Fatalf("reply to %q\ngot  %q\nwant %q", req, buf, want)
	}
}

// arr encodes args as a RESP array of bulk strings, including the correct lengths.
func arr(args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return b.String()
}

// TestCommands exercises the supported commands on one persistent connection.
func TestCommands(t *testing.T) {
	addr := startServer(t, nil, server.Config{})
	c := dial(t, addr)

	c.cmd(arr("PING"), "+PONG\r\n")
	c.cmd("PING\r\n", "+PONG\r\n")
	c.cmd(arr("PING", "hello"), "$5\r\nhello\r\n")
	c.cmd(arr("ECHO", "xyz"), "$3\r\nxyz\r\n")
	c.cmd(arr("SELECT", "0"), "+OK\r\n")
	c.cmd(arr("SELECT", "1"), "-ERR DB index is out of range\r\n")
	c.cmd(arr("CLIENT", "SETINFO", "LIB-NAME", "ioredis"), "+OK\r\n")
	c.cmd(arr("CLIENT", "SETNAME", "app"), "+OK\r\n")
	c.cmd(arr("CLIENT", "GETNAME"), "$3\r\napp\r\n")
	c.cmd(arr("FOO"), "-ERR unknown command 'FOO'\r\n")
	c.cmd(arr("BF.ADD", "k"), "-ERR wrong number of arguments for 'BF.ADD' command\r\n")

	c.cmd(arr("BF.RESERVE", "users", "0.01", "1000"), "+OK\r\n")
	c.cmd(arr("BF.RESERVE", "users", "0.01", "1000"), "-ERR item exists\r\n")
	c.cmd(arr("BF.RESERVE", "x", "1", "10"), "-ERR error rate must be between 0 and 1\r\n")
	c.cmd(arr("BF.RESERVE", "x", "0.01", "0"), "-ERR capacity must be greater than 0\r\n")
	c.cmd(arr("bf.reserve", "", "0.01", "10"), "-ERR key must not be empty\r\n")

	c.cmd(arr("BF.ADD", "users", "alice"), ":1\r\n")
	c.cmd(arr("bf.add", "users", "alice"), ":0\r\n")
	c.cmd(arr("BF.EXISTS", "users", "alice"), ":1\r\n")
	c.cmd(arr("BF.EXISTS", "users", "bob"), ":0\r\n")
	c.cmd(arr("BF.ADD", "missing", "z"), "-ERR not found\r\n")
	c.cmd(arr("BF.ADD", "users", "a\r\nb"), ":1\r\n")
	c.cmd(arr("BF.EXISTS", "users", "a\r\nb"), ":1\r\n")
	c.cmd("BF.EXISTS users bob\r\n", ":0\r\n")

	// Two commands in one write; replies stay in order.
	c.cmd(arr("PING")+arr("PING"), "+PONG\r\n+PONG\r\n")
}

// TestQuitClosesConnection checks that QUIT sends +OK and then closes.
func TestQuitClosesConnection(t *testing.T) {
	addr := startServer(t, nil, server.Config{})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("*1\r\n$4\r\nQUIT\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "+OK\r\n" {
		t.Fatalf("got %q", buf[:n])
	}
	n, err = conn.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("after quit n=%d err=%v", n, err)
	}
}

// TestProtocolErrorCloses checks that a bad frame returns an error and drops the socket.
func TestProtocolErrorCloses(t *testing.T) {
	addr := startServer(t, nil, server.Config{})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("*1\r\n$3\r\nPING\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "-ERR protocol error\r\n" {
		t.Fatalf("got %q", buf)
	}
}

// TestMaxClients checks that the connection past the limit is rejected.
func TestMaxClients(t *testing.T) {
	addr := startServer(t, nil, server.Config{MaxConnections: 1})
	hold := dial(t, addr)
	hold.cmd("*1\r\n$4\r\nPING\r\n", "+PONG\r\n")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "-ERR max number of clients reached\r\n" {
		t.Fatalf("got %q", buf[:n])
	}
}

// TestReadTimeout checks that an idle client is disconnected.
func TestReadTimeout(t *testing.T) {
	addr := startServer(t, nil, server.Config{ReadTimeout: 150 * time.Millisecond})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err != io.EOF {
		t.Fatalf("err=%v, want EOF", err)
	}
}

// TestReserveTooLarge checks the per-filter bit cap through the RESP command.
func TestReserveTooLarge(t *testing.T) {
	store := bloom.NewStore(bloom.StoreConfig{MaxBits: 64, MaxBytes: 1 << 20})
	addr := startServer(t, store, server.Config{})
	c := dial(t, addr)
	c.cmd(arr("BF.RESERVE", "k", "0.01", "100000"),
		"-ERR filter exceeds configured maximum size\r\n")
}

// TestNilStore checks that New refuses to start without a registry.
func TestNilStore(t *testing.T) {
	if _, err := server.New(nil, server.Config{}, nil); err == nil {
		t.Fatal("expected error")
	}
}
