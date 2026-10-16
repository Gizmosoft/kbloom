// Package resp reads and writes the RESP2 subset used by this server:
// arrays of bulk strings in, and simple strings, errors, integers, and bulk
// strings out.
package resp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Request-size limits used when the caller does not supply its own.
const (
	// defaultMaxBulk rejects one argument larger than 16 MiB before it is allocated.
	defaultMaxBulk = 16 << 20
	// defaultMaxArgs caps a command array. The BF.* commands use far fewer slots.
	defaultMaxArgs = 128
)

// Conditions ReadCommand reports besides ordinary I/O errors.
var (
	// ErrProtocol marks a malformed request. The connection should be closed.
	ErrProtocol = errors.New("protocol error")
	// ErrEmpty is an empty inline line, which Redis ignores.
	ErrEmpty = errors.New("empty command")
)

// Reader parses one command at a time from a connection.
type Reader struct {
	r       *bufio.Reader
	maxBulk int
	maxArgs int
}

// NewReader wraps r. Non-positive limits fall back to package defaults.
func NewReader(r io.Reader, maxBulk, maxArgs int) *Reader {
	if maxBulk <= 0 {
		maxBulk = defaultMaxBulk
	}
	if maxArgs <= 0 {
		maxArgs = defaultMaxArgs
	}
	return &Reader{
		r:       bufio.NewReaderSize(r, 64<<10),
		maxBulk: maxBulk,
		maxArgs: maxArgs,
	}
}

// ReadCommand returns the command name and arguments.
// Inline commands (no leading '*') are split on whitespace.
func (rd *Reader) ReadCommand() ([][]byte, error) {
	b, err := rd.r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch b {
	case '*':
		// A leading '*' starts a RESP array of bulk strings.
		return rd.readArray()
	case '\n':
		return nil, ErrEmpty
	case '\r':
		// A blank line is "\r\n". Anything else after '\r' is not a command.
		next, err := rd.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if next == '\n' {
			return nil, ErrEmpty
		}
		return nil, fmt.Errorf("%w: expected array", ErrProtocol)
	default:
		// Push the byte back and parse the line as an inline command, such as "PING".
		if err := rd.r.UnreadByte(); err != nil {
			return nil, err
		}
		return rd.readInline()
	}
}

// readArray parses "*<n>\r\n" followed by n bulk strings.
func (rd *Reader) readArray() ([][]byte, error) {
	n, err := rd.readLen()
	if err != nil {
		return nil, err
	}
	if n < 0 || n > int64(rd.maxArgs) {
		return nil, fmt.Errorf("%w: invalid array length", ErrProtocol)
	}
	args := make([][]byte, n)
	for i := int64(0); i < n; i++ {
		kind, err := rd.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if kind != '$' {
			return nil, fmt.Errorf("%w: expected bulk string", ErrProtocol)
		}
		args[i], err = rd.readBulk()
		if err != nil {
			return nil, err
		}
	}
	return args, nil
}

// readBulk parses "$<n>\r\n<payload>\r\n". Oversized lengths are rejected before the buffer is allocated.
func (rd *Reader) readBulk() ([]byte, error) {
	n, err := rd.readLen()
	if err != nil {
		return nil, err
	}
	if n < 0 || n > int64(rd.maxBulk) {
		return nil, fmt.Errorf("%w: invalid bulk length", ErrProtocol)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return nil, err
	}
	// The payload is followed by its own CRLF, which is not part of the value.
	var crlf [2]byte
	if _, err := io.ReadFull(rd.r, crlf[:]); err != nil {
		return nil, err
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return nil, fmt.Errorf("%w: expected CRLF", ErrProtocol)
	}
	return buf, nil
}

// readInline parses one Redis inline line and splits it on whitespace.
func (rd *Reader) readInline() ([][]byte, error) {
	line, err := rd.readCRLF()
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil, ErrEmpty
	}
	fields := strings.Fields(string(line))
	if len(fields) > rd.maxArgs {
		return nil, fmt.Errorf("%w: too many arguments", ErrProtocol)
	}
	args := make([][]byte, len(fields))
	for i, field := range fields {
		// Fields returns subslices of a temporary string, so copy each argument out.
		args[i] = []byte(field)
	}
	return args, nil
}

// readLen parses a decimal length that ends at the next CRLF.
func (rd *Reader) readLen() (int64, error) {
	line, err := rd.readCRLF()
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid length", ErrProtocol)
	}
	return n, nil
}

// readCRLF returns the line without the trailing CRLF.
// The returned slice is copied because bufio reuses its buffer.
func (rd *Reader) readCRLF() ([]byte, error) {
	line, err := rd.r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("%w: line too long", ErrProtocol)
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("%w: expected CRLF", ErrProtocol)
	}
	out := make([]byte, len(line)-2)
	copy(out, line[:len(line)-2])
	return out, nil
}

// Writer buffers RESP2 replies. Flush after each command so clients are not
// left waiting on a partial buffer.
type Writer struct {
	w *bufio.Writer
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 4096)}
}

// Flush pushes any buffered reply bytes.
func (w *Writer) Flush() error { return w.w.Flush() }

// Simple writes a RESP simple string, such as +OK\r\n.
func (w *Writer) Simple(s string) error {
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("simple string contains CR or LF")
	}
	_, err := w.w.WriteString("+" + s + "\r\n")
	return err
}

// Error writes a RESP error. Callers pass the full message, including the ERR prefix.
func (w *Writer) Error(msg string) error {
	if strings.ContainsAny(msg, "\r\n") {
		msg = "ERR protocol error"
	}
	_, err := w.w.WriteString("-" + msg + "\r\n")
	return err
}

// Integer writes a RESP integer.
func (w *Writer) Integer(n int64) error {
	// 24 bytes covers ':', a signed int64, and CRLF without a heap allocation.
	var buf [24]byte
	b := buf[:0]
	b = append(b, ':')
	b = strconv.AppendInt(b, n, 10)
	b = append(b, '\r', '\n')
	_, err := w.w.Write(b)
	return err
}

// Bulk writes a RESP bulk string.
func (w *Writer) Bulk(p []byte) error {
	// Frame is "$<len>\r\n" + payload + "\r\n". The payload may itself contain CRLF.
	var hdr [32]byte
	h := hdr[:0]
	h = append(h, '$')
	h = strconv.AppendInt(h, int64(len(p)), 10)
	h = append(h, '\r', '\n')
	if _, err := w.w.Write(h); err != nil {
		return err
	}
	if _, err := w.w.Write(p); err != nil {
		return err
	}
	_, err := w.w.WriteString("\r\n")
	return err
}

// Null writes a RESP null bulk string.
func (w *Writer) Null() error {
	_, err := w.w.WriteString("$-1\r\n")
	return err
}
