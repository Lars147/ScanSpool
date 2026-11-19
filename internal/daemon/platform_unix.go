//go:build linux || darwin

package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func checkOwnedFile(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe service installation path %s: must be root-owned and not group/world writable", path)
	}
	return nil
}

func checkDataOwner(info os.FileInfo, uid int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid {
		return fmt.Errorf("data directory must already belong to receiver UID %d; no ownership changes are performed", uid)
	}
	return nil
}

func checkOwnedParents(path string) error {
	for {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("installation parent is not a real directory: %s", path)
		}
		if err := checkOwnedFile(path, info); err != nil {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func lockInstallation(path string) (io.Closer, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		err = checkOwnedFile(path, info)
	}
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("invalid installation lock")
	}
	if err == nil {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot acquire service installation lock: %w", err)
	}
	return f, nil
}
