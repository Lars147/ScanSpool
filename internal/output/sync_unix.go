//go:build !windows

package output

import "os"

func syncOutputDirectory(file *os.File) error { return file.Sync() }
