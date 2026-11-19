package output

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"scanspool/internal/config"
	"scanspool/internal/persist"
)

type localOperations struct {
	syncFile func(*os.File) error
	publish  func(*os.Root, string, string) error
	syncDir  func(*os.File) error
}

// Local publishes a complete PDF without replacing an existing file. The
// temporary file lives on the destination filesystem, independently of outbox.
func Local(ctx context.Context, dataDir, directory, name string, data io.Reader) error {
	err := writeLocal(ctx, dataDir, directory, name, data, localOperations{
		syncFile: (*os.File).Sync,
		publish:  (*os.Root).Link,
		syncDir:  syncOutputDirectory,
	})
	if err != nil {
		return &config.LocalDirectoryError{Directory: directory, Err: err}
	}
	return nil
}

func writeLocal(ctx context.Context, dataDir, directory, name string, data io.Reader, ops localOperations) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validJobName(name) {
		return fmt.Errorf("invalid local output name")
	}
	root, err := config.OpenLocalDirectory(directory, queueDir(dataDir))
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
	temporary := ".scan-" + hex.EncodeToString(random[:]) + ".partial"
	f, err := root.OpenFile(temporary, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	defer f.Close()
	if _, err := io.Copy(f, contextReader{ctx, data}); err != nil {
		return err
	}
	if err := ops.syncFile(f); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link publishes atomically with no replacement; rename would overwrite a
	// conflicting file on Unix. A consumer may remove the PDF immediately.
	if err := ops.publish(root, temporary, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish local PDF (destination must support hard links): %w", err)
		}
		if err := verifyExisting(ctx, root, temporary, name, ops.syncFile); err != nil {
			return err
		}
	}
	if err := root.Remove(temporary); err != nil {
		return &persist.CommitError{Err: err}
	}
	if err := ops.syncDir(dir); err != nil {
		return &persist.CommitError{Err: fmt.Errorf("sync local output directory: %w", err)}
	}
	return nil
}

func verifyExisting(ctx context.Context, root *os.Root, temporary, name string, syncFile func(*os.File) error) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("local output already exists and is not a regular file: %s", name)
	}
	existing, err := root.Open(name)
	if err != nil {
		return err
	}
	defer existing.Close()
	staged, err := root.Open(temporary)
	if err != nil {
		return err
	}
	defer staged.Close()
	left, right := sha256.New(), sha256.New()
	if _, err := io.Copy(left, contextReader{ctx, existing}); err != nil {
		return err
	}
	if _, err := io.Copy(right, contextReader{ctx, staged}); err != nil {
		return err
	}
	if !bytes.Equal(left.Sum(nil), right.Sum(nil)) {
		return fmt.Errorf("local output already exists with different content: %s", name)
	}
	return syncFile(existing)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
