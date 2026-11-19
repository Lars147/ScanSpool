//go:build !windows

package persist

import "os"

func syncDirectory(file *os.File) error { return file.Sync() }
