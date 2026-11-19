//go:build windows

package persist

import "os"

// Windows does not support File.Sync on directory handles. The temporary file
// is flushed before replacement; cross-platform status is documented separately.
func syncDirectory(*os.File) error { return nil }
