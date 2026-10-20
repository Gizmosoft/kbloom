// Package server exposes Bloom filters over a RESP2 TCP connection.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Gizmosoft/kbloom/internal/bloom"
	"github.com/Gizmosoft/kbloom/internal/resp"
)

// Config controls connection limits and deadlines.
// Zero MaxConnections, MaxBulkBytes, and MaxArgs select built-in defaults.
// A zero timeout disables that deadline.
type Config struct {
	MaxConnections int
	MaxBulkBytes   int
	MaxArgs        int
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// Server accepts Redis clients and dispatches BF.* commands.
type Server struct {
	store *bloom.Store
	cfg   Config
	log   *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{} // live clients, guarded by mu
	sem   chan struct{}         // counting semaphore of size MaxConnections
	wg    sync.WaitGroup        // handlers that have not yet returned
}

// New validates cfg and returns a server bound to store.
func New(store *bloom.Store, cfg Config, logger *slog.Logger) (*Server, error) {
	if store == nil {
		return nil, errors.New("store is nil")
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 10000
	}
	if cfg.MaxBulkBytes <= 0 {
		cfg.MaxBulkBytes = 16 << 20
	}
	if cfg.MaxArgs <= 0 {
		cfg.MaxArgs = 128
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		store: store,
		cfg:   cfg,
		log:   logger,
		conns: make(map[net.Conn]struct{}),
	}, nil
}

// Serve accepts connections until ctx is cancelled or Accept fails.
// Cancellation closes the listener and active connections, then waits for
// handlers to return. Serve is not safe to call twice.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.sem = make(chan struct{}, s.cfg.MaxConnections)
	// Closing the listener unblocks Accept when the process is asked to stop.
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
		s.closeAll()
	})
	defer stop()

	var serveErr error
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Shutdown closes the listener, so this error is expected and not returned.
			if ctx.Err() != nil {
				break
			}
			s.log.Error("accept failed", "err", err)
			s.closeAll()
			serveErr = err
			break
		}
		// A connection can be queued after cancellation. Drop it instead of serving it.
		if ctx.Err() != nil {
			_ = conn.Close()
			break
		}
		if !s.acquire() {
			// Reject without taking a handler slot. The client sees a normal RESP error.
			_, _ = conn.Write([]byte("-ERR max number of clients reached\r\n"))
			_ = conn.Close()
			s.log.Warn("rejected connection", "remote", conn.RemoteAddr().String())
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(conn)
	}
	s.wg.Wait()
	return serveErr
}

// acquire takes a connection slot without blocking. False means the client limit is full.
func (s *Server) acquire() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// serveConn runs one client to completion, then frees its slot and closes the socket.
func (s *Server) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.sem }() // return the client slot
	defer func() {
		// Untrack before Close so closeAll is not holding a connection this handler is finishing.
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	if tc, ok := conn.(*net.TCPConn); ok {
		// Detect dead peers that stop sending without closing.
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(2 * time.Minute)
	}
	s.handle(conn)
}

// handle reads pipelined commands and flushes one reply before reading the next.
func (s *Server) handle(conn net.Conn) {
	rd := resp.NewReader(conn, s.cfg.MaxBulkBytes, s.cfg.MaxArgs)
	wr := resp.NewWriter(conn)
	var sess session
	for {
		if err := setDeadline(conn, s.cfg.ReadTimeout, true); err != nil {
			return
		}
		args, err := rd.ReadCommand()
		if err != nil {
			// Redis ignores a blank inline line and keeps the connection.
			if errors.Is(err, resp.ErrEmpty) {
				continue
			}
			// A dropped peer or an idle timeout closes quietly. A bad frame gets an error first.
			if benign(err) {
				return
			}
			s.log.Debug("protocol error", "remote", conn.RemoteAddr().String(), "err", err)
			_ = setDeadline(conn, s.cfg.WriteTimeout, false)
			_ = wr.Error("ERR protocol error")
			_ = wr.Flush()
			return
		}
		if err := setDeadline(conn, s.cfg.WriteTimeout, false); err != nil {
			return
		}
		quit, err := s.dispatch(wr, &sess, args)
		if err != nil {
			s.log.Debug("write failed", "remote", conn.RemoteAddr().String(), "err", err)
			return
		}
		if err := wr.Flush(); err != nil {
			return
		}
		// QUIT has already queued +OK. Close only after that reply is flushed.
		if quit {
			return
		}
	}
}

// setDeadline arms the read or write deadline. A non-positive duration clears it.
func setDeadline(conn net.Conn, d time.Duration, read bool) error {
	var deadline time.Time
	if d > 0 {
		deadline = time.Now().Add(d)
	}
	if read {
		return conn.SetReadDeadline(deadline)
	}
	return conn.SetWriteDeadline(deadline)
}

// benign reports a disconnect or timeout that should close the socket without an error reply.
func benign(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// closeAll closes current clients. The snapshot is taken under the lock and the
// closes happen outside it, so a handler can untrack without deadlocking.
func (s *Server) closeAll() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}
