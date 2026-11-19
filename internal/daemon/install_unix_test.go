//go:build linux || darwin

package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInstallWithPrivateAdministrativeUmask(t *testing.T) {
	f := newFixture(t)
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	f.install(t)
	for _, path := range []string{f.m.paths.Binary, f.m.paths.Unit, f.m.paths.Manifest} {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatal("installer-created directory prevents receiver access", path, info, err)
		}
	}
	info, err := os.Stat(f.dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("private receiver directory changed", info, err)
	}
}
