//go:build linux || darwin

package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireLease prevents two service processes from sharing a host identity and
// transaction journal. The OS releases the advisory lock after a crash.
func AcquireLease(dir string) (io.Closer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another ScanSpool process is using this data directory: %w", err)
	}
	return file, nil
}
