package output

import (
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
	"scanspool/internal/config"
)

// Upload publishes by rename only after the entire temporary file is uploaded.
// Both control and passive-data sockets are closed on cancellation.
func Upload(ctx context.Context, target config.FTP, name string, data io.Reader) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if path.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid upload name")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var mu sync.Mutex
	var sockets []net.Conn
	dial := func(network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		deadline, _ := ctx.Deadline()
		conn.SetDeadline(deadline)
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			conn.Close()
			return nil, ctx.Err()
		}
		sockets = append(sockets, conn)
		mu.Unlock()
		return conn, nil
	}
	closeSockets := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range sockets {
			conn.Close()
		}
	}
	stop := context.AfterFunc(ctx, closeSockets)
	defer func() { stop(); closeSockets() }()
	host := target.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "21")
	}
	conn, err := ftp.Dial(host, ftp.DialWithDialFunc(dial), ftp.DialWithContext(ctx), ftp.DialWithTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("FTP connect: %w", err)
	}
	defer conn.Quit()
	if err := conn.Login(target.User, target.Password); err != nil {
		return fmt.Errorf("FTP login: %w", err)
	}
	if err := conn.ChangeDir(target.Directory); err != nil {
		return fmt.Errorf("FTP directory: %w", err)
	}
	temporary := "." + name + ".partial"
	if err := conn.Stor(temporary, data); err != nil {
		return fmt.Errorf("FTP upload (local PDF retained): %w", err)
	}
	if err := conn.Rename(temporary, name); err != nil {
		return fmt.Errorf("FTP publish (local PDF retained): %w", err)
	}
	return nil
}
