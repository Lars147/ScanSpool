//go:build windows

package daemon

import (
	"fmt"
	"io"
	"os"
)

func checkOwnedFile(string, os.FileInfo) error { return nil }
func checkDataOwner(os.FileInfo, int) error    { return fmt.Errorf("systemd is not supported on Windows") }
func checkOwnedParents(string) error           { return nil }
func lockInstallation(string) (io.Closer, error) {
	return nil, fmt.Errorf("systemd is not supported on Windows")
}
