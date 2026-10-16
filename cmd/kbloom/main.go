package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kartikeyhebbar/kbloom/internal/bloom"
	"github.com/kartikeyhebbar/kbloom/internal/server"
)

// config is the process configuration after flags and environment variables are merged.
type config struct {
	addr         string
	maxBits      uint64
	maxMemory    uint64
	maxClients   int
	readTimeout  time.Duration
	writeTimeout time.Duration
	logLevel     string
}

// main loads configuration, serves until a signal, and exits non-zero on failure.
func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		// Flag errors are already printed. Exit with their code and do not print them again.
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "kbloom: %v\n", err)
		os.Exit(1)
	}
	if err := serve(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "kbloom: %v\n", err)
		os.Exit(1)
	}
}

// exitError is a flag-parse failure whose message was already written to stderr.
type exitError struct {
	code int
	err  error
}

// Error returns the underlying flag error text.
func (e exitError) Error() string { return e.err.Error() }

// Unwrap exposes the flag error to errors.Is and errors.As.
func (e exitError) Unwrap() error { return e.err }

// parseConfig reads flags, then fills any flag the user did not pass from the environment.
// An explicit flag always wins over the matching KBLOOM_* variable.
func parseConfig(args []string) (config, error) {
	// Defaults suit a single process that should not take Redis's port.
	cfg := config{
		addr:         ":6380",
		maxBits:      1 << 32,
		maxMemory:    1 << 30,
		maxClients:   10000,
		readTimeout:  0,
		writeTimeout: 10 * time.Second,
		logLevel:     "info",
	}

	fs := flag.NewFlagSet("kbloom", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `kbloom serves in-memory Bloom filters over RESP2.

Redis clients can call PING, BF.RESERVE, BF.ADD, and BF.EXISTS.
The default port is 6380 so a local Redis on 6379 is left alone.

Usage:
  kbloom [flags]

Flags:
`)
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "TCP listen address (env KBLOOM_ADDR)")
	fs.Uint64Var(&cfg.maxBits, "max-bits", cfg.maxBits, "maximum bits in one filter; 0 is unlimited (env KBLOOM_MAX_BITS)")
	fs.Uint64Var(&cfg.maxMemory, "max-memory", cfg.maxMemory, "total byte budget for all filters; 0 is unlimited (env KBLOOM_MAX_MEMORY)")
	fs.IntVar(&cfg.maxClients, "max-clients", cfg.maxClients, "maximum concurrent connections (env KBLOOM_MAX_CLIENTS)")
	fs.DurationVar(&cfg.readTimeout, "timeout", cfg.readTimeout, "idle read timeout; 0 disables (env KBLOOM_TIMEOUT)")
	fs.DurationVar(&cfg.writeTimeout, "write-timeout", cfg.writeTimeout, "per-reply write timeout; 0 disables (env KBLOOM_WRITE_TIMEOUT)")
	fs.StringVar(&cfg.logLevel, "log-level", cfg.logLevel, "debug, info, warn, or error (env KBLOOM_LOG_LEVEL)")
	// ContinueOnError prints the usage line itself, so this error must not be printed again.
	if err := fs.Parse(args); err != nil {
		return config{}, exitError{code: 2, err: err}
	}

	// Visit lists only flags present on the command line, not defaults.
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if err := applyEnv(seen, &cfg); err != nil {
		return config{}, err
	}
	if cfg.maxClients <= 0 {
		return config{}, errors.New("max clients must be positive")
	}
	if cfg.readTimeout < 0 || cfg.writeTimeout < 0 {
		return config{}, errors.New("timeouts must not be negative")
	}
	switch strings.ToLower(cfg.logLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return config{}, fmt.Errorf("invalid log level %q", cfg.logLevel)
	}
	return cfg, nil
}

// applyEnv copies KBLOOM_* values into cfg for flags that were left unset.
func applyEnv(seen map[string]bool, cfg *config) error {
	if !seen["addr"] {
		if v := os.Getenv("KBLOOM_ADDR"); v != "" {
			cfg.addr = v
		}
	}
	if !seen["max-bits"] {
		n, err := envUint("KBLOOM_MAX_BITS", cfg.maxBits)
		if err != nil {
			return err
		}
		cfg.maxBits = n
	}
	if !seen["max-memory"] {
		n, err := envUint("KBLOOM_MAX_MEMORY", cfg.maxMemory)
		if err != nil {
			return err
		}
		cfg.maxMemory = n
	}
	if !seen["max-clients"] {
		n, err := envInt("KBLOOM_MAX_CLIENTS", cfg.maxClients)
		if err != nil {
			return err
		}
		cfg.maxClients = n
	}
	if !seen["timeout"] {
		d, err := envDuration("KBLOOM_TIMEOUT", cfg.readTimeout)
		if err != nil {
			return err
		}
		cfg.readTimeout = d
	}
	if !seen["write-timeout"] {
		d, err := envDuration("KBLOOM_WRITE_TIMEOUT", cfg.writeTimeout)
		if err != nil {
			return err
		}
		cfg.writeTimeout = d
	}
	if !seen["log-level"] {
		if v := os.Getenv("KBLOOM_LOG_LEVEL"); v != "" {
			cfg.logLevel = v
		}
	}
	return nil
}

// envUint parses key as a base-10 uint64. An unset variable returns fallback.
func envUint(key string, fallback uint64) (uint64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// envInt parses key as a base-10 int. An unset variable returns fallback.
func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// envDuration parses key with time.ParseDuration. An unset variable returns fallback.
func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// serve listens on cfg.addr and blocks until SIGINT or SIGTERM.
func serve(cfg config) error {
	// parseConfig already rejected unknown level names.
	var level slog.Level
	switch strings.ToLower(cfg.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	store := bloom.NewStore(bloom.StoreConfig{
		MaxBits:  cfg.maxBits,
		MaxBytes: cfg.maxMemory,
	})
	srv, err := server.New(store, server.Config{
		MaxConnections: cfg.maxClients,
		ReadTimeout:    cfg.readTimeout,
		WriteTimeout:   cfg.writeTimeout,
	}, logger)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.addr, err)
	}
	logger.Info("listening",
		"addr", ln.Addr().String(),
		"max_bits", cfg.maxBits,
		"max_memory", cfg.maxMemory,
		"max_clients", cfg.maxClients,
	)

	// Cancel the server context on shutdown so Accept unblocks and clients are closed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}
