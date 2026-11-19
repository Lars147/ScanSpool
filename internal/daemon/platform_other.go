//go:build !linux

package daemon

import "fmt"

func checkPlatform() error {
	return fmt.Errorf("service commands support Linux with systemd; use run with your platform's service manager")
}
