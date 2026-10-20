package main

import (
	"fmt"
	"io"
	"net"
	"time"
)

// runHealth dials the configured listen address and expects a RESP PONG.
// Docker runs this as HEALTHCHECK inside the same container as the server.
func runHealth(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	addr, err := healthDialAddr(cfg.addr)
	if err != nil {
		return err
	}
	return ping(addr)
}

// healthDialAddr turns a listen address into a loopback dial address.
// A wildcard listen address has no host to connect to, so the probe uses loopback.
func healthDialAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// ping sends one RESP PING and requires the reply +PONG.
func ping(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}
	buf := make([]byte, len("+PONG\r\n"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != "+PONG\r\n" {
		return fmt.Errorf("health check reply %q", buf)
	}
	return nil
}
