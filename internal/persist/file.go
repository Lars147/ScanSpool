// Package persist commits private state files durably on the same filesystem.
package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CommitError reports whether rename succeeded before a durability failure.
// Callers must keep memory consistent with the committed file even on this error.
type CommitError struct{ Err error }

func (e *CommitError) Error() string {
	return "file replaced, durability not confirmed: " + e.Err.Error()
}
func (e *CommitError) Unwrap() error { return e.Err }
func Committed(err error) bool       { var e *CommitError; return errors.As(err, &e) }

type operations struct {
	syncFile func(*os.File) error
	rename   func(string, string) error
	syncDir  func(*os.File) error
}

func WriteFile(path string, data []byte) error {
	return writeFile(path, data, operations{(*os.File).Sync, os.Rename, syncDirectory})
}

func writeFile(path string, data []byte, ops operations) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := ops.syncFile(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ops.rename(f.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return &CommitError{err}
	}
	defer directory.Close()
	if err := ops.syncDir(directory); err != nil {
		return &CommitError{fmt.Errorf("sync directory: %w", err)}
	}
	return nil
}
