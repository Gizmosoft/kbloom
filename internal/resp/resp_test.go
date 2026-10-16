package resp

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestReadCommand checks array, inline, empty, and binary bulk commands.
func TestReadCommand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "ping", in: "*1\r\n$4\r\nPING\r\n", want: []string{"PING"}},
		{name: "inline", in: "BF.ADD users alice\r\n", want: []string{"BF.ADD", "users", "alice"}},
		{name: "empty bulk", in: "*2\r\n$6\r\nBF.ADD\r\n$0\r\n\r\n", want: []string{"BF.ADD", ""}},
		{name: "binary", in: "*1\r\n$4\r\na\r\nb\r\n", want: []string{"a\r\nb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := NewReader(bytes.NewReader([]byte(tc.in)), 0, 0).ReadCommand()
			if err != nil {
				t.Fatal(err)
			}
			if len(args) != len(tc.want) {
				t.Fatalf("len=%d want %d (%q)", len(args), len(tc.want), args)
			}
			for i := range tc.want {
				if string(args[i]) != tc.want[i] {
					t.Fatalf("arg %d = %q, want %q", i, args[i], tc.want[i])
				}
			}
		})
	}
}

// TestReadCommandErrors checks EOF, blank lines, and malformed frames.
func TestReadCommandErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want error
	}{
		{name: "eof", in: "", want: io.EOF},
		{name: "blank", in: "\r\n", want: ErrEmpty},
		{name: "bad length", in: "*x\r\n", want: ErrProtocol},
		{name: "not bulk", in: "*1\r\n+PING\r\n", want: ErrProtocol},
		{name: "too big", in: "*1\r\n$10\r\n0123456789\r\n", max: 4, want: ErrProtocol},
		{name: "bare lf", in: "PING\n", want: ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader([]byte(tc.in)), tc.max, 0).ReadCommand()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestReadCommandSplitWrite checks that a command split across two writes still parses.
func TestReadCommandSplitWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write([]byte("*1\r\n$4\r\n"))
		time.Sleep(20 * time.Millisecond)
		_, _ = client.Write([]byte("PING\r\n"))
		_ = client.Close()
	}()

	args, err := NewReader(server, 0, 0).ReadCommand()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || string(args[0]) != "PING" {
		t.Fatalf("args=%q", args)
	}
}

// TestWriter checks the on-wire form of each reply type.
func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Simple("PONG"); err != nil {
		t.Fatal(err)
	}
	if err := w.Simple("OK"); err != nil {
		t.Fatal(err)
	}
	if err := w.Integer(1); err != nil {
		t.Fatal(err)
	}
	if err := w.Integer(0); err != nil {
		t.Fatal(err)
	}
	if err := w.Error("ERR item exists"); err != nil {
		t.Fatal(err)
	}
	if err := w.Bulk([]byte("a\r\nb")); err != nil {
		t.Fatal(err)
	}
	if err := w.Null(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "+PONG\r\n+OK\r\n:1\r\n:0\r\n-ERR item exists\r\n$4\r\na\r\nb\r\n$-1\r\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

// FuzzReadCommand checks that arbitrary bytes do not panic the parser.
func FuzzReadCommand(f *testing.F) {
	f.Add([]byte("*1\r\n$4\r\nPING\r\n"))
	f.Add([]byte("PING\r\n"))
	f.Add([]byte("*3\r\n$6\r\nBF.ADD\r\n$1\r\nk\r\n$4\r\na\r\nb\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = NewReader(bytes.NewReader(data), 1<<20, 32).ReadCommand()
	})
}
