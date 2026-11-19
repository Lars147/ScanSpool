//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func checkPlatform() error {
	name, err := os.ReadFile("/proc/1/comm")
	if err != nil || strings.TrimSpace(string(name)) != "systemd" {
		return fmt.Errorf("service commands require Linux running systemd; use run with your service manager")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl is required: %w", err)
	}
	return nil
}
