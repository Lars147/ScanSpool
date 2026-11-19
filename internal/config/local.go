package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// OpenLocalDirectory prepares and checks both lexical and resolved paths. The
// outbox may not exist yet during first-time setup, but its parent must exist.
func OpenLocalDirectory(directory, outbox string) (*os.Root, error) {
	if err := (Destination{Kind: KindLocal, LocalDirectory: directory}).Validate(); err != nil {
		return nil, err
	}
	if err := ValidateLocalDirectory(directory, outbox); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0777); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	resolvedOutbox, err := filepath.EvalSymlinks(outbox)
	if errors.Is(err, os.ErrNotExist) {
		// Do not mistake a dangling outbox symlink for an absent directory.
		if _, statErr := os.Lstat(outbox); !errors.Is(statErr, os.ErrNotExist) {
			return nil, err
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(outbox))
		if parentErr != nil {
			return nil, parentErr
		}
		resolvedOutbox = filepath.Join(parent, filepath.Base(outbox))
	} else if err != nil {
		return nil, err
	}
	if err := ValidateLocalDirectory(resolved, resolvedOutbox); err != nil {
		return nil, err
	}
	return os.OpenRoot(resolved)
}

// CheckLocalDirectory probes the operations required by local delivery using
// the receiver's actual permissions, including ACLs. No PDF is published.
func CheckLocalDirectory(directory, outbox string) error {
	if err := checkLocalDirectory(directory, outbox); err != nil {
		return &LocalDirectoryError{Directory: directory, Err: err}
	}
	return nil
}

func checkLocalDirectory(directory, outbox string) error {
	root, err := OpenLocalDirectory(directory, outbox)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := ".scanspool-check-" + hex.EncodeToString(random[:])
	temporary, linked := name+".partial", name+"-linked.partial"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	defer file.Close()
	if _, err := file.WriteString("ScanSpool output access check\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Link(temporary, linked); err != nil {
		return fmt.Errorf("check local publication (folder must support hard links): %w", err)
	}
	defer root.Remove(linked)
	if err := root.Remove(linked); err != nil {
		return err
	}
	if err := root.Remove(temporary); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return dir.Sync()
	}
	return nil
}
