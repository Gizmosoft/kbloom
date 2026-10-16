package server

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/kartikeyhebbar/kbloom/internal/bloom"
	"github.com/kartikeyhebbar/kbloom/internal/resp"
)

// session holds the small amount of per-connection Redis client state we honor
// so common drivers can complete their handshake.
type session struct {
	clientName string
}

// dispatch runs one command. It returns true when the connection should close after the reply is flushed.
func (s *Server) dispatch(w *resp.Writer, sess *session, args [][]byte) (bool, error) {
	if len(args) == 0 {
		return false, w.Error("ERR unknown command '?'")
	}
	// Command names are case-insensitive. Argument bytes are left unchanged.
	cmd := strings.ToUpper(string(args[0]))
	switch cmd {
	case "PING":
		return false, cmdPing(w, args)
	case "QUIT":
		if len(args) != 1 {
			return false, wrongArity(w, cmd)
		}
		// Ask the caller to close, but only after +OK has been written and flushed.
		if err := w.Simple("OK"); err != nil {
			return true, err
		}
		return true, nil
	case "ECHO":
		return false, cmdEcho(w, args)
	case "SELECT":
		return false, cmdSelect(w, args)
	case "CLIENT":
		return false, cmdClient(w, sess, args)
	case "BF.RESERVE":
		return false, s.cmdReserve(w, args)
	case "BF.ADD":
		return false, s.cmdAdd(w, args)
	case "BF.EXISTS":
		return false, s.cmdExists(w, args)
	default:
		return false, w.Error("ERR unknown command '" + displayCmd(cmd) + "'")
	}
}

// cmdPing replies +PONG, or a bulk copy of the optional argument, matching Redis.
func cmdPing(w *resp.Writer, args [][]byte) error {
	switch len(args) {
	case 1:
		return w.Simple("PONG")
	case 2:
		return w.Bulk(args[1])
	default:
		return wrongArity(w, "PING")
	}
}

// cmdEcho writes its single argument back as a bulk string.
func cmdEcho(w *resp.Writer, args [][]byte) error {
	if len(args) != 2 {
		return wrongArity(w, "ECHO")
	}
	return w.Bulk(args[1])
}

// cmdSelect accepts only database 0. This process has one keyspace.
func cmdSelect(w *resp.Writer, args [][]byte) error {
	if len(args) != 2 {
		return wrongArity(w, "SELECT")
	}
	if string(args[1]) != "0" {
		return w.Error("ERR DB index is out of range")
	}
	return w.Simple("OK")
}

// cmdClient handles the small CLIENT subset drivers send during connect.
func cmdClient(w *resp.Writer, sess *session, args [][]byte) error {
	if len(args) < 2 {
		return wrongArity(w, "CLIENT")
	}
	sub := strings.ToUpper(string(args[1]))
	switch sub {
	case "SETNAME":
		if len(args) != 3 {
			return wrongArity(w, "CLIENT|SETNAME")
		}
		sess.clientName = string(args[2])
		return w.Simple("OK")
	case "GETNAME":
		if len(args) != 2 {
			return wrongArity(w, "CLIENT|GETNAME")
		}
		if sess.clientName == "" {
			// Redis returns a null bulk string when the connection has no name.
			return w.Null()
		}
		return w.Bulk([]byte(sess.clientName))
	case "SETINFO":
		// Drivers announce their library name and version. The server does not store it.
		if len(args) != 4 {
			return wrongArity(w, "CLIENT|SETINFO")
		}
		return w.Simple("OK")
	default:
		return w.Error("ERR unknown subcommand '" + displayCmd(sub) + "'")
	}
}

// cmdReserve parses "BF.RESERVE key error_rate capacity" and creates the filter.
func (s *Server) cmdReserve(w *resp.Writer, args [][]byte) error {
	if len(args) != 4 {
		return wrongArity(w, "BF.RESERVE")
	}
	rate, err := strconv.ParseFloat(string(args[2]), 64)
	if err != nil {
		return w.Error("ERR error rate must be between 0 and 1")
	}
	capacity, err := strconv.ParseUint(string(args[3]), 10, 64)
	if err != nil {
		return w.Error("ERR capacity must be greater than 0")
	}
	if err := s.store.Reserve(string(args[1]), rate, capacity); err != nil {
		return writeStoreErr(s.log, w, err)
	}
	return w.Simple("OK")
}

// cmdAdd inserts one item. The reply is 1 when a bit flipped and 0 when the item was probably present.
func (s *Server) cmdAdd(w *resp.Writer, args [][]byte) error {
	if len(args) != 3 {
		return wrongArity(w, "BF.ADD")
	}
	added, err := s.store.Add(string(args[1]), args[2])
	if err != nil {
		return writeStoreErr(s.log, w, err)
	}
	if added {
		return w.Integer(1)
	}
	return w.Integer(0)
}

// cmdExists checks one item. The reply is 0 for a definite miss and 1 for a probable hit.
func (s *Server) cmdExists(w *resp.Writer, args [][]byte) error {
	if len(args) != 3 {
		return wrongArity(w, "BF.EXISTS")
	}
	found, err := s.store.Exists(string(args[1]), args[2])
	if err != nil {
		return writeStoreErr(s.log, w, err)
	}
	if found {
		return w.Integer(1)
	}
	return w.Integer(0)
}

// writeStoreErr maps sentinel failures onto RESP errors. Redis clients treat a
// leading '-' as an exception; a bulk-string error would look like success data.
func writeStoreErr(log *slog.Logger, w *resp.Writer, err error) error {
	switch {
	case errors.Is(err, bloom.ErrExists):
		return w.Error("ERR item exists")
	case errors.Is(err, bloom.ErrNotFound):
		return w.Error("ERR not found")
	case errors.Is(err, bloom.ErrBadErrorRate):
		return w.Error("ERR error rate must be between 0 and 1")
	case errors.Is(err, bloom.ErrBadCapacity):
		return w.Error("ERR capacity must be greater than 0")
	case errors.Is(err, bloom.ErrEmptyKey):
		return w.Error("ERR key must not be empty")
	case errors.Is(err, bloom.ErrTooLarge):
		return w.Error("ERR filter exceeds configured maximum size")
	default:
		// Hide unexpected failures from the client and keep the detail in the log.
		log.Error("filter operation failed", "err", err)
		return w.Error("ERR internal error")
	}
}

// wrongArity writes the Redis-style arity error for cmd.
func wrongArity(w *resp.Writer, cmd string) error {
	return w.Error("ERR wrong number of arguments for '" + cmd + "' command")
}

// displayCmd returns a single-line ASCII token safe to embed in a RESP error.
func displayCmd(s string) string {
	// An empty, huge, or non-printable name would break the "-ERR ...\r\n" frame.
	if s == "" || len(s) > 64 {
		return "?"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "?"
		}
	}
	return s
}
