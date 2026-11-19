//go:build !linux && !darwin && !windows

package setup

import (
	"fmt"
	"io"
)

func AcquireLease(string) (io.Closer, error) {
	return nil, fmt.Errorf("service data-directory locking is currently supported on Linux and macOS")
}
