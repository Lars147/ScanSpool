package vens

import (
	"context"
	"net"
	"time"
)

type contextConnection struct {
	net.Conn
	stop func() bool
}

func (c *contextConnection) Close() error { c.stop(); return c.Conn.Close() }

// dialScanner makes shutdown/request cancellation interrupt in-flight I/O too.
func dialScanner(ctx context.Context, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return &contextConnection{Conn: conn, stop: context.AfterFunc(ctx, func() { conn.Close() })}, nil
}
