package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"scanspool/internal/vens"
)

// ButtonListener listens for scanner button press events on UDP:55265.
type ButtonListener struct {
	conn     *net.UDPConn
	callback func()
	done     chan struct{}
	host     net.IP
	cancel   context.CancelFunc
}

// NewButtonListener creates a ButtonListener that calls callback on button press.
func NewButtonListener(host string, callback func()) *ButtonListener {
	return &ButtonListener{callback: callback, host: net.ParseIP(host)}
}

// Start begins listening for button press events. Blocks until ctx is cancelled.
func (b *ButtonListener) Start(ctx context.Context) error {
	addr := &net.UDPAddr{Port: int(vens.ClientNotifyPort)}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("listen UDP:%d: %w", vens.ClientNotifyPort, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.conn = conn
	b.done = make(chan struct{})
	slog.Info("button listener started", "port", vens.ClientNotifyPort)

	go b.loop(ctx)
	return nil
}

// Stop closes the listener and waits for the goroutine to exit.
func (b *ButtonListener) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.conn != nil {
		b.conn.Close()
	}
	if b.done != nil {
		<-b.done
	}
}

func (b *ButtonListener) loop(ctx context.Context) {
	defer close(b.done)
	buf := make([]byte, 256)
	var lastPress time.Time
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, remote, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("button listener read error", "err", err)
			continue
		}

		eventType, eventData, err := vens.ParseEventNotification(buf[:n])
		if err != nil {
			slog.Debug("ignoring non-VENS packet", "remote", remote, "err", err)
			continue
		}

		if !remote.IP.Equal(b.host) || eventType != 1 {
			continue
		}
		if time.Since(lastPress) < 2*time.Second {
			continue
		}
		lastPress = time.Now()
		slog.Info("scanner button pressed", "type", eventType, "data", eventData, "remote", remote)
		if b.callback != nil {
			b.callback()
		}
	}
}
